package catalog

import "sage-router/internal/auth"

// DiscoveryListerKey selects the lister-registry key (in BuiltinListers())
// for a given (provider, authType) pair.
//
// Most pairs dispatch to the provider's own lister. The exception: when
// the provider's direct /models endpoint forbids subscription tokens
// (currently only openai — ChatGPT subscription scopes don't include
// `api.model.read`), the dispatch routes to a "mirror" lister that
// uses a public catalog source (OpenRouter) and writes results back
// under the provider key. See fix 20260514-openrouter-fallback.
//
// The capability suffix "@openrouter-mirror" names the BEHAVIOR (use
// OpenRouter as the mirror) rather than coupling to the auth type, so
// future providers that hit the same architectural mismatch can follow
// the same naming convention. The `@` is intentional: no existing
// provider key uses it as a delimiter, so the namespace is collision-free.
//
// Provider keys aren't templated into URLs (the executor reads BaseURL
// from config.KnownProviders, not the lister key) and aren't used as
// JSON object keys with constraints — the `@` is purely a logical
// separator inside BuiltinListers().
func DiscoveryListerKey(provider, authType string) string {
	if authType == auth.AuthTypeSubscription && provider == "openai" {
		// Cycle 20260517-provider-auth-variants M2-discovery.3: switched
		// from openai@openrouter-mirror to openai@codex-subscription.
		// The mirror lister surfaced models the user couldn't actually
		// serve (it pulled the full OpenRouter catalog including non-Codex
		// variants); the codex-subscription lister returns a small
		// permissive whitelist aligned with the chatgpt.com/backend-api
		// surface (M0.8 live validation).
		return "openai@codex-subscription"
	}
	return provider
}
