package catalog

import "testing"

// Initiative: 20260514-openrouter-fallback. DiscoveryListerKey routes
// (provider, authType) pairs to the right lister-registry key. Most
// pairs use the provider's own lister; subscription openai routes to
// the OpenRouter-mirror lister because ChatGPT subscription tokens
// lack `api.model.read` scope.

func TestDiscoveryListerKey_DefaultIsProvider(t *testing.T) {
	cases := []struct {
		provider, authType, want string
	}{
		// apikey connections always dispatch to the direct lister.
		{"openai", "apikey", "openai"},
		{"anthropic", "apikey", "anthropic"},
		{"gemini", "apikey", "gemini"},
		{"openrouter", "apikey", "openrouter"},
		{"ollama", "apikey", "ollama"},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"_"+tc.authType, func(t *testing.T) {
			got := DiscoveryListerKey(tc.provider, tc.authType)
			if got != tc.want {
				t.Errorf("DiscoveryListerKey(%q, %q) = %q, want %q",
					tc.provider, tc.authType, got, tc.want)
			}
		})
	}
}

// TestDiscoveryListerKey_OpenAISubscriptionRoutesToCodex — load-bearing
// dispatch. Cycle 20260517-provider-auth-variants M2-discovery.3 swapped
// the destination from "openai@openrouter-mirror" (legacy — pulled the
// full OpenRouter catalog, surfacing models the user couldn't actually
// serve) to "openai@codex-subscription" (returns the small permissive
// whitelist aligned with chatgpt.com/backend-api/codex/responses, M0.8
// live-validated for gpt-5.4).
func TestDiscoveryListerKey_OpenAISubscriptionRoutesToCodex(t *testing.T) {
	got := DiscoveryListerKey("openai", "subscription")
	want := "openai@codex-subscription"
	if got != want {
		t.Errorf("DiscoveryListerKey(openai, subscription) = %q, want %q (cycle 20260517-provider-auth-variants M2-discovery.3)",
			got, want)
	}
}

// TestDiscoveryListerKey_AnthropicSubscriptionKeepsDirect — anthropic
// subscription connections should keep using the direct lister.
// Anthropic /v1/models may have its own subscription quirks (R2 from
// the 20260514-discovery-url-doubling plan), but those are TBD and
// the dispatch helper should not assume.
func TestDiscoveryListerKey_AnthropicSubscriptionKeepsDirect(t *testing.T) {
	got := DiscoveryListerKey("anthropic", "subscription")
	if got != "anthropic" {
		t.Errorf("DiscoveryListerKey(anthropic, subscription) = %q, want anthropic (no fallback today)",
			got)
	}
}
