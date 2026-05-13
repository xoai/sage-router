package catalog

import (
	"context"
	"testing"
	"time"

	"sage-router/internal/store"
)

// freshStore opens an in-memory SQLite, runs all migrations, enables
// foreign keys, and returns a catalog.Store wrapping the same
// connection.
func freshStore(t *testing.T) (Store, store.Store, context.Context) {
	t.Helper()
	s, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db := s.DB()
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign_keys: %v", err)
	}
	return NewSQLiteStore(db), s, context.Background()
}

func TestSQLiteStore_UpsertGetModel_RoundTrip(t *testing.T) {
	cs, _, ctx := freshStore(t)

	m := Model{
		Provider:      "openai",
		ModelID:       "gpt-4.1",
		DisplayName:   "GPT-4.1",
		Tier:          TierFrontier,
		ContextWindow: 1_000_000,
		MaxOutput:     32768,
		Caps: Capabilities{
			SupportsImages:   true,
			SupportsTools:    true,
			SupportsThinking: false,
		},
		Source: SourceSeed,
	}
	if err := cs.UpsertModel(ctx, m); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}

	got, err := cs.GetModel(ctx, "openai", "gpt-4.1")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got == nil {
		t.Fatal("GetModel returned nil for known model")
	}
	if got.DisplayName != "GPT-4.1" {
		t.Errorf("DisplayName = %q, want \"GPT-4.1\"", got.DisplayName)
	}
	if got.Tier != TierFrontier {
		t.Errorf("Tier = %d, want %d", got.Tier, TierFrontier)
	}
	if !got.Caps.SupportsImages || !got.Caps.SupportsTools || got.Caps.SupportsThinking {
		t.Errorf("Caps = %+v, want images=tools=true thinking=false", got.Caps)
	}
	if got.Source != SourceSeed {
		t.Errorf("Source = %q, want %q", got.Source, SourceSeed)
	}
}

func TestSQLiteStore_GetModel_NotFound(t *testing.T) {
	cs, _, ctx := freshStore(t)
	got, err := cs.GetModel(ctx, "openai", "no-such")
	if err != nil {
		t.Fatalf("GetModel err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for unknown model, got %+v", got)
	}
}

// AC11b — ListModels MUST return rows ordered by (provider, model_id) ASC.
// Shuffle insert order; verify deterministic output.
func TestSQLiteStore_ListModels_DeterministicOrder(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Insert in shuffled order.
	rows := []Model{
		{Provider: "openai", ModelID: "z-model", Source: SourceSeed},
		{Provider: "anthropic", ModelID: "claude-sonnet-4-6", Source: SourceSeed},
		{Provider: "openai", ModelID: "a-model", Source: SourceSeed},
		{Provider: "anthropic", ModelID: "claude-haiku-4-5", Source: SourceSeed},
	}
	for _, r := range rows {
		if err := cs.UpsertModel(ctx, r); err != nil {
			t.Fatalf("UpsertModel %s/%s: %v", r.Provider, r.ModelID, err)
		}
	}

	got, err := cs.ListModels(ctx, "")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []struct{ provider, modelID string }{
		{"anthropic", "claude-haiku-4-5"},
		{"anthropic", "claude-sonnet-4-6"},
		{"openai", "a-model"},
		{"openai", "z-model"},
	}
	if len(got) != len(want) {
		t.Fatalf("ListModels len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Provider != w.provider || got[i].ModelID != w.modelID {
			t.Errorf("row %d = %s/%s, want %s/%s",
				i, got[i].Provider, got[i].ModelID, w.provider, w.modelID)
		}
	}
}

func TestSQLiteStore_ListModels_FilterByProvider(t *testing.T) {
	cs, _, ctx := freshStore(t)

	for _, r := range []Model{
		{Provider: "openai", ModelID: "gpt-4.1", Source: SourceSeed},
		{Provider: "anthropic", ModelID: "claude-sonnet-4-6", Source: SourceSeed},
		{Provider: "anthropic", ModelID: "claude-haiku-4-5", Source: SourceSeed},
	} {
		if err := cs.UpsertModel(ctx, r); err != nil {
			t.Fatalf("UpsertModel: %v", err)
		}
	}

	got, err := cs.ListModels(ctx, "anthropic")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListModels(anthropic) len = %d, want 2", len(got))
	}
	for _, m := range got {
		if m.Provider != "anthropic" {
			t.Errorf("unexpected provider %q in filtered list", m.Provider)
		}
	}
}

// Source precedence: seed cannot overwrite non-seed rows.
func TestSQLiteStore_UpsertModel_SeedDoesNotOverwriteDiscovery(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// First write: discovery row.
	first := Model{
		Provider: "openai", ModelID: "gpt-4.1",
		DisplayName: "GPT-4.1 (discovered)", Tier: TierFrontier,
		Source: SourceDiscovery,
	}
	if err := cs.UpsertModel(ctx, first); err != nil {
		t.Fatalf("UpsertModel discovery: %v", err)
	}

	// Second write: seed value with different display name — should be ignored.
	seed := Model{
		Provider: "openai", ModelID: "gpt-4.1",
		DisplayName: "GPT-4.1 (seed)", Tier: TierStrong,
		Source: SourceSeed,
	}
	if err := cs.UpsertModel(ctx, seed); err != nil {
		t.Fatalf("UpsertModel seed: %v", err)
	}

	got, err := cs.GetModel(ctx, "openai", "gpt-4.1")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got.DisplayName != "GPT-4.1 (discovered)" {
		t.Errorf("seed overwrote discovery row: DisplayName = %q", got.DisplayName)
	}
	if got.Source != SourceDiscovery {
		t.Errorf("Source = %q after seed write, want discovery preserved", got.Source)
	}
}

