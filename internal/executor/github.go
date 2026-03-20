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
	githubCopilotBaseURL  = "https://api.githubcopilot.com"
	githubCopilotEndpoint = "/chat/completions"

	// Default header values for GitHub Copilot requests.
	githubEditorVersion      = "vscode/1.100.0"
	githubCopilotIntegration = "sage-router"
)

// GitHubCopilotExecutor handles requests to the GitHub Copilot API.
// It speaks the OpenAI wire format but requires additional Copilot-specific
// headers (Editor-Version, Copilot-Integration-Id).
type GitHubCopilotExecutor struct {
	baseURL string
	pool    *ClientPool

	// EditorVersion overrides the default Editor-Version header value.
	EditorVersion string
	// IntegrationID overrides the default Copilot-Integration-Id header value.
	IntegrationID string
}

// NewGitHubCopilotExecutor creates a GitHubCopilotExecutor.
// If baseURL is empty, the default GitHub Copilot API URL is used.
func NewGitHubCopilotExecutor(baseURL string, pool *ClientPool) *GitHubCopilotExecutor {
	if baseURL == "" {
		baseURL = githubCopilotBaseURL
	}
	return &GitHubCopilotExecutor{
		baseURL:       strings.TrimRight(baseURL, "/"),
		pool:          pool,
		EditorVersion: githubEditorVersion,
		IntegrationID: githubCopilotIntegration,
	}
}

// Provider implements Executor.
func (e *GitHubCopilotExecutor) Provider() string {
	return "github-copilot"
}

// Execute implements Executor. It sends a request to the GitHub Copilot API
// with the required Copilot-specific headers.
func (e *GitHubCopilotExecutor) Execute(ctx context.Context, req *ExecuteRequest) (*Result, error) {
	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = githubCopilotEndpoint
	}
	targetURL := e.baseURL + endpoint

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("github copilot executor: build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Editor-Version", e.EditorVersion)
	httpReq.Header.Set("Copilot-Integration-Id", e.IntegrationID)

	// Apply authentication. Copilot uses Bearer tokens.
	if req.Credentials != nil {
		token := req.Credentials.AccessToken
		if token == "" {
			token = req.Credentials.APIKey
		}
		if token != "" {
			httpReq.Header.Set("Authorization", "Bearer "+token)
		}
	}

	client := e.pool.Get(req.ProxyURL)

	start := time.Now()
	resp, err := client.Do(httpReq)
	latency := time.Since(start)

	if err != nil {
		return nil, fmt.Errorf("github copilot executor: do request: %w", err)
	}

	return &Result{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       resp.Body,
		URL:        targetURL,
		Latency:    latency,
	}, nil
}
