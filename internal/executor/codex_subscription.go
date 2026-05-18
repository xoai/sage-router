package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	openairesp "sage-router/internal/translate/openai-responses"
	"sage-router/pkg/canonical"
)

// CodexSubscriptionExecutor is the upstream-dispatch impl for the
// (openai, subscription) variant. It targets the ChatGPT-subscription
// backend at chatgpt.com/backend-api/codex/responses — NOT
// api.openai.com/v1/responses (the predecessor cycle's wrong-path target;
// see memory `f32bbc73`).
//
// The PKCE access_token from the codex auth.json (~/.codex/auth.json
// tokens.access_token) is used DIRECTLY as `Authorization: Bearer`. No
// RFC 8693 token-exchange step (the predecessor cycle's
// ExchangeForAPIKey path was wrong-path and is being ripped at M2.6).
//
// Per decision-codex-subscription-contract.md (M0-validated 2026-05-17):
//   - 5 required headers (Content-Type, Authorization, chatgpt-account-id,
//     OpenAI-Beta, originator)
//   - `chatgpt-account-id` MUST be present — request fails with explicit
//     error before HTTP if missing from Credentials.ExtraHeaders.
//   - `store: false` MUST be in the body — the translator
//     (openai-responses.FromCanonical) emits it; this executor doesn't
//     mutate the body.
//
// Implements: Executor, Formatted, AuthErrorParser, PreflightChecker.
// Does NOT implement OAuthIdentified (Codex backend doesn't require
// identity-prepend the way claude.ai does for claude-max).
type CodexSubscriptionExecutor struct {
	baseURL string
	pool    *ClientPool
}

const (
	codexSubscriptionDefaultBaseURL = "https://chatgpt.com/backend-api"
	codexSubscriptionEndpoint       = "/codex/responses"
	codexSubscriptionBetaHeader     = "responses=experimental"
	codexSubscriptionOriginator     = "codex_cli_rs"
)

// NewCodexSubscriptionExecutor constructs a CodexSubscriptionExecutor
// targeting the production backend. Tests use newCodexSubscriptionExecutorForTest
// to override the base URL.
func NewCodexSubscriptionExecutor(pool *ClientPool) *CodexSubscriptionExecutor {
	return &CodexSubscriptionExecutor{
		baseURL: codexSubscriptionDefaultBaseURL,
		pool:    pool,
	}
}

// newCodexSubscriptionExecutorForTest is the test-only constructor that
// overrides the base URL — used to point at httptest.Server. The "ForTest"
// suffix is deliberate so reviewers notice if it leaks into production
// code paths (mirrors Flow.SetTokenURLForTest discipline in oauth/flow.go).
func newCodexSubscriptionExecutorForTest(baseURL string, pool *ClientPool) *CodexSubscriptionExecutor {
	return &CodexSubscriptionExecutor{
		baseURL: strings.TrimRight(baseURL, "/"),
		pool:    pool,
	}
}

// Provider implements Executor. The codex backend is OpenAI's
// subscription surface, so the canonical provider ID is "openai" —
// matches smart-router metadata + catalog dispatch conventions.
func (e *CodexSubscriptionExecutor) Provider() string {
	return "openai"
}

// Format implements Formatted. CodexSubscriptionExecutor's translator
// emits the OpenAI Responses API body shape (FormatResponses), not
// chat-completions (FormatOpenAI). Replaces the per-call branch in
// routes_v1.go::resolveTargetFormat that the M5.2 lift will delete.
func (e *CodexSubscriptionExecutor) Format() canonical.Format {
	return canonical.FormatResponses
}