// Source precedence: user cannot be overwritten except by user.
func TestSQLiteStore_UpsertModel_UserCannotBeOverwrittenExceptByUser(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// User write.
	user := Model{
		Provider: "openai", ModelID: "gpt-4.1",
		DisplayName: "User Custom", Source: SourceUser,
	}
	if err := cs.UpsertModel(ctx, user); err != nil {
		t.Fatalf("UpsertModel user: %v", err)
	}

	// Discovery should be ignored.
	disc := Model{
		Provider: "openai", ModelID: "gpt-4.1",
		DisplayName: "Discovery Override", Source: SourceDiscovery,
	}
	if err := cs.UpsertModel(ctx, disc); err != nil {
		t.Fatalf("UpsertModel discovery: %v", err)
	}

	got, _ := cs.GetModel(ctx, "openai", "gpt-4.1")
	if got.DisplayName != "User Custom" {
		t.Errorf("discovery overwrote user row: DisplayName = %q", got.DisplayName)
	}

	// User can overwrite user.
	user2 := Model{
		Provider: "openai", ModelID: "gpt-4.1",
		DisplayName: "User Custom v2", Source: SourceUser,
	}
	if err := cs.UpsertModel(ctx, user2); err != nil {
		t.Fatalf("UpsertModel user2: %v", err)
	}
	got, _ = cs.GetModel(ctx, "openai", "gpt-4.1")
	if got.DisplayName != "User Custom v2" {
		t.Errorf("user-over-user blocked: DisplayName = %q", got.DisplayName)
	}
}

