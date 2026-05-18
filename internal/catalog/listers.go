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
		// Mirror lister: DEAD CODE pending follow-up removal cycle.
		// DiscoveryListerKey no longer dispatches to this key — it was
		// superseded by `openai@codex-subscription` below at cycle
		// 20260517-provider-auth-variants M2-discovery.3 (the broad
		// OpenRouter catalog surfaced models the codex backend can't
		// serve). Retained-but-unreachable for one cycle so an operator
		// who needs to flip back via an env-var override has a
		// well-tested code path to revert to; if no rollback need
		// surfaces by the NEXT cycle, delete `lister_openrouter_mirror.go`
		// + its test + this map entry. Post-ship /review M1 flag.
		"openai@openrouter-mirror": ListerFunc(listOpenAIViaOpenRouter),

		// Codex-subscription lister: returns the small known-good model
		// whitelist for the chatgpt.com/backend-api/codex/responses surface.
		// Per-account variation is real (M0.8 evidence — see
		// .sage/work/20260517-provider-auth-variants/m0-baseline.md Finding 4):
		// a prolite-tier account accepts gpt-5.4 but rejects gpt-5.2 /
		// gpt-5.1-codex / codex-mini-latest. We seed a permissive list
		// with the model the M0 capture verified, plus likely-available
		// variants documented by the predecessor cycle. The backend
		// ultimately gates; this list just hints the dashboard's catalog.
		// Cycle 20260517-provider-auth-variants M2-discovery.2.
		"openai@codex-subscription": ListerFunc(listCodexSupportedModels),
	}
}

// codexSupportedModels is the permissive whitelist for openai+subscription
// (chatgpt.com/backend-api/codex/responses). Per-account variation is real
// — some accounts get only gpt-5.4, others get gpt-5.x/codex-mini-latest.
// The variant Executor will surface backend "model not supported" 400s with
// a clear error; the dashboard's catalog page is a hint, not a contract.
//
// Source of truth ordering:
//  1. gpt-5.4         — M0.8 LIVE-VALIDATED (request_id 027a7bc5-... HTTP 200)
//  2. gpt-5.1-codex   — predecessor cycle's ADR + Codex CLI source
//  3. gpt-5.2         — predecessor cycle's ADR
//  4. codex-mini-latest — Codex CLI's mini variant
//
// Future cycle: surface X-Codex-Bengalfox-Limit-Name from a successful
// response and dynamically extend the catalog with the user's actual
// allowed models. Open Q deferred from M2 — see ADR-codex-subscription-contract.md.
var codexSupportedModels = []Model{
	{ModelID: "gpt-5.4", Provider: "openai", DisplayName: "GPT-5.4 (Codex/ChatGPT)", Source: SourceDiscovery},
	{ModelID: "gpt-5.1-codex", Provider: "openai", DisplayName: "GPT-5.1 Codex", Source: SourceDiscovery},
	{ModelID: "gpt-5.2", Provider: "openai", DisplayName: "GPT-5.2", Source: SourceDiscovery},
	{ModelID: "codex-mini-latest", Provider: "openai", DisplayName: "Codex Mini (latest)", Source: SourceDiscovery},
}

// listCodexSupportedModels is the ModelLister impl for openai+subscription.
// Returns the static permissive whitelist regardless of credentials —
// backend gates the actual model resolution, this is purely a catalog seed.
func listCodexSupportedModels(_ context.Context, _ ListerCredentials) ([]Model, error) {
	// Return a defensive copy so callers can't mutate the package-level
	// constant via the returned slice's backing array.
	out := make([]Model, len(codexSupportedModels))
	copy(out, codexSupportedModels)
	return out, nil
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
