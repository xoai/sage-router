package executor

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Result holds the outcome of a single upstream API call.
type Result struct {
	StatusCode int
	Headers    http.Header
	Body       io.ReadCloser
	URL        string
	Latency    time.Duration
}

// Executor is the interface every provider-specific executor must implement.
type Executor interface {
	// Provider returns the canonical provider ID (e.g. "openai", "anthropic").
	Provider() string

	// Execute sends the request to the upstream provider and returns the raw
	// result. The caller is responsible for closing Result.Body.
	Execute(ctx context.Context, req *ExecuteRequest) (*Result, error)
}

// ExecuteRequest contains everything an Executor needs to build and send the
// upstream HTTP request.
type ExecuteRequest struct {
	Model       string
	Body        []byte
	Stream      bool
	Credentials *Credentials
	ProxyURL    string
	Endpoint    string // The target API endpoint path (e.g. "/v1/chat/completions")
}

// Credentials carries authentication material for a single upstream connection.
type Credentials struct {
	ConnectionID string
	AuthType     string // "apikey" | "subscription" | "none" (canonical — see auth.NormalizeAuthType)
	AccessToken  string
	RefreshToken string
	APIKey       string
	ExpiresAt    time.Time
	ProviderData map[string]any
	// ExtraHeaders are provider-specific request headers beyond the standard
	// Authorization header. The server populates this from auth.Credential.ExtraHeaders().
	// Currently used by OpenAI subscription for the ChatGPT-Account-ID header.
	ExtraHeaders map[string]string
}
