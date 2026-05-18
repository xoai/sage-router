package executor

import (
	"context"
	"testing"
)

// stubExecutor is a no-op Executor used by Variants tests. It carries a
// label so tests can assert which variant was returned by Get().
type stubExecutor struct{ label string }

func (s *stubExecutor) Provider() string { return s.label }
func (s *stubExecutor) Execute(ctx context.Context, req *ExecuteRequest) (*Result, error) {
	return &Result{StatusCode: 200, URL: "stub://" + s.label}, nil
}

// TestVariants_GetExactMatch — when (provider, auth_type) is registered
// directly, Get returns it. No wildcard fallback consulted.
func TestVariants_GetExactMatch(t *testing.T) {
	v := NewVariants()
	exact := &stubExecutor{label: "openai-apikey"}
	wild := &stubExecutor{label: "openai-wild"}
	v.Register(VariantKey{Provider: "openai", AuthType: "apikey"}, exact)
	v.Register(VariantKey{Provider: "openai", AuthType: ""}, wild)

	got, ok := v.Get("openai", "apikey")
	if !ok {
		t.Fatal("Get(openai, apikey) returned ok=false; want true")
	}
	if got.Provider() != "openai-apikey" {
		t.Errorf("exact-match returned %q, want openai-apikey (wildcard mistakenly preferred?)", got.Provider())
	}
}

// TestVariants_GetWildcardFallback — when (provider, auth_type) is NOT
// registered but (provider, "") IS, Get returns the wildcard entry.
func TestVariants_GetWildcardFallback(t *testing.T) {
	v := NewVariants()
	wild := &stubExecutor{label: "gemini-wild"}
	v.Register(VariantKey{Provider: "gemini", AuthType: ""}, wild)

	got, ok := v.Get("gemini", "apikey")
	if !ok {
		t.Fatal("Get(gemini, apikey) returned ok=false; want true via wildcard fallback")
	}
	if got.Provider() != "gemini-wild" {
		t.Errorf("wildcard returned %q, want gemini-wild", got.Provider())
	}
}

// TestVariants_GetMiss — when neither (P, A) nor (P, "") is registered,
// Get returns (nil, false). No silent fallthrough to a default executor.
func TestVariants_GetMiss(t *testing.T) {
	v := NewVariants()
	v.Register(VariantKey{Provider: "openai", AuthType: "apikey"}, &stubExecutor{label: "x"})

	got, ok := v.Get("anthropic", "subscription")
	if ok {
		t.Errorf("Get(anthropic, subscription) returned ok=true; want false")
	}
	if got != nil {
		t.Errorf("Get miss returned non-nil executor %v", got)
	}
}

// TestVariants_Iterate — Iterate visits every registered (key, executor)
// pair. Used by main.go's RetryExecutor wrap loop and startup validation.
// Per m1 holistic-review fold: API is Iterate(fn), NOT All() returning a
// mutable map, so callers can't mutate the registry post-init.
func TestVariants_Iterate(t *testing.T) {
	v := NewVariants()
	v.Register(VariantKey{Provider: "openai", AuthType: "apikey"}, &stubExecutor{label: "a"})
	v.Register(VariantKey{Provider: "openai", AuthType: "subscription"}, &stubExecutor{label: "b"})
	v.Register(VariantKey{Provider: "anthropic", AuthType: ""}, &stubExecutor{label: "c"})

	seen := map[VariantKey]string{}
	v.Iterate(func(k VariantKey, e Executor) {
		seen[k] = e.Provider()
	})

	want := map[VariantKey]string{
		{Provider: "openai", AuthType: "apikey"}:       "a",
		{Provider: "openai", AuthType: "subscription"}: "b",
		{Provider: "anthropic", AuthType: ""}:          "c",
	}
	if len(seen) != len(want) {
		t.Errorf("iterated %d entries, want %d", len(seen), len(want))
	}
	for k, label := range want {
		if got := seen[k]; got != label {
			t.Errorf("Iterate saw %v=%q, want %q", k, got, label)
		}
	}
}
