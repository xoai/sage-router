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
	geminiDefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"
)

// GeminiExecutor handles requests to the Google Gemini (Generative Language) API.
// Unlike most providers, Gemini places the API key in a URL query parameter
// rather than a header, and encodes the model name directly in the URL path.
type GeminiExecutor struct {
	baseURL string
	pool    *ClientPool
}

// NewGeminiExecutor creates a GeminiExecutor.
// If baseURL is empty, the default Gemini API URL is used.
func NewGeminiExecutor(baseURL string, pool *ClientPool) *GeminiExecutor {
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
	}
	return &GeminiExecutor{
		baseURL: strings.TrimRight(baseURL, "/"),
		pool:    pool,
	}
}

// Provider implements Executor.
func (e *GeminiExecutor) Provider() string {
	return "gemini"
}

// Execute implements Executor. It builds the Gemini-specific URL with the
// model name and API key baked in, then sends the request upstream.
func (e *GeminiExecutor) Execute(ctx context.Context, req *ExecuteRequest) (*Result, error) {
	targetURL, err := e.buildURL(req)
	if err != nil {
		return nil, fmt.Errorf("gemini executor: build url: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("gemini executor: build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// For OAuth-based auth, set the Authorization header (API-key auth is
	// handled via the URL query parameter in buildURL).
	if req.Credentials != nil && req.Credentials.AuthType == "oauth" {
		httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.AccessToken)
	}

	client := e.pool.Get(req.ProxyURL)

	start := time.Now()
	resp, err := client.Do(httpReq)
	latency := time.Since(start)

	if err != nil {
		return nil, fmt.Errorf("gemini executor: do request: %w", err)
	}

	return &Result{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       resp.Body,
		URL:        targetURL,
		Latency:    latency,
	}, nil
}

// buildURL constructs the full Gemini API URL:
//
//	{baseURL}/models/{model}:streamGenerateContent?alt=sse&key={key}   (streaming)
//	{baseURL}/models/{model}:generateContent?key={key}                  (non-streaming)
func (e *GeminiExecutor) buildURL(req *ExecuteRequest) (string, error) {
	if req.Model == "" {
		return "", fmt.Errorf("model is required for Gemini requests")
	}

	var sb strings.Builder
	sb.WriteString(e.baseURL)
	sb.WriteString("/models/")
	sb.WriteString(req.Model)

	// Resolve the API key (if any) before building the query string so we
	// can avoid a dangling '?' when there is no key.
	var apiKey string
	if req.Credentials != nil {
		apiKey = req.Credentials.APIKey
	}

	if req.Stream {
		sb.WriteString(":streamGenerateContent?alt=sse")
		if apiKey != "" {
			sb.WriteString("&key=")
			sb.WriteString(apiKey)
		}
	} else {
		sb.WriteString(":generateContent")
		if apiKey != "" {
			sb.WriteString("?key=")
			sb.WriteString(apiKey)
		}
	}

	return sb.String(), nil
}
