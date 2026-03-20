package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultExecutor is a generic, OpenAI-compatible provider executor.
// It works for any provider whose API follows the OpenAI chat completions
// convention (OpenAI, OpenRouter, Ollama, and similar).
type DefaultExecutor struct {
	provider string
	baseURL  string
	pool     *ClientPool
}

// NewDefaultExecutor creates a DefaultExecutor for the named provider.
// baseURL is the provider's API root (e.g. "https://api.openai.com/v1").
func NewDefaultExecutor(provider, baseURL string, pool *ClientPool) *DefaultExecutor {
	return &DefaultExecutor{
		provider: provider,
		baseURL:  strings.TrimRight(baseURL, "/"),
		pool:     pool,
	}
}

// Provider implements Executor.
func (e *DefaultExecutor) Provider() string {
	return e.provider
}

// Execute implements Executor. It sends the request body to the provider's
// /chat/completions endpoint (or a custom endpoint when req.Endpoint is set)
// and returns the raw upstream response.
func (e *DefaultExecutor) Execute(ctx context.Context, req *ExecuteRequest) (*Result, error) {
	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = "/chat/completions"
	}
	targetURL := e.baseURL + endpoint

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("default executor: build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// Apply authentication.
	if req.Credentials != nil {
		switch req.Credentials.AuthType {
		case "apikey":
			httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.APIKey)
		case "oauth":
			httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.AccessToken)
		case "none":
			// No auth header needed.
		default:
			// Fall back to API key if present, otherwise access token.
			if req.Credentials.APIKey != "" {
				httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.APIKey)
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
		return nil, fmt.Errorf("default executor: do request: %w", err)
	}

	return &Result{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       resp.Body,
		URL:        targetURL,
		Latency:    latency,
	}, nil
}
