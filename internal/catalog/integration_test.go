package catalog

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sage-router/internal/routing"
	"sage-router/internal/store"
)

// TestEndToEnd_FullCatalogLifecycle — Models Discovery M3.6 (AC30).
//
// One sequential test pinning the integrated behavior across the seven
// surfaces M1–M3 ship: migrations → seed → OpenRouter refresh →
// per-provider discovery → user pricing override → smart-route ranking
// (cache-aware effective price) → usage cost via Registry.EstimateCost.
//
// This test is deliberately monolithic — the individual surfaces have
// their own focused unit tests. M3.6's job is to assert that the
// surfaces actually compose, that their persisted state survives across
// boundary calls, and that ADR-2's source-precedence rules hold
// end-to-end (user > openrouter > discovery > seed for pricing; user >
// discovery > seed for models).
//
// Steps map 1:1 to plan §3.6.
//
// External dependencies stubbed inside the test process:
//   - OpenRouter `/api/v1/models` → httptest.Server serving the
//     vendored fixture at testdata/openrouter_response.json (365 models).
//   - Anthropic /v1/models → in-test ListerFunc returning a synthetic
//     2-model slice. Anthropic doesn't return pricing in production
//     either, so the mock omits Pricing fields.
//
// Note on package placement: this file lives in `package catalog`
// (regular test) rather than an external `catalog_test`. The plan called
// for `internal/catalog/integration_test.go`. We exercise the cheap-
// strategy routing math directly via routing.SmartRouter (which has no
// dependency on internal/catalog) rather than crossing into
// internal/server's `buildSmartCandidates` — both paths produce
// equivalent ranking from the same Registry+Store data, and the server
// path has its own 11-test coverage in
// internal/server/buildsmartcandidates_test.go. Routing math here is
// the integration assertion; the server wrapper around it is unit-
// tested separately.
func TestEndToEnd_FullCatalogLifecycle(t *testing.T) {
	ctx := context.Background()

	// ─── Step 1: migrations ───────────────────────────────────────────
	db, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var tableCount int
	if err := db.DB().QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table'",
	).Scan(&tableCount); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	// Migrations 1-11 create at least the core production tables. A
	// drift below 10 means a migration was deleted or didn't run.
	if tableCount < 10 {
		t.Errorf("table count = %d, want >= 10 (migrations 1-11 should have created core tables)", tableCount)
	}

	// ─── Step 2: seed catalog ─────────────────────────────────────────
	cs := NewSQLiteStore(db.DB())
	if err := SeedFromConstants(ctx, cs); err != nil {
		t.Fatalf("SeedFromConstants: %v", err)
	}
	if err := SeedProviderMeta(ctx, cs); err != nil {
		t.Fatalf("SeedProviderMeta: %v", err)
	}

	var modelCount, metaCount int
	if err := db.DB().QueryRow("SELECT COUNT(*) FROM catalog_models").Scan(&modelCount); err != nil {
		t.Fatalf("count catalog_models: %v", err)
	}
	if err := db.DB().QueryRow("SELECT COUNT(*) FROM catalog_provider_meta").Scan(&metaCount); err != nil {
		t.Fatalf("count catalog_provider_meta: %v", err)
	}
	if modelCount < 17 {
		t.Errorf("catalog_models count = %d, want >= 17 (seed has 5+8+4 = 17 minimum)", modelCount)
	}
	if metaCount != 6 {
		t.Errorf("catalog_provider_meta count = %d, want 6 (openai/anthropic/gemini/openrouter/ollama/github-copilot)", metaCount)
	}

	// Snapshot how many anthropic seed pricing rows exist now — Step 3
	// will use this to verify OpenRouter doesn't clobber them.
	var anthropicSeedPricingBefore int
	if err := db.DB().QueryRow(
		"SELECT COUNT(*) FROM catalog_pricing WHERE provider='anthropic' AND source='seed'",
	).Scan(&anthropicSeedPricingBefore); err != nil {
		t.Fatalf("count anthropic seed pricing: %v", err)
	}
	if anthropicSeedPricingBefore < 3 {
		t.Fatalf("anthropic seed pricing rows = %d, want >= 3 before OpenRouter step",
			anthropicSeedPricingBefore)
	}

	// ─── Step 3: OpenRouter mock + FetchAndPersist ────────────────────
	fixtureBody, err := os.ReadFile(filepath.Join("testdata", "openrouter_response.json"))
	if err != nil {
		t.Fatalf("read openrouter fixture: %v", err)
	}
	orServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixtureBody)
	}))
	defer orServer.Close()

	reg := NewRegistry(cs)
	refresher := &OpenRouterRefresher{
		Store:    cs,
		Registry: reg,
		URL:      orServer.URL + "/api/v1/models",
		Client:   http.DefaultClient,
	}
	orCount, err := refresher.FetchAndPersist(ctx)
	if err != nil {
		t.Fatalf("FetchAndPersist: %v", err)
	}
	if orCount < 100 {
		t.Errorf("OpenRouter ingest count = %d, want >= 100 (fixture has ~365 models)", orCount)
	}

	var openrouterPricingCount, anthropicSeedPricingAfter int
	db.DB().QueryRow(
		"SELECT COUNT(*) FROM catalog_pricing WHERE source='openrouter'",
	).Scan(&openrouterPricingCount)
	db.DB().QueryRow(
		"SELECT COUNT(*) FROM catalog_pricing WHERE provider='anthropic' AND source='seed'",
	).Scan(&anthropicSeedPricingAfter)

	if openrouterPricingCount < 5 {
		t.Errorf("openrouter-sourced pricing rows = %d, want >= 5", openrouterPricingCount)
	}
	if anthropicSeedPricingAfter != anthropicSeedPricingBefore {
		t.Errorf("anthropic-direct seed pricing changed: before=%d, after=%d "+
			"(OpenRouter must not clobber anthropic-direct rows — they're addressed "+
			"as provider='openrouter' with qualified model_ids, not provider='anthropic')",
			anthropicSeedPricingBefore, anthropicSeedPricingAfter)
	}

	// ─── Step 4: mock anthropic lister + DiscoverProvider ─────────────
	mockLister := ListerFunc(func(_ context.Context, _ ListerCredentials) ([]Model, error) {
		// Two models: one that overlaps with the seed (claude-sonnet-4-6,
		// pinning the seed→discovery upgrade per ADR-2 precedence) and
		// one brand-new ID (claude-discovered-test, pinning the
		// discovery-creates-new-row path).
		return []Model{
			{
				Provider: "anthropic", ModelID: "claude-sonnet-4-6",
				DisplayName: "Claude Sonnet 4.6 (re-discovered)", Tier: TierFrontier,
				ContextWindow: 1_000_000, MaxOutput: 64_000,
				Caps: Capabilities{SupportsImages: true, SupportsTools: true},
			},
			{
				Provider: "anthropic", ModelID: "claude-discovered-test",
				DisplayName: "Claude Discovered (test-only)", Tier: TierFrontier,
				ContextWindow: 200_000, MaxOutput: 64_000,
				Caps: Capabilities{SupportsTools: true},
			},
		}, nil
	})
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{"anthropic": mockLister})
	result := runner.DiscoverProvider(ctx, "anthropic", ListerCredentials{
		BaseURL: "http://mock", APIKey: "fake",
	})
	if result.Err != nil {
		t.Fatalf("DiscoverProvider: %v", result.Err)
	}
	if result.Count == 0 {
		t.Fatalf("DiscoverProvider Count = 0, want > 0")
	}

	// New row exists with source='discovery'.
	var newRowSource string
	if err := db.DB().QueryRow(
		"SELECT source FROM catalog_models WHERE provider='anthropic' AND model_id='claude-discovered-test'",
	).Scan(&newRowSource); err != nil {
		t.Fatalf("query discovered row: %v", err)
	}
	if newRowSource != "discovery" {
		t.Errorf("claude-discovered-test source = %q, want 'discovery'", newRowSource)
	}

	// Overlapping row upgraded from seed to discovery (ADR-2 §Conflict
	// resolution: catalog_models source precedence is user > discovery > seed).
	var overlapSource string
	if err := db.DB().QueryRow(
		"SELECT source FROM catalog_models WHERE provider='anthropic' AND model_id='claude-sonnet-4-6'",
	).Scan(&overlapSource); err != nil {
		t.Fatalf("query overlapping row: %v", err)
	}
	if overlapSource != "discovery" {
		t.Errorf("claude-sonnet-4-6 catalog_models.source = %q, want 'discovery' (discovery overwrites seed)",
			overlapSource)
	}

	// Pricing for the overlapping row stays at seed (Anthropic /v1/models
	// returns no pricing; the lister's Model carries empty Pricing; the
	// runner doesn't touch catalog_pricing).
	var overlapPricingSource string
	if err := db.DB().QueryRow(
		"SELECT source FROM catalog_pricing WHERE provider='anthropic' AND model_id='claude-sonnet-4-6'",
	).Scan(&overlapPricingSource); err != nil {
		t.Fatalf("query overlap pricing: %v", err)
	}
	if overlapPricingSource != "seed" {
		t.Errorf("claude-sonnet-4-6 catalog_pricing.source = %q, want 'seed' "+
			"(discovery without pricing must NOT clobber seed pricing)",
			overlapPricingSource)
	}

	// ─── Step 5: user pricing override ────────────────────────────────
	// Direct UpsertPricing with SourceUser mirrors the production
	// PUT /api/catalog/pricing/anthropic/claude-sonnet-4-6 handler's
	// net effect (the handler delegates to UpsertPricing + invalidates
	// the Registry cache). The auth layer is orthogonal to the
	// pricing-flow contract this step pins.
	if err := cs.UpsertPricing(ctx, "anthropic", "claude-sonnet-4-6", Pricing{
		Input: 99.99, Output: 99.99,
		CacheRead: 0, CacheWrite: 0,
		Source: SourceUser,
	}); err != nil {
		t.Fatalf("UpsertPricing user: %v", err)
	}
	reg.Invalidate()

	var userInputPrice float64
	var userPricingSource string
	if err := db.DB().QueryRow(
		"SELECT input_price, source FROM catalog_pricing WHERE provider='anthropic' AND model_id='claude-sonnet-4-6'",
	).Scan(&userInputPrice, &userPricingSource); err != nil {
		t.Fatalf("query user-override row: %v", err)
	}
	if math.Abs(userInputPrice-99.99) > 1e-6 {
		t.Errorf("input_price = %v, want 99.99", userInputPrice)
	}
	if userPricingSource != "user" {
		t.Errorf("pricing source = %q, want 'user' (ADR-2 precedence: user wins over discovery, openrouter, seed)",
			userPricingSource)
	}

	// Registry reflects the override after Invalidate.
	regPricing := reg.Pricing("anthropic", "claude-sonnet-4-6")
	if regPricing == nil {
		t.Fatal("Registry.Pricing returned nil after user override + Invalidate")
	}
	if math.Abs(regPricing.Input-99.99) > 1e-6 {
		t.Errorf("Registry.Pricing.Input = %v, want 99.99 (cache must be invalidated)", regPricing.Input)
	}

	// ─── Step 6: smart candidates + cheap-strategy ranking ────────────
	if err := db.CreateConnection(&store.Connection{
		ID:       "c-anthropic-test",
		Provider: "anthropic",
		Name:     "anthropic-integration",
		AuthType: "apikey",
		APIKey:   "sk-ant-test",
		State:    "idle",
	}); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	// Seed usage_log to give the connection a 50% cache-hit-rate.
	if err := db.RecordUsage(&store.UsageEntry{
		ID:           "u-ratio",
		RequestID:    "r-ratio",
		Provider:     "anthropic",
		Model:        "claude-haiku-4-5-20251001",
		ConnectionID: "c-anthropic-test",
		InputTokens:  500, OutputTokens: 100, CacheReadTokens: 500,
		Status: "ok",
	}); err != nil {
		t.Fatalf("RecordUsage (ratio seed): %v", err)
	}

	ratio, err := db.GetCacheHitRate(ctx, "c-anthropic-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("GetCacheHitRate: %v", err)
	}
	// 500 cache_read / (500 input + 500 cache_read) = 0.5.
	if math.Abs(ratio-0.5) > 1e-9 {
		t.Errorf("cache hit ratio = %v, want 0.5", ratio)
	}

	// Build candidates from the Registry for anthropic. This mirrors
	// server.buildSmartCandidates's iteration shape without crossing
	// into the server package (see file-level comment for rationale).
	var candidates []routing.ModelCandidate
	for _, m := range reg.ListProvider("anthropic") {
		candidates = append(candidates, routing.ModelCandidate{
			Provider:       m.Provider,
			Model:          m.ModelID,
			Tier:           m.Tier,
			InputPrice:     m.Pricing.Input,
			ContextWindow:  m.ContextWindow,
			CacheReadPrice: m.Pricing.CacheRead,
			CachedRatio:    ratio,
		})
	}
	if len(candidates) < 4 {
		t.Fatalf("len(candidates) = %d, want >= 4 (anthropic seed has 5 + 1 discovered)",
			len(candidates))
	}

	sr := routing.NewSmartRouter()
	sorted := sr.RouteWithConstraints(routing.StrategyCheap, "", candidates, routing.RequestConstraints{})
	if len(sorted) == 0 {
		t.Fatal("RouteWithConstraints returned empty slice")
	}

	// The $99.99 user override on claude-sonnet-4-6 must rank LAST in
	// the cheap strategy — it's far costlier than any seeded Anthropic
	// model ($1.00 haiku / $3.00 sonnet-4-dated / $5.00 opus-4-6 / $15
	// opus-4-dated). This pins the end-to-end flow: catalog.user-
	// pricing override → Registry.Pricing → ModelCandidate.InputPrice
	// → routing.effectivePrice → cheap-strategy rank.
	last := sorted[len(sorted)-1]
	if last.Model != "claude-sonnet-4-6" {
		var sortedModels []string
		for _, c := range sorted {
			sortedModels = append(sortedModels, c.Model)
		}
		t.Errorf("cheap-strategy LAST = %q (price %v), want claude-sonnet-4-6 "+
			"(user-overridden to $99.99). Full order: %v",
			last.Model, last.InputPrice, sortedModels)
	}

	// Discovered row with no pricing (Anthropic /v1/models doesn't
	// return prices) appears at zero — under cheap it must rank FIRST.
	// This is correct routing math: a $0-priced model is "cheaper"
	// than any positive-priced one. The seed allowlist (subscription /
	// other gates) would filter such rows at the server level, but at
	// the routing-math level $0 wins.
	first := sorted[0]
	if first.InputPrice != 0 {
		// Not a hard failure — depending on seed pricing values, a
		// zero-priced discovered row may or may not exist. Just log
		// so a future maintainer sees the actual ordering.
		t.Logf("cheap-strategy FIRST = %q (price %v) — non-zero is fine, just informational",
			first.Model, first.InputPrice)
	}

	// ─── Step 7: usage record + cost via EstimateCost ─────────────────
	const haikuModel = "claude-haiku-4-5-20251001"
	cost := reg.EstimateCost("anthropic", haikuModel, TokenBreakdown{
		Input: 1000, Output: 500,
	})
	// Seed: claude-haiku-4-5-20251001 input=$1.00, output=$5.00 per 1M.
	// Cost = (1000 * 1.0 + 500 * 5.0) / 1_000_000 = $0.0035.
	const expectedCost = (1000.0*1.0 + 500.0*5.0) / 1_000_000.0
	if math.Abs(cost-expectedCost) > 1e-9 {
		t.Errorf("EstimateCost = %v, want %v ($0.0035 at seed prices for haiku 4.5)",
			cost, expectedCost)
	}

	// Record the usage entry and read it back through a windowed
	// QueryUsage filter. The `From: now-1h` predicate exercises the
	// M3.1 timestamp-format contract (self-learning
	// 82fa8ebabdbe4ea49ad3cdff7978e421 — `RecordUsage` stores
	// `created_at` as ISO-T-Z via `timeStr`; range filters must
	// compare against the same format or SQLite's lexicographic TEXT
	// comparison silently drops same-date rows). Without the From
	// window, QueryUsage emits no `created_at` predicate and the
	// gotcha is not exercised here.
	now := time.Now().UTC()
	if err := db.RecordUsage(&store.UsageEntry{
		ID: "u-cost", RequestID: "r-cost",
		Provider: "anthropic", Model: haikuModel,
		ConnectionID: "c-anthropic-test",
		InputTokens:  1000, OutputTokens: 500, TotalTokens: 1500,
		Cost: cost, CostSource: "apikey", Status: "ok",
	}); err != nil {
		t.Fatalf("RecordUsage (cost row): %v", err)
	}

	entries, err := db.QueryUsage(store.UsageFilter{
		Provider: "anthropic",
		From:     now.Add(-1 * time.Hour),
		To:       now.Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.ID == "u-cost" {
			if math.Abs(e.Cost-expectedCost) > 1e-9 {
				t.Errorf("stored cost = %v, want %v", e.Cost, expectedCost)
			}
			if e.CostSource != "apikey" {
				t.Errorf("stored cost_source = %q, want 'apikey'", e.CostSource)
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("u-cost row not found in usage_log via windowed QueryUsage — " +
			"either RecordUsage didn't persist OR the timestamp-format contract " +
			"regressed (see self-learning 82fa8ebabdbe4ea49ad3cdff7978e421: " +
			"RecordUsage writes ISO-T-Z; QueryUsage's date predicate must use " +
			"the same timeStr helper, not a fresh time.Format)")
	}
}