func TestSQLiteStore_UpsertPricing_RoundTrip(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Pricing requires the FK target to exist.
	if err := cs.UpsertModel(ctx, Model{
		Provider: "openai", ModelID: "gpt-4.1", Source: SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}

	p := Pricing{
		Input: 2.0, Output: 8.0,
		CacheRead: 0.5, CacheWrite: 0,
		Source: SourceSeed,
	}
	if err := cs.UpsertPricing(ctx, "openai", "gpt-4.1", p); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	got, err := cs.GetPricing(ctx, "openai", "gpt-4.1")
	if err != nil {
		t.Fatalf("GetPricing: %v", err)
	}
	if got == nil {
		t.Fatal("GetPricing returned nil")
	}
	if got.Input != 2.0 || got.Output != 8.0 || got.CacheRead != 0.5 {
		t.Errorf("Pricing fields = %+v, want input=2.0 output=8.0 cache_read=0.5", got)
	}
	if got.Source != SourceSeed {
		t.Errorf("Source = %q, want seed", got.Source)
	}
}

// Pricing-specific rule: discovery may NOT clobber openrouter pricing.
// (User-overrides + discovery vs seed are tested above; this is the
// extra rule from ADR-2 §Conflict resolution.)
func TestSQLiteStore_UpsertPricing_DiscoveryDoesNotClobberOpenRouter(t *testing.T) {
	cs, _, ctx := freshStore(t)

	if err := cs.UpsertModel(ctx, Model{
		Provider: "anthropic", ModelID: "claude-sonnet-4-6", Source: SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}

	or := Pricing{Input: 3.0, Output: 15.0, Source: SourceOpenRouter}
	if err := cs.UpsertPricing(ctx, "anthropic", "claude-sonnet-4-6", or); err != nil {
		t.Fatalf("UpsertPricing openrouter: %v", err)
	}

	disc := Pricing{Input: 99.0, Output: 999.0, Source: SourceDiscovery}
	if err := cs.UpsertPricing(ctx, "anthropic", "claude-sonnet-4-6", disc); err != nil {
		t.Fatalf("UpsertPricing discovery: %v", err)
	}

	got, _ := cs.GetPricing(ctx, "anthropic", "claude-sonnet-4-6")
	if got.Input != 3.0 {
		t.Errorf("discovery clobbered openrouter Input: got %v, want 3.0", got.Input)
	}
	if got.Source != SourceOpenRouter {
		t.Errorf("Source = %q, want openrouter preserved", got.Source)
	}
}

// User pricing override beats openrouter.
func TestSQLiteStore_UpsertPricing_UserBeatsOpenRouter(t *testing.T) {
	cs, _, ctx := freshStore(t)

	if err := cs.UpsertModel(ctx, Model{
		Provider: "anthropic", ModelID: "claude-sonnet-4-6", Source: SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}

	or := Pricing{Input: 3.0, Source: SourceOpenRouter}
	_ = cs.UpsertPricing(ctx, "anthropic", "claude-sonnet-4-6", or)

	user := Pricing{Input: 0.01, Source: SourceUser}
	if err := cs.UpsertPricing(ctx, "anthropic", "claude-sonnet-4-6", user); err != nil {
		t.Fatalf("UpsertPricing user: %v", err)
	}

	got, _ := cs.GetPricing(ctx, "anthropic", "claude-sonnet-4-6")
	if got.Input != 0.01 || got.Source != SourceUser {
		t.Errorf("user override not applied: got %+v", got)
	}
}

func TestSQLiteStore_BulkUpsertPricing(t *testing.T) {
	cs, _, ctx := freshStore(t)

	for _, mp := range []string{"openai/gpt-4.1", "anthropic/claude-sonnet-4-6"} {
		var prov, mid string
		for i, c := range mp {
			if c == '/' {
				prov, mid = mp[:i], mp[i+1:]
				break
			}
		}
		if err := cs.UpsertModel(ctx, Model{Provider: prov, ModelID: mid, Source: SourceSeed}); err != nil {
			t.Fatalf("UpsertModel %s: %v", mp, err)
		}
	}

	updates := []PricingUpdate{
		{Provider: "openai", ModelID: "gpt-4.1",
			Pricing: Pricing{Input: 2.5, Output: 10.0, Source: SourceOpenRouter}},
		{Provider: "anthropic", ModelID: "claude-sonnet-4-6",
			Pricing: Pricing{Input: 3.0, Output: 15.0, Source: SourceOpenRouter}},
	}
	if err := cs.BulkUpsertPricing(ctx, updates); err != nil {
		t.Fatalf("BulkUpsertPricing: %v", err)
	}

	gp1, _ := cs.GetPricing(ctx, "openai", "gpt-4.1")
	if gp1 == nil || gp1.Input != 2.5 {
		t.Errorf("openai pricing not applied: %+v", gp1)
	}
	gp2, _ := cs.GetPricing(ctx, "anthropic", "claude-sonnet-4-6")
	if gp2 == nil || gp2.Input != 3.0 {
		t.Errorf("anthropic pricing not applied: %+v", gp2)
	}
}

func TestSQLiteStore_SetGetProviderMeta_RoundTrip(t *testing.T) {
	cs, _, ctx := freshStore(t)

	now := time.Date(2026, 5, 12, 14, 30, 0, 0, time.UTC)
	pm := ProviderMeta{
		Provider:                 "openai",
		DiscoveryEnabled:         true,
		SubscriptionDiscoverable: true,
		LastDiscoveredAt:         now,
		LastDiscoveryError:       "test-error",
		BackoffStep:              2,
		NextDiscoveryAfter:       now.Add(2 * time.Hour),
	}
	if err := cs.SetProviderMeta(ctx, "openai", pm); err != nil {
		t.Fatalf("SetProviderMeta: %v", err)
	}

	got, err := cs.GetProviderMeta(ctx, "openai")
	if err != nil {
		t.Fatalf("GetProviderMeta: %v", err)
	}
	if got == nil {
		t.Fatal("GetProviderMeta returned nil")
	}
	if !got.DiscoveryEnabled || !got.SubscriptionDiscoverable {
		t.Errorf("bool fields = %v/%v, want true/true",
			got.DiscoveryEnabled, got.SubscriptionDiscoverable)
	}
	if got.LastDiscoveryError != "test-error" {
		t.Errorf("LastDiscoveryError = %q", got.LastDiscoveryError)
	}
	if got.BackoffStep != 2 {
		t.Errorf("BackoffStep = %d, want 2", got.BackoffStep)
	}
	if d := got.LastDiscoveredAt.Sub(now); d < -time.Second || d > time.Second {
		t.Errorf("LastDiscoveredAt drift = %v", d)
	}
	if d := got.NextDiscoveryAfter.Sub(now.Add(2 * time.Hour)); d < -time.Second || d > time.Second {
		t.Errorf("NextDiscoveryAfter drift = %v", d)
	}
}

func TestSQLiteStore_GetProviderMeta_NotFound(t *testing.T) {
	cs, _, ctx := freshStore(t)
	got, err := cs.GetProviderMeta(ctx, "no-such")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for unknown provider, got %+v", got)
	}
}

func TestSQLiteStore_ListProviderMetas(t *testing.T) {
	cs, _, ctx := freshStore(t)

	for _, p := range []string{"openai", "anthropic", "gemini"} {
		if err := cs.SetProviderMeta(ctx, p, ProviderMeta{Provider: p, DiscoveryEnabled: true}); err != nil {
			t.Fatalf("SetProviderMeta %s: %v", p, err)
		}
	}

	got, err := cs.ListProviderMetas(ctx)
	if err != nil {
		t.Fatalf("ListProviderMetas: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("ListProviderMetas len = %d, want 3", len(got))
	}
	// Ordered by provider ASC.
	want := []string{"anthropic", "gemini", "openai"}
	for i, w := range want {
		if got[i].Provider != w {
			t.Errorf("row %d provider = %q, want %q", i, got[i].Provider, w)
		}
	}
}