// Execute sends the request to chatgpt.com/backend-api/codex/responses.
// Body is built by the openai-responses translator (must include
// `model`, `instructions` non-empty, `stream: true`, `store: false`,
// `input` per the live-validated contract — M0.8 evidence at
// .sage/work/20260517-provider-auth-variants/m0-baseline.md).
//
// The executor's only responsibility is wire-shaping: build the HTTP
// request with the right URL + headers, send, return the raw Result.
// Body shape is the translator's job.
func (e *CodexSubscriptionExecutor) Execute(ctx context.Context, req *ExecuteRequest) (*Result, error) {
	// Preflight: missing AccessToken → fail before HTTP. The PKCE
	// access_token IS the bearer; no exchange step.
	if req.Credentials == nil || req.Credentials.AccessToken == "" {
		return nil, fmt.Errorf("codex executor: missing AccessToken for connection %s", connID(req.Credentials))
	}
	// Preflight: missing ChatGPT-Account-ID → fail before HTTP. The
	// backend rejects without it; failing early saves the round trip.
	// Look up under both the codebase convention ("ChatGPT-Account-ID",
	// used by Credential.ExtraHeaders + existing tests) and the lowercase
	// wire form ("chatgpt-account-id", emitted by opencode plugin).
	accountID := ""
	if req.Credentials.ExtraHeaders != nil {
		if v := req.Credentials.ExtraHeaders["ChatGPT-Account-ID"]; v != "" {
			accountID = v
		} else if v := req.Credentials.ExtraHeaders["chatgpt-account-id"]; v != "" {
			accountID = v
		}
	}
	if accountID == "" {
		return nil, fmt.Errorf("codex executor: missing chatgpt-account-id header for connection %s", connID(req.Credentials))
	}

	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = codexSubscriptionEndpoint
	}
	targetURL := e.baseURL + endpoint

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("codex executor: build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.AccessToken)
	httpReq.Header.Set("OpenAI-Beta", codexSubscriptionBetaHeader)
	httpReq.Header.Set("originator", codexSubscriptionOriginator)
	// chatgpt-account-id verified non-empty above.
	httpReq.Header.Set("chatgpt-account-id", accountID)

	// Forward any other client-provided ExtraHeaders. Skip chatgpt-account-id
	// (already set) to keep a single source of truth for that header value.
	for k, v := range req.Credentials.ExtraHeaders {
		if strings.EqualFold(k, "chatgpt-account-id") {
			continue
		}
		httpReq.Header.Set(k, v)
	}

	client := e.pool.Get(req.ProxyURL)

	start := time.Now()
	resp, err := client.Do(httpReq)
	latency := time.Since(start)

	if err != nil {
		return nil, fmt.Errorf("codex executor: do request: %w", err)
	}

	return &Result{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       resp.Body,
		URL:        targetURL,
		Latency:    latency,
	}, nil
}

// ParseAuthError implements AuthErrorParser. Sniffs 401 responses for the
// canonical tier-error body emitted by api.openai.com when an account
// lacks api.responses.write scope. The chatgpt.com/backend-api endpoint
// (M0-validated) didn't surface this exact body in our live tests, but
// the sniff stays defensive — future account-tier limits may surface
// here, and mapping them to ErrTierMissingScopes lets the route handler
// emit a friendly LastError on the connection for the dashboard.
func (e *CodexSubscriptionExecutor) ParseAuthError(statusCode int, body []byte) error {
	if statusCode == 401 && bytes.Contains(body, []byte("Missing scopes: api.responses.write")) {
		return openairesp.ErrTierMissingScopes
	}
	return nil
}

// PreflightCredentials implements PreflightChecker. Empty AccessToken →
// ErrTierMissingScopes. The route handler maps the typed error to
// `connection.SetLastError(...) + MarkAuthExpired()` for fast user
// re-auth surfacing without burning a roundtrip.
//
// "Empty AccessToken" means the connection was created via auto-detect
// but the codex auth.json wasn't readable, OR the PKCE flow stored a
// row before completing (defensive). The ADR's pre-flight at M2 lifts
// this from routes_v1.go:1326's inline check.
func (e *CodexSubscriptionExecutor) PreflightCredentials(creds *Credentials) error {
	if creds == nil || creds.AccessToken == "" {
		return openairesp.ErrTierMissingScopes
	}
	return nil
}

// connID is a small helper for error messages — surfaces the connection
// ID from Credentials when present so logs/errors are traceable.
// Tests with nil Credentials get the literal "<nil>".
func connID(c *Credentials) string {
	if c == nil {
		return "<nil>"
	}
	return c.ConnectionID
}
