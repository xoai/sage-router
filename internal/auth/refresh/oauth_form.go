package refresh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"sage-router/internal/auth"
	"sage-router/internal/auth/providers"
)

// formTokenResponse is the canonical shape returned by Anthropic / Gemini
// (RFC 6749 §5.1 success / §5.2 error). OpenAI's auth.openai.com wraps
// errors in the chat-completions error envelope — captured in
// WrappedError below. The struct accommodates BOTH shapes; one or the
// other populates depending on the provider.
type formTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`

	// RFC 6749 §5.2 top-level error fields (Anthropic / Gemini).
	Error            string `json:"-"`
	ErrorDescription string `json:"error_description,omitempty"`

	// rawError is unmarshaled from the `error` JSON field, which can be
	// EITHER a string (RFC 6749) OR an object (OpenAI's chat-completions
	// envelope: {"error":{"message":..,"code":"refresh_token_expired"}}).
	// Discrimination happens in the unmarshal helper below.
	RawError json.RawMessage `json:"error,omitempty"`

	// WrappedError carries the parsed-from-OpenAI nested error fields.
	// Populated by post-unmarshal normalization when RawError parses as
	// an object. Codex-rs defines these error codes at
	// codex-rs/login/src/auth/manager.rs (RefreshTokenFailedReason enum).
	WrappedError struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"` // refresh_token_expired / refresh_token_reused / refresh_token_invalidated / ...
	} `json:"-"`
}

// normalizeError extracts the error code from EITHER RFC 6749 top-level
// string OR OpenAI's wrapped envelope object. Called after JSON unmarshal.
func (r *formTokenResponse) normalizeError() {
	if len(r.RawError) == 0 {
		return
	}
	// Try string (RFC 6749 standard).
	var asString string
	if err := json.Unmarshal(r.RawError, &asString); err == nil && asString != "" {
		r.Error = asString
		return
	}
	// Try object (OpenAI's chat-completions envelope).
	if err := json.Unmarshal(r.RawError, &r.WrappedError); err == nil {
		// Map OpenAI's code field to a canonical Error string so downstream
		// invalid_grant / invalid_token discrimination works uniformly.
		switch r.WrappedError.Code {
		case "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
			r.Error = "invalid_grant"
		default:
			r.Error = r.WrappedError.Code
		}
		if r.ErrorDescription == "" {
			r.ErrorDescription = r.WrappedError.Message
		}
	}
}

// refreshViaOAuthForm is the shared implementation for OpenAI / Anthropic /
// Gemini. They follow RFC 6749 §6 (refresh_token grant); body shape is
// per-provider via the `RefreshTokenFormat` field on ProviderConfig
// (defaults to form-encoded RFC default; openai uses JSON per codex-rs
// canonical reference + memory `bf614108` public-client parity rule).
//
// The result Credential preserves Provider, AccountID, ExtraData from
// the input and gets fresh tokens + expires_at. Previously ExchangedToken
// was preserved too for the RFC 8693 chain, but that whole machinery was
// removed at cycle 20260517-provider-auth-variants M2.6.2 (the chain was
// wrong-path per memory `f32bbc73`) — CodexSubscriptionExecutor uses the
// PKCE access_token directly against chatgpt.com/backend-api/codex/responses.
//
// Providers MAY rotate the refresh_token (returning a new one), in which
// case we replace it; or they may omit it, in which case we keep the
// old one. Returns the parsed id_token as a second return value so
// callers that need it (currently none — kept for API stability) can
// access it.
func refreshViaOAuthForm(ctx context.Context, cred *auth.Credential, tokenURL, clientID string) (*auth.Credential, string, error) {
	// Body shape selected per the provider's RefreshTokenFormat (or, if
	// unset, falls back to TokenRequestFormat for providers whose Refresh
	// and Exchange phases share the same wire shape — currently Anthropic).
	// OpenAI's codex CLI splits the shapes: form-encoded for the initial
	// PKCE Exchange, JSON for the refresh_token grant (per codex-rs
	// `login/src/auth/manager.rs`). Memory `bf614108` public-client OAuth
	// parity rule applies.
	cfg := providers.Providers[cred.Provider]
	bodyFormat := cfg.RefreshTokenFormat
	if bodyFormat == "" {
		bodyFormat = cfg.TokenRequestFormat
	}

	var reqBody io.Reader
	var contentType string
	switch bodyFormat {
	case "json":
		payload := map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": cred.RefreshToken,
		}
		if clientID != "" {
			payload["client_id"] = clientID
		}
		bs, err := json.Marshal(payload)
		if err != nil {
			return nil, "", fmt.Errorf("refresh %s: marshal json body: %w", cred.Provider, err)
		}
		reqBody = bytes.NewReader(bs)
		contentType = "application/json"
	default:
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", cred.RefreshToken)
		if clientID != "" {
			form.Set("client_id", clientID)
		}
		reqBody = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("refresh %s: build request: %w", cred.Provider, err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("refresh %s: %w", cred.Provider, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// Parse JSON eagerly — providers return JSON for both success and 4xx
	// error bodies. If parse fails we fall back to a generic error using
	// the raw status code (truncated body).
	var parsed formTokenResponse
	parseErr := json.Unmarshal(body, &parsed)
	parsed.normalizeError() // discriminate RFC-string vs OpenAI-envelope shape

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// invalid_grant means our refresh_token is dead — distinct sentinel
		// so callers can auto-disable rather than retry.
		if parsed.Error == "invalid_grant" || parsed.Error == "invalid_token" {
			return nil, "", fmt.Errorf("refresh %s (%s): %w",
				cred.Provider, parsed.ErrorDescription, ErrRefreshTokenRevoked)
		}
		msg := parsed.Error
		if msg == "" {
			msg = truncate(string(body), 500)
		}
		return nil, "", fmt.Errorf("refresh %s: status %d: %s", cred.Provider, resp.StatusCode, msg)
	}

	if parseErr != nil {
		return nil, "", fmt.Errorf("refresh %s: parse response: %w (body: %s)",
			cred.Provider, parseErr, truncate(string(body), 200))
	}
	if parsed.AccessToken == "" {
		return nil, "", errors.New("refresh: provider returned 200 but no access_token")
	}

	// Preserve the old refresh_token when the provider doesn't rotate.
	newRefreshToken := parsed.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = cred.RefreshToken
	}

	return &auth.Credential{
		Provider:     cred.Provider,
		ConnectionID: cred.ConnectionID,
		AccessToken:  parsed.AccessToken,
		RefreshToken: newRefreshToken,
		ExpiresAt:    computeExpiresAt(parsed.ExpiresIn),
		AccountID:    cred.AccountID,
		ExtraData:    cred.ExtraData,
	}, parsed.IDToken, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}
