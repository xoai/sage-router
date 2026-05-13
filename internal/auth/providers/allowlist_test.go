package providers

import "testing"

func TestModelInAllowlist_ExactMatch(t *testing.T) {
	allowed := []string{"gpt-5", "gpt-4o", "o3-mini"}
	cases := map[string]bool{
		"gpt-5":      true,
		"gpt-4o":     true,
		"o3-mini":    true,
		"gpt-3.5":    false,
		"gpt-5-mini": false, // exact match only — no implicit prefix
		// Empty model is tested separately by TestModelInAllowlist_EmptyModel —
		// it's permitted unconditionally per godoc.
	}
	for model, want := range cases {
		t.Run(model, func(t *testing.T) {
			if got := ModelInAllowlist(model, allowed); got != want {
				t.Errorf("ModelInAllowlist(%q) = %v, want %v", model, got, want)
			}
		})
	}
}

func TestModelInAllowlist_WildcardSuffix(t *testing.T) {
	// `-x` suffix on an allowlist entry matches any model starting with
	// the prefix BEFORE the `-x`. Per spec: claude-sonnet-4-x matches
	// claude-sonnet-4, claude-sonnet-4-20251015, claude-sonnet-4-1, etc.
	allowed := []string{"claude-sonnet-4-x", "claude-opus-4-x"}
	cases := map[string]bool{
		"claude-sonnet-4":            true, // bare prefix
		"claude-sonnet-4-20251015":   true, // dated variant
		"claude-sonnet-4-1":          true, // numbered variant
		"claude-sonnet-4-thinking":   true, // any suffix
		"claude-opus-4-20260101":     true,
		"claude-sonnet-3-5":          false, // different version
		"claude-sonnet-40-special":   false, // would-be false-positive if we matched on "claude-sonnet-4" without boundary
		"claude-sonnet-4x-leftover":  false, // ditto
	}
	for model, want := range cases {
		t.Run(model, func(t *testing.T) {
			if got := ModelInAllowlist(model, allowed); got != want {
				t.Errorf("ModelInAllowlist(%q) = %v, want %v", model, got, want)
			}
		})
	}
}

func TestModelInAllowlist_EmptyListAllowsAll(t *testing.T) {
	// An empty allowlist means "no restriction" — used for apikey
	// connections which can hit any model the API supports.
	if !ModelInAllowlist("anything", nil) {
		t.Error("nil allowlist should permit all models")
	}
	if !ModelInAllowlist("anything", []string{}) {
		t.Error("empty allowlist should permit all models")
	}
}

func TestModelInAllowlist_EmptyModel(t *testing.T) {
	// Empty model string is the "no model specified" case used during
	// selection of connections for non-model-specific operations.
	// Treat as permitted so the selection logic isn't accidentally
	// over-restrictive.
	if !ModelInAllowlist("", []string{"gpt-5"}) {
		t.Error("empty model should pass any allowlist (no restriction applicable)")
	}
}

func TestSubscriptionAllowed_LooksUpProvider(t *testing.T) {
	if !SubscriptionAllowed("anthropic", "claude-opus-4") {
		t.Error("anthropic + claude-opus-4 should be allowed by registry")
	}
	if SubscriptionAllowed("anthropic", "gpt-5") {
		t.Error("anthropic + gpt-5 should NOT be allowed (different provider)")
	}
	if !SubscriptionAllowed("openai", "gpt-5") {
		t.Error("openai + gpt-5 should be allowed")
	}
	if SubscriptionAllowed("not-a-provider", "anything") {
		t.Error("unknown provider should NOT be allowed")
	}
}
