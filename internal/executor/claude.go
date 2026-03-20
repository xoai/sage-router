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
	// Authorization: Bearer for OAuth tokens.
	if req.Credentials != nil {
		switch req.Credentials.AuthType {
		case "apikey":
			httpReq.Header.Set("x-api-key", req.Credentials.APIKey)
		case "oauth":
			httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.AccessToken)
		case "none":
			// No auth header needed.
		default:
			// Best-effort fallback.
			if req.Credentials.APIKey != "" {
				httpReq.Header.Set("x-api-key", req.Credentials.APIKey)
			} else if req.Credentials.AccessToken != "" {
				httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.AccessToken)
			}
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
