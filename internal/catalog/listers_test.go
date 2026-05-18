package catalog

import (
	"context"
	"errors"
	"testing"
)

// TestBuiltinListers_CoversAllKnownProviders — the production registry
// must include one entry per provider in
// internal/config/providers.go: KnownProviders. Drift would cause a
// silent "no lister for X" failure in the discovery loop.
//
// Capability listers (keys containing "@") are allowed in BuiltinListers
// to support fallback dispatch (e.g., "openai@openrouter-mirror" for
// subscription OpenAI). They're tracked separately in `capabilityKeys`
// below.
func TestBuiltinListers_CoversAllKnownProviders(t *testing.T) {
	listers := BuiltinListers()
	want := []string{"openai", "anthropic", "gemini", "openrouter", "ollama", "github-copilot"}
	for _, p := range want {
		if _, ok := listers[p]; !ok {
			t.Errorf("BuiltinListers missing entry for %q", p)
		}
	}
	// Capability listers — mirror/fallback dispatch keys. Each entry
	// must explain why it's there so the registry stays honest.
	capabilityKeys := map[string]string{
		"openai@openrouter-mirror":  "legacy: pre-M2 subscription openai routed here via openrouter mirror (fix 20260514-openrouter-fallback). Retained for backward-compat during the M2 migration window.",
		"openai@codex-subscription": "cycle 20260517-provider-auth-variants M2-discovery.2: returns Codex-supported model whitelist for chatgpt.com/backend-api/codex/responses",
	}
	// Reject unexpected entries — keeps the registry honest.
	for k := range listers {
		found := false
		for _, p := range want {
			if k == p {
				found = true
				break
			}
		}
		if _, isCapability := capabilityKeys[k]; isCapability {
			found = true
		}
		if !found {
			t.Errorf("BuiltinListers has unexpected entry %q (add to `want` or `capabilityKeys` with rationale)", k)
		}
	}
}

// TestGitHubCopilotLister_ReturnsErrDiscoveryUnsupported — the stub
// must report unsupported (not silently succeed with an empty list).
// The seeded ProviderMeta sets discovery_enabled=false for this
// provider so production never invokes it, but the contract matters
// for any test path or future enablement.
func TestGitHubCopilotLister_ReturnsErrDiscoveryUnsupported(t *testing.T) {
	listers := BuiltinListers()
	lister, ok := listers["github-copilot"]
	if !ok {
		t.Fatal("github-copilot not in BuiltinListers")
	}
	_, err := lister.ListModels(context.Background(), ListerCredentials{})
	if !errors.Is(err, ErrDiscoveryUnsupported) {
		t.Errorf("github-copilot lister err = %v, want ErrDiscoveryUnsupported", err)
	}
}

// TestCodexSubscriptionLister_ReturnsPermissiveWhitelist — cycle
// 20260517-provider-auth-variants M2-discovery.2. The codex-subscription
// lister returns the small known-good whitelist for the
// chatgpt.com/backend-api/codex/responses surface. M0.8 evidence
// (m0-baseline.md Finding 4) proved the whitelist is per-account; the
// lister returns a permissive seed and the backend gates the actual
// model resolution at request time.
//
// Pin invariants:
//   - gpt-5.4 is present (M0 LIVE-VALIDATED)
//   - at least one Codex-specific variant is present (gpt-5.1-codex / codex-mini-latest)
//   - all entries have Provider="openai"
//   - the lister doesn't error or require credentials (it's a static seed)
func TestCodexSubscriptionLister_ReturnsPermissiveWhitelist(t *testing.T) {
	listers := BuiltinListers()
	lister, ok := listers["openai@codex-subscription"]
	if !ok {
		t.Fatal("openai@codex-subscription not registered in BuiltinListers")
	}
	models, err := lister.ListModels(context.Background(), ListerCredentials{})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("empty list — should return at least the M0-validated gpt-5.4")
	}

	seen := map[string]bool{}
	for _, m := range models {
		if m.Provider != "openai" {
			t.Errorf("model %q has Provider=%q, want openai", m.ModelID, m.Provider)
		}
		if m.Source != SourceDiscovery {
			t.Errorf("model %q has Source=%q, want %q", m.ModelID, m.Source, SourceDiscovery)
		}
		seen[m.ModelID] = true
	}

	// gpt-5.4 is the load-bearing entry — empirically validated at M0.8
	// against the prolite tier; if this disappears, M2's live-validated
	// contract loses its catalog hint.
	if !seen["gpt-5.4"] {
		t.Error("gpt-5.4 missing from codex-subscription lister output (M0 LIVE-VALIDATED model — must be present)")
	}
	// At least one Codex-specific variant — covers accounts with broader
	// Codex access than the prolite tier the M0 test was on.
	hasCodexVariant := seen["gpt-5.1-codex"] || seen["codex-mini-latest"] || seen["gpt-5-codex"]
	if !hasCodexVariant {
		t.Error("no Codex-specific variant in lister output — forward-compat with plus/pro tiers requires at least one")
	}
}

// TestCodexSubscriptionLister_ReturnedSliceIsDefensiveCopy — pins that
// callers mutating the returned slice can't corrupt the package-level
// codexSupportedModels constant. Memory `1ad0007d` adjacent pattern:
// shared backing arrays cause cross-test pollution.
func TestCodexSubscriptionLister_ReturnedSliceIsDefensiveCopy(t *testing.T) {
	listers := BuiltinListers()
	lister := listers["openai@codex-subscription"]
	a, _ := lister.ListModels(context.Background(), ListerCredentials{})
	if len(a) == 0 {
		t.Fatal("empty list")
	}
	originalFirstModelID := a[0].ModelID
	a[0].ModelID = "tampered"

	b, _ := lister.ListModels(context.Background(), ListerCredentials{})
	if b[0].ModelID != originalFirstModelID {
		t.Errorf("returned slice shares backing array with package constant: %q leaked after mutation", b[0].ModelID)
	}
}

// TestListerFunc_SatisfiesInterface — ListerFunc adapter must
// implement ModelLister.
func TestListerFunc_SatisfiesInterface(t *testing.T) {
	var lf ListerFunc = func(_ context.Context, _ ListerCredentials) ([]Model, error) {
		return []Model{{Provider: "test", ModelID: "m1"}}, nil
	}
	var _ ModelLister = lf // compile-time assertion

	models, err := lf.ListModels(context.Background(), ListerCredentials{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(models) != 1 || models[0].ModelID != "m1" {
		t.Errorf("unexpected: %+v", models)
	}
}
