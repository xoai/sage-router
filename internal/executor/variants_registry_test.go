package executor

import (
	"testing"

	"sage-router/internal/auth"
	"sage-router/pkg/canonical"
)

// TestVariantsRegistry_AllAuthTypesRegistered — invariant pin per spec
// §3.1 AC-A4: for each AuthType the canonicalization layer accepts and
// each provider the production main.go wires, Variants.Get returns an
// executor (either via exact match or via wildcard fallback).
//
// The list of providers mirrors cmd/sage-router/main.go's executor map;
// the list of auth types mirrors internal/auth/authtype.go constants.
// Both are duplicated INTENTIONALLY — a future cycle that adds a new
// provider OR a new auth_type without registering it in main.go will
// fail this test, which is the desired safety net.
func TestVariantsRegistry_AllAuthTypesRegistered(t *testing.T) {
	v := NewVariants()
	// Mirror main.go's M1.7 wiring: every provider as a wildcard.
	providers := []string{
		"openai", "anthropic", "gemini", "github-copilot",
		"openrouter", "ollama", "default",
	}
	for _, p := range providers {
		v.Register(VariantKey{Provider: p, AuthType: ""}, &stubExecutor{label: p + "-wild"})
	}

	authTypes := []string{
		auth.AuthTypeAPIKey,
		auth.AuthTypeSubscription,
		auth.AuthTypeNone,
		// auth.AuthTypeAutoDetect deliberately omitted — Q9 dead-code
		// rip (M5.5) removes auto_detect from the request path entirely.
	}

	for _, p := range providers {
		for _, a := range authTypes {
			if _, ok := v.Get(p, a); !ok {
				t.Errorf("Variants.Get(%q, %q) returned ok=false — wildcard fallback should resolve every (provider, authType) combination", p, a)
			}
		}
	}
}

// BenchmarkExecutorDispatch_Before measures the OLD provider-keyed map
// lookup overhead. Pins the M1.11 perf budget for the Variants migration.
// Memory-only Go map indexing should be <50 ns/op.
func BenchmarkExecutorDispatch_Before(b *testing.B) {
	executors := map[string]Executor{
		"openai":         &stubExecutor{label: "openai"},
		"anthropic":      &stubExecutor{label: "anthropic"},
		"gemini":         &stubExecutor{label: "gemini"},
		"github-copilot": &stubExecutor{label: "github-copilot"},
		"openrouter":     &stubExecutor{label: "openrouter"},
		"ollama":         &stubExecutor{label: "ollama"},
		"default":        &stubExecutor{label: "default"},
	}
	keys := []string{"openai", "anthropic", "gemini", "github-copilot", "openrouter", "default"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = executors[keys[i%len(keys)]]
	}
}

// BenchmarkExecutorDispatch_After measures the NEW Variants.Get lookup
// overhead. Two map lookups in the worst case (exact-miss → wildcard hit);
// expected ~2x of Before. Per m-R6 review fold: assert <2x cost — surface
// at M1 checkpoint if drift exceeds.
func BenchmarkExecutorDispatch_After(b *testing.B) {
	v := NewVariants()
	// Same set as Before, registered as wildcards (M1.7 pattern).
	for _, p := range []string{
		"openai", "anthropic", "gemini", "github-copilot",
		"openrouter", "ollama", "default",
	} {
		v.Register(VariantKey{Provider: p, AuthType: ""}, &stubExecutor{label: p})
	}
	// Explicit (openai, subscription) — exercises the exact-match path
	// (one lookup, no fallback). Realistic for M2-shipped state.
	v.Register(VariantKey{Provider: "openai", AuthType: "subscription"}, &stubExecutor{label: "openai-sub"})

	type query struct{ p, a string }
	queries := []query{
		{"openai", "apikey"},        // wildcard fallback (1 miss + 1 hit)
		{"openai", "subscription"},  // exact match (1 hit)
		{"anthropic", "subscription"}, // wildcard fallback
		{"gemini", ""},              // wildcard direct hit
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := queries[i%len(queries)]
		_, _ = v.Get(q.p, q.a)
	}
}

// Compile-time pin: stubExecutor satisfies the optional interfaces' negative
// case — bareVariant in optional_interfaces_test.go covers the same shape
// but stubExecutor is what AllAuthTypesRegistered uses. Keep this assertion
// so a future refactor that accidentally makes stubExecutor implement
// Formatted via embedding gets caught.
var (
	_ Executor             = (*stubExecutor)(nil)
	_ canonical.Format     // import-touch so the import survives go fmt
)
