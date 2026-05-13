package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	claudeDefaultBaseURL    = "https://api.anthropic.com"
	claudeDefaultEndpoint   = "/v1/messages"
	claudeAnthropicVersion  = "2023-06-01"
)

// ClaudeExecutor handles requests to the Anthropic Claude API.
type ClaudeExecutor struct {
	baseURL string
	pool    *ClientPool
}

// NewClaudeExecutor creates a ClaudeExecutor.
// If baseURL is empty, the default Anthropic API URL is used.
func NewClaudeExecutor(baseURL string, pool *ClientPool) *ClaudeExecutor {
	if baseURL == "" {
		baseURL = claudeDefaultBaseURL
	}
	return &ClaudeExecutor{
		baseURL: strings.TrimRight(baseURL, "/"),
		pool:    pool,
	}
}

// Provider implements Executor.
func (e *ClaudeExecutor) Provider() string {
	return "anthropic"
}

// Execute implements Executor. It builds the Anthropic-specific request with
// the required headers and sends it upstream.
func (e *ClaudeExecutor) Execute(ctx context.Context, req *ExecuteRequest) (*Result, error) {
	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = claudeDefaultEndpoint
	}
	targetURL := e.baseURL + endpoint

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("claude executor: build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", claudeAnthropicVersion)

	// Apply authentication. Claude uses x-api-key for API key auth and
	// Authorization: Bearer for subscription tokens. Unknown AuthType
	// values return a loud error rather than a silent best-effort guess —
	// store-side NormalizeAuthType canonicalizes the value before it ever
	// reaches the executor (auth/authtype.go + store/sqlite.go scan).
	if req.Credentials != nil {
		switch req.Credentials.AuthType {
		case "apikey":
			httpReq.Header.Set("x-api-key", req.Credentials.APIKey)
		case "subscription":
			httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.AccessToken)
			for k, v := range req.Credentials.ExtraHeaders {
				httpReq.Header.Set(k, v)
			}
		case "none":
			// No auth header needed.
		default:
			return nil, fmt.Errorf("claude executor: unsupported auth_type %q for connection %s",
				req.Credentials.AuthType, req.Credentials.ConnectionID)
		}
	}

	client := e.pool.Get(req.ProxyURL)

	start := time.Now()
	resp, err := client.Do(httpReq)
	latency := time.Since(start)

	if err != nil {
		return nil, fmt.Errorf("claude executor: do request: %w", err)
	}

	return &Result{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       resp.Body,
		URL:        targetURL,
		Latency:    latency,
	}, nil
}

// OverrideCapabilities implements CapabilityOverrider (Models Discovery M3.5).
//
// Anthropic's /v1/models endpoint does not return capability flags, so
// discovery-sourced rows land in catalog_models with zero capabilities.
// Without this override the smart-router would treat thinking-capable
// variants as non-thinking. The override flips SupportsThinking=true
// for any Claude variant known to support extended thinking; other
// flags pass through (additive-only semantic — the override never
// REMOVES a capability the catalog asserted).
//
// Known thinking-capable families (as of Claude 4.6 / Haiku 4.5):
//   - claude-3-7-* (first thinking-capable family)
//   - claude-{opus,sonnet,haiku}-N-* where N ≥ 4 (4.x and forward)
//
// The pattern is intentionally inclusive — future Anthropic releases
// in the 4.x/5.x/6.x lineages inherit the flag without a code change.
func (e *ClaudeExecutor) OverrideCapabilities(model string, base Capabilities) Capabilities {
	if claudeSupportsThinking(model) {
		base.SupportsThinking = true
	}
	return base
}

// claudeSupportsThinking returns true for Claude model IDs known to
// support extended thinking. See OverrideCapabilities for the
// matched lineages.
func claudeSupportsThinking(model string) bool {
	if strings.HasPrefix(model, "claude-3-7-") {
		return true
	}
	for _, family := range []string{"claude-opus-", "claude-sonnet-", "claude-haiku-"} {
		rest, ok := strings.CutPrefix(model, family)
		if !ok || rest == "" {
			continue
		}
		// First char of `rest` is the major version digit. Treat
		// '4'..'9' as thinking-capable; older majors ('0'..'3') and
		// non-digits as non-thinking. The boundary at '4' matches the
		// Anthropic versioning convention where extended thinking
		// became standard in the 4.x family.
		if rest[0] >= '4' && rest[0] <= '9' {
			return true
		}
	}
	return false
}
