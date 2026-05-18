package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"sage-router/internal/auth"
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
//
// Cycle 20260517-provider-auth-variants M2.6.1: openai+subscription routing
// + ExchangedToken preference REMOVED from this executor. The variant
// abstraction now dispatches (openai, subscription) to CodexSubscriptionExecutor
// which routes to chatgpt.com/backend-api/codex/responses with the PKCE
// access_token used directly. DefaultExecutor is the (provider, apikey)
// variant for openai/openrouter/ollama and stays bare /chat/completions.
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

	// Apply authentication. Unknown AuthType values fail loudly — see the
	// claude.go comment for the rationale on dropping the legacy fallback.
	if req.Credentials != nil {
		switch req.Credentials.AuthType {
		case auth.AuthTypeAPIKey:
			httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.APIKey)
		case auth.AuthTypeSubscription:
			// PKCE access_token used directly as Bearer. The variant
			// abstraction (M1+M2) routes openai+subscription to
			// CodexSubscriptionExecutor instead; this branch survives for
			// any (provider, subscription) registered against DefaultExecutor
			// directly (none today, but the wildcard registration in main.go
			// keeps the branch viable).
			httpReq.Header.Set("Authorization", "Bearer "+req.Credentials.AccessToken)
			for k, v := range req.Credentials.ExtraHeaders {
				httpReq.Header.Set(k, v)
			}
		case auth.AuthTypeNone:
			// No auth header needed.
		default:
			return nil, fmt.Errorf("%s executor: unsupported auth_type %q for connection %s",
				e.provider, req.Credentials.AuthType, req.Credentials.ConnectionID)
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

// OverrideCapabilities implements CapabilityOverrider (Models Discovery M3.5).
// Identity body for the M3 baseline — DefaultExecutor is shared by
// openai / openrouter / ollama / custom OpenAI-compat providers, each
// with its own catalog row set. The interface is wired so a future
// patch (e.g., GPT-5 capability inheritance) doesn't need to rewire
// the smart-router. The current body returns base unchanged.
func (e *DefaultExecutor) OverrideCapabilities(model string, base Capabilities) Capabilities {
	return base
}
