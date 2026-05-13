package catalog

import (
	"context"
	"strings"
	"testing"

	"sage-router/internal/config"
)

// TestSeedFromConstants_PopulatesCatalog — AC2. After SeedFromConstants
// runs on a fresh DB, catalog_models and catalog_pricing contain one
// row per entry in config.ModelCatalog, all with source='seed'.
func TestSeedFromConstants_PopulatesCatalog(t *testing.T) {
	cs, _, ctx := freshStore(t)

	if err := SeedFromConstants(ctx, cs); err != nil {
		t.Fatalf("SeedFromConstants: %v", err)
	}

	models, err := cs.ListModels(ctx, "")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != len(config.ModelCatalog) {
		t.Fatalf("seeded %d models, want %d", len(models), len(config.ModelCatalog))
	}

	// Every row carries source='seed' and pricing matches the static
	// catalog. Verifies AC2's "with source='seed'" clause.
	for _, m := range models {
		want, ok := config.ModelCatalog[m.ModelID]
		if !ok {
			t.Errorf("unexpected seeded model %q (not in config.ModelCatalog)", m.ModelID)
			continue
		}
		if m.Source != SourceSeed {
			t.Errorf("%s: Source=%q, want seed", m.ModelID, m.Source)
		}
		if m.Pricing.Source != SourceSeed {
			t.Errorf("%s: Pricing.Source=%q, want seed", m.ModelID, m.Pricing.Source)
		}
		if m.Provider != want.Provider {
			t.Errorf("%s: Provider=%q, want %q", m.ModelID, m.Provider, want.Provider)
		}
		if m.Tier != want.Tier {
			t.Errorf("%s: Tier=%d, want %d", m.ModelID, m.Tier, want.Tier)
		}
		if m.Pricing.Input != want.InputPrice {
			t.Errorf("%s: Pricing.Input=%v, want %v", m.ModelID, m.Pricing.Input, want.InputPrice)
		}
		if m.Pricing.Output != want.OutputPrice {
			t.Errorf("%s: Pricing.Output=%v, want %v", m.ModelID, m.Pricing.Output, want.OutputPrice)
		}
		if m.Caps.SupportsImages != want.SupportsImages {
			t.Errorf("%s: SupportsImages mismatch", m.ModelID)
		}
		if m.Caps.SupportsTools != want.SupportsTools {
			t.Errorf("%s: SupportsTools mismatch", m.ModelID)
		}
		if m.Caps.SupportsThinking != want.SupportsThinking {
			t.Errorf("%s: SupportsThinking mismatch", m.ModelID)
		}
	}
}

// TestSeedFromConstants_Idempotent — AC3. Second invocation produces
// zero semantic change: same row count, same per-row values. The
// underlying SQL may UPDATE rows with identical values (and tick
// updated_at to the current second), which is a no-op from any
// caller's perspective — the visible state matches what the first
// run produced.
func TestSeedFromConstants_Idempotent(t *testing.T) {
	cs, _, ctx := freshStore(t)

	if err := SeedFromConstants(ctx, cs); err != nil {
		t.Fatalf("SeedFromConstants 1: %v", err)
	}
	before, _ := cs.ListModels(ctx, "")

	if err := SeedFromConstants(ctx, cs); err != nil {
		t.Fatalf("SeedFromConstants 2: %v", err)
	}
	after, _ := cs.ListModels(ctx, "")

	if len(before) != len(after) {
		t.Fatalf("row count drifted: before=%d after=%d", len(before), len(after))
	}
	// Every row's semantic content must match across runs. We compare
	// every field except UpdatedAt (which may have ticked into the
	// next second under -race overhead — a stylistic SQL detail, not
	// a correctness issue).
	for i := range before {
		b, a := before[i], after[i]
		if b.Provider != a.Provider || b.ModelID != a.ModelID {
			t.Errorf("row %d identity drift: %s/%s vs %s/%s",
				i, b.Provider, b.ModelID, a.Provider, a.ModelID)
		}
		if b.DisplayName != a.DisplayName ||
			b.Tier != a.Tier ||
			b.ContextWindow != a.ContextWindow ||
			b.MaxOutput != a.MaxOutput ||
			b.Caps != a.Caps ||
			b.Pricing != a.Pricing ||
			b.Source != a.Source {
			t.Errorf("%s: semantic drift on idempotent re-run\nbefore: %+v\nafter:  %+v",
				b.ModelID, b, a)
		}
	}
}

