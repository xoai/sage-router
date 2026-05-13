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
func TestBuiltinListers_CoversAllKnownProviders(t *testing.T) {
	listers := BuiltinListers()
	want := []string{"openai", "anthropic", "gemini", "openrouter", "ollama", "github-copilot"}
	for _, p := range want {
		if _, ok := listers[p]; !ok {
			t.Errorf("BuiltinListers missing entry for %q", p)
		}
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
		if !found {
			t.Errorf("BuiltinListers has unexpected entry %q", k)
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
