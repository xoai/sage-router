package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/providers"
)

// expiryBuffer is subtracted from the upstream-reported expires_in so the
// token is considered "expiring" 5 minutes before its actual expiry. This
// matches sage-wiki's R2 buffer (and pi-ai's practice).
const expiryBuffer = 5 * time.Minute

// Flow is one in-flight PKCE OAuth handshake. NewFlow seeds it with random
// verifier/challenge/state; the caller drives the rest through AuthorizeURL
// (to produce the browser-visible URL) and Exchange (to swap the callback
// code for tokens). DashboardSID is populated only for flows initiated
// from the dashboard; CLI flows leave it empty.
type Flow struct {
	Provider     string
	ConnName     string
	Verifier     string
	Challenge    string
	State        string
	CreatedAt    time.Time
	DashboardSID string
	Priority     int

	// Origin is the dashboard URL the user started the flow from
	// (e.g., "https://sage-router.example.com"). Populated by the
	// /api/auth/oauth/start handler from the request's Origin header.
	// Empty for CLI-initiated flows; the bridge's BridgeHandler uses
	// it to redirect back to the dashboard after callback.
	Origin string

	// tokenURL is overridable by tests. Production code reads it from the
	// provider registry; tests point it at httptest.Server.
	tokenURL string
}

// TokenResponse is the canonical shape the provider's token endpoint
// returns. Different providers may add fields; the common subset is
// access_token + refresh_token + expires_in.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope,omitempty"`

	// ExchangedToken field removed in cycle 20260517-provider-auth-variants
	// M2.6.2 — RFC 8693 exchange chain was wrong-path (memory `f32bbc73`).
}

// SetTokenURLForTest overrides the URL Exchange POSTs to. Test-only —
// production code reads tokenURL from the provider registry at NewFlow
// time. The ugly suffix is deliberate so reviewers notice if it leaks
// into a non-test path.
func (f *Flow) SetTokenURLForTest(url string) {
	f.tokenURL = url
}

// NewFlow constructs a Flow for the named provider (canonical ID — caller
// should pass providers.Resolve's output). connName is the human-readable
// label for the resulting Connection row.
func NewFlow(provider, connName string) (*Flow, error) {
	cfg, ok := providers.Providers[provider]
	if !ok {
		return nil, fmt.Errorf("oauth: unknown provider %q", provider)
	}
	if cfg.FlowType != providers.FlowPKCE {
		return nil, fmt.Errorf("oauth: provider %q does not support PKCE flow (FlowType=%s)", provider, cfg.FlowType)
	}

	verifier, err := GenerateVerifier()
	if err != nil {
		return nil, fmt.Errorf("oauth: generate verifier: %w", err)
	}
	state, err := GenerateState()
	if err != nil {
		return nil, fmt.Errorf("oauth: generate state: %w", err)
	}

	return &Flow{
		Provider:  provider,
		ConnName:  connName,
		Verifier:  verifier,
		Challenge: GenerateChallenge(verifier),
		State:     state,
		CreatedAt: time.Now(),
		tokenURL:  cfg.TokenURL,
	}, nil
}

// AuthorizeURL builds the browser-visible URL that initiates the OAuth flow.
// The redirectURI must match a value the provider has registered against
// the client_id — for sage-router that's the localhost bridge URLs.
func (f *Flow) AuthorizeURL(redirectURI string) string {
	cfg := providers.Providers[f.Provider]
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", f.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", f.State)
	if len(cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	for k, v := range cfg.ExtraAuthParams {
		q.Set(k, v)
	}
	return cfg.AuthorizeURL + "?" + q.Encode()
}

// Exchange swaps the authorization code received at the callback for tokens.
// Body shape is selected by the provider's TokenRequestFormat:
//   - "" / "form" (RFC 6749 §4.1.3 default) — application/x-www-form-urlencoded
//   - "json" — application/json (Anthropic's platform.claude.com)
//
// The JSON shape is verified byte-for-byte against the working
// opencode-anthropic-auth plugin (src/auth.ts::exchangeCode) and pinned
// by TestFlow_Exchange_AnthropicSendsJSON. See M0.8 of cycle
// 20260517-provider-auth-variants for the diagnostic that drove this split.
func (f *Flow) Exchange(ctx context.Context, code, redirectURI string) (*TokenResponse, error) {
	cfg := providers.Providers[f.Provider]

	var reqBody io.Reader
	var contentType string
	switch cfg.TokenRequestFormat {
	case "json":
		payload := map[string]string{
			"grant_type":    "authorization_code",
			"code":          code,
			"state":         f.State,
			"client_id":     cfg.ClientID,
			"redirect_uri":  redirectURI,
			"code_verifier": f.Verifier,
		}
		bs, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("oauth exchange: marshal json body: %w", err)
		}
		reqBody = bytes.NewReader(bs)
		contentType = "application/json"
	default:
		form := url.Values{}
		form.Set("grant_type", "authorization_code")
		form.Set("code", code)
		form.Set("client_id", cfg.ClientID)
		form.Set("code_verifier", f.Verifier)
		form.Set("redirect_uri", redirectURI)
		reqBody = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tokenURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth exchange: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Truncate huge error bodies and avoid leaking the verifier/code in logs.
		msg := string(body)
		if len(msg) > 500 {
			msg = msg[:500] + "…(truncated)"
		}
		return nil, fmt.Errorf("oauth exchange: provider returned %d: %s", resp.StatusCode, msg)
	}

	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("oauth exchange: parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("oauth exchange: provider returned 200 but no access_token in body")
	}

	// Cycle 20260517-provider-auth-variants M2.6.2: RFC 8693 token-exchange
	// auto-chain REMOVED. The predecessor cycle's exchange path targeted
	// api.openai.com/v1/responses (unreachable for ChatGPT subscribers per
	// memory `f32bbc73`). The variant abstraction routes (openai, subscription)
	// to CodexSubscriptionExecutor which uses the PKCE access_token directly
	// against chatgpt.com/backend-api/codex/responses — no exchange step.
	return &tr, nil
}

// ToCredential builds an auth.Credential from a TokenResponse. expires_at
// is computed with the 5-minute safety buffer (matches sage-wiki R2). The
// AccountID claim is extracted from id_token when the provider registry
// names a JWT claim path; otherwise left empty. ExchangedToken plumbing
// REMOVED in cycle 20260517-provider-auth-variants M2.6.2.
func (f *Flow) ToCredential(resp *TokenResponse) *auth.Credential {
	cfg := providers.Providers[f.Provider]
	cred := &auth.Credential{
		Provider:     f.Provider,
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(resp.ExpiresIn)*time.Second - expiryBuffer),
	}
	if cfg.AccountIDClaim != "" && resp.IDToken != "" {
		if id, err := ExtractIDTokenClaim(resp.IDToken, cfg.AccountIDClaim); err == nil {
			cred.AccountID = id
		}
		// On error we silently leave AccountID empty — the credential still
		// works without it; some flows don't issue an id_token at all.
	}
	return cred
}