// TestSeedFromConstants_NeverOverwritesNonSeed — AC4. A row already
// written by discovery (or any non-seed source) must survive an
// idempotent seed re-run unchanged.
func TestSeedFromConstants_NeverOverwritesNonSeed(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Pre-seed a single row with source='discovery' that COLLIDES with
	// a static-catalog model. Use a model we know is in config.ModelCatalog.
	const provider, modelID = "openai", "gpt-4.1"
	if _, ok := config.ModelCatalog[modelID]; !ok {
		t.Fatalf("test premise broken: %s not in config.ModelCatalog", modelID)
	}
	if err := cs.UpsertModel(ctx, Model{
		Provider: provider, ModelID: modelID,
		DisplayName: "Discovery Custom", Tier: TierStrong,
		Source: SourceDiscovery,
	}); err != nil {
		t.Fatalf("pre-seed discovery row: %v", err)
	}

	if err := SeedFromConstants(ctx, cs); err != nil {
		t.Fatalf("SeedFromConstants: %v", err)
	}

	got, err := cs.GetModel(ctx, provider, modelID)
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got.Source != SourceDiscovery {
		t.Errorf("seed overwrote discovery row: Source=%q", got.Source)
	}
	if got.DisplayName != "Discovery Custom" {
		t.Errorf("seed overwrote discovery row: DisplayName=%q", got.DisplayName)
	}
}

// TestSeedFromConstants_AnthropicCacheWriteNonZero — AC2d. Every
// Anthropic-family seed row must have CacheWrite > 0 (= Input*1.25).
// Prevents the future-Anthropic-clone seed oversight described in
// ADR-3 §Failure-modes. The convention is: CacheWrite=0 means "free
// at write" by design (OpenAI, Gemini); Anthropic explicitly writes
// the surcharge.
func TestSeedFromConstants_AnthropicCacheWriteNonZero(t *testing.T) {
	cs, _, ctx := freshStore(t)

	if err := SeedFromConstants(ctx, cs); err != nil {
		t.Fatalf("SeedFromConstants: %v", err)
	}

	models, _ := cs.ListModels(ctx, "anthropic")
	if len(models) == 0 {
		t.Fatal("no anthropic models seeded")
	}
	for _, m := range models {
		if !strings.Contains(m.ModelID, "claude") {
			continue
		}
		if m.Pricing.CacheWrite <= 0 {
			t.Errorf("Anthropic model %s seeded with CacheWrite=%v; want > 0 (Input*1.25 convention)",
				m.ModelID, m.Pricing.CacheWrite)
		}
		// Spot-check: CacheWrite should be approximately Input * 1.25.
		want := m.Pricing.Input * 1.25
		if delta := m.Pricing.CacheWrite - want; delta < -1e-9 || delta > 1e-9 {
			t.Errorf("Anthropic %s CacheWrite=%v, want Input*1.25=%v",
				m.ModelID, m.Pricing.CacheWrite, want)
		}
	}
}

