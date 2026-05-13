package catalog

import (
	"context"
	"errors"
	"fmt"
)

// ListerCredentials carries the per-call inputs every ModelLister
// needs. It's a thin, provider-agnostic value type so the catalog
// package doesn't have to import internal/store (the connection
// shape is built by the caller from store.Connection +
// config.KnownProviders).
type ListerCredentials struct {
	// BaseURL is the provider's API root — e.g.
	// "https://api.anthropic.com/v1" for Anthropic. From the static
	// internal/config/providers.go: KnownProviders table.
	BaseURL string

	// APIKey is the user's bearer / x-api-key credential. May be
	// empty for subscription-auth connections; AccessToken takes
	// precedence then.
	APIKey string

	// AccessToken is the subscription-auth (OAuth) bearer token.
	// When set, listers should prefer this over APIKey.
	AccessToken string

	// ExtraHeaders forwards any per-provider headers — e.g.
	// "anthropic-version" — that aren't covered by the auth fields.
	// Optional; nil is fine.
	ExtraHeaders map[string]string
}

// ModelLister queries one provider's "list available models" endpoint
// and returns []catalog.Model. The caller stamps Source = SourceDiscovery
// before persisting (so listers stay decoupled from the catalog's
// source-precedence rules).
//
// Implementations must respect ctx for cancellation/timeout. Per-call
// timeout is set by the DiscoveryRunner (typically 10s).
type ModelLister interface {
	ListModels(ctx context.Context, creds ListerCredentials) ([]Model, error)
}

// ErrDiscoveryUnsupported is returned by listers for providers that
// do not expose a public model-list endpoint (e.g., github-copilot
// as of 2026-05-12 — see .sage/docs/anthropic-discovery-feasibility.md
// for the analogous discovery-feasibility doc when one is needed for
// that provider).
var ErrDiscoveryUnsupported = errors.New("catalog: discovery not supported by this provider")

// ListerFunc adapts a free function into a ModelLister, so per-provider
// implementations can stay stateless without forcing a wrapper struct.
type ListerFunc func(ctx context.Context, creds ListerCredentials) ([]Model, error)

// ListModels makes ListerFunc satisfy ModelLister.
func (f ListerFunc) ListModels(ctx context.Context, creds ListerCredentials) ([]Model, error) {
	return f(ctx, creds)
}

// ---------------------------------------------------------------------------
// github-copilot — stub (no confirmed public /v1/models endpoint as of
// 2026-05-12). The seeded ProviderMeta sets discovery_enabled=false so
// this lister is never invoked by the DiscoveryRunner in production;
// the stub exists so the registry map has an entry for every known
// provider, making the absence explicit.
// ---------------------------------------------------------------------------

// listGitHubCopilotModels always returns ErrDiscoveryUnsupported.
// When/if GitHub publishes a Copilot models endpoint, replace the
// body and flip provider_meta.discovery_enabled=true in the seed.
func listGitHubCopilotModels(_ context.Context, _ ListerCredentials) ([]Model, error) {
	return nil, ErrDiscoveryUnsupported
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// BuiltinListers returns the production lister-per-provider map.
// Used by the DiscoveryRunner (task 2.3) to dispatch ListModels.
// Provider IDs match internal/config/providers.go: KnownProviders keys.
func BuiltinListers() map[string]ModelLister {
	return map[string]ModelLister{
		"openai":         ListerFunc(listOpenAIModels),
		"anthropic":      ListerFunc(listAnthropicModels),
		"gemini":         ListerFunc(listGeminiModels),
		"openrouter":     ListerFunc(listOpenRouterModels),
		"ollama":         ListerFunc(listOllamaModels),
		"github-copilot": ListerFunc(listGitHubCopilotModels),
	}
}

// listerCheckResponse is a small helper: returns an error sized for
// the response body when HTTP status isn't 200. Used by every
// concrete lister so error messages stay consistent.
func listerStatusError(provider string, status int, body []byte) error {
	const maxBodySnippet = 256
	snippet := body
	if len(snippet) > maxBodySnippet {
		snippet = snippet[:maxBodySnippet]
	}
	return fmt.Errorf("lister %s: HTTP %d: %s", provider, status, string(snippet))
}