// TestSeedProviderMeta_DefaultValues — AC2c. Seeded provider_meta
// rows must have backoff_step=0, next_discovery_after=NULL, and the
// per-provider discovery_enabled / subscription_discoverable values
// from the ADR-2 defaults table.
func TestSeedProviderMeta_DefaultValues(t *testing.T) {
	cs, _, ctx := freshStore(t)

	if err := SeedProviderMeta(ctx, cs); err != nil {
		t.Fatalf("SeedProviderMeta: %v", err)
	}

	want := map[string]struct {
		discoveryEnabled         bool
		subscriptionDiscoverable bool
	}{
		"openai":         {true, true},
		"anthropic":      {true, true},
		"gemini":         {true, false},
		"openrouter":     {true, false},
		"ollama":         {true, false},
		"github-copilot": {false, false},
	}

	for provider, expected := range want {
		got, err := cs.GetProviderMeta(ctx, provider)
		if err != nil {
			t.Fatalf("GetProviderMeta %s: %v", provider, err)
		}
		if got == nil {
			t.Errorf("%s: not seeded", provider)
			continue
		}
		if got.DiscoveryEnabled != expected.discoveryEnabled {
			t.Errorf("%s: DiscoveryEnabled=%v, want %v",
				provider, got.DiscoveryEnabled, expected.discoveryEnabled)
		}
		if got.SubscriptionDiscoverable != expected.subscriptionDiscoverable {
			t.Errorf("%s: SubscriptionDiscoverable=%v, want %v",
				provider, got.SubscriptionDiscoverable, expected.subscriptionDiscoverable)
		}
		// AC2c: backoff_step=0, next_discovery_after=zero-time.
		if got.BackoffStep != 0 {
			t.Errorf("%s: BackoffStep=%d, want 0", provider, got.BackoffStep)
		}
		if !got.NextDiscoveryAfter.IsZero() {
			t.Errorf("%s: NextDiscoveryAfter=%v, want zero", provider, got.NextDiscoveryAfter)
		}
		if got.LastDiscoveryError != "" {
			t.Errorf("%s: LastDiscoveryError=%q, want empty", provider, got.LastDiscoveryError)
		}
	}
}

// TestSeedProviderMeta_Idempotent — second invocation must NOT
// overwrite an existing row that already has runtime state (e.g.,
// a row updated by the discovery loop with LastDiscoveredAt set).
func TestSeedProviderMeta_Idempotent(t *testing.T) {
	cs, _, ctx := freshStore(t)

	if err := SeedProviderMeta(ctx, cs); err != nil {
		t.Fatalf("SeedProviderMeta 1: %v", err)
	}

	// Simulate a discovery-loop write that updated backoff_step.
	pm, _ := cs.GetProviderMeta(ctx, "openai")
	pm.BackoffStep = 2
	pm.LastDiscoveryError = "401 from /v1/models"
	if err := cs.SetProviderMeta(ctx, "openai", *pm); err != nil {
		t.Fatalf("SetProviderMeta: %v", err)
	}

	// Re-run the seeder. It must NOT clobber the runtime fields.
	if err := SeedProviderMeta(ctx, cs); err != nil {
		t.Fatalf("SeedProviderMeta 2: %v", err)
	}

	got, _ := cs.GetProviderMeta(ctx, "openai")
	if got.BackoffStep != 2 {
		t.Errorf("seed clobbered runtime backoff_step: got %d, want 2", got.BackoffStep)
	}
	if got.LastDiscoveryError != "401 from /v1/models" {
		t.Errorf("seed clobbered runtime LastDiscoveryError: %q", got.LastDiscoveryError)
	}
}

// AC2b — both seed functions are callable from test code. This is
// satisfied implicitly by every test in this file (they all call
// SeedFromConstants and/or SeedProviderMeta from internal/store
// migrations + a catalog.Store), but the explicit version asserts
// the contract is documented in the public API.
func TestSeed_CallableFromTestCode(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Both functions accept (ctx, catalog.Store) and return error.
	// This compile-time check would fail if either signature changed.
	var _ func(context.Context, Store) error = SeedFromConstants
	var _ func(context.Context, Store) error = SeedProviderMeta

	if err := SeedFromConstants(ctx, cs); err != nil {
		t.Fatalf("SeedFromConstants: %v", err)
	}
	if err := SeedProviderMeta(ctx, cs); err != nil {
		t.Fatalf("SeedProviderMeta: %v", err)
	}
}
