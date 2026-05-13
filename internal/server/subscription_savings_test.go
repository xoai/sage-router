package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"sage-router/internal/catalog"
	"sage-router/internal/store"
)

// savingsTestCounter — distinct usage IDs across rapid seed calls.
// Same pattern as seedUsageCounter in the store tests (Windows clock
// granularity workaround per the [LRN:gotcha] memory).
var savingsTestCounter atomic.Uint64

// TestHandleUsageSummary_SubscriptionSavingsUsesCacheReadPrice — AC25b.
// Seeds a subscription usage row with cache_read_tokens > 0, sets
// CacheRead = 0.1 × Input via a user pricing override, fetches
// /api/usage/summary, asserts subscription_savings is LOWER than the
// "treat all input as fresh" baseline by exactly the cache discount.
//
// This is the end-to-end RC2 verification: without the M1.8b
// SubscriptionUsageGroup cache columns AND the M1.9b TokenBreakdown
// wiring, the assertion fails because cache_read_tokens would be
// silently treated as fresh input.
func TestHandleUsageSummary_SubscriptionSavingsUsesCacheReadPrice(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	// Seed one subscription row with input=1000, cache_read=500.
	// The store-level test (TestSubscriptionUsageGroups_IncludesCacheTokens)
	// already verified the aggregation; this test is the layer above —
	// proving the value flows through into summary.SubscriptionSavings.
	const (
		provider     = "anthropic"
		modelID      = "claude-sonnet-4-6"
		inputTokens  = 1000
		outputTokens = 0
		cacheRead    = 500
	)
	seedSubRowForSavings(t, st, provider, modelID, inputTokens, outputTokens, cacheRead, 0)

	// Override pricing so CacheRead = 0.1 × Input. The seed catalog
	// already has anthropic pricing; bumping it via the user override
	// path also exercises the source-precedence rules.
	cs := catalog.NewSQLiteStore(st.DB())
	if err := cs.UpsertPricing(context.Background(), provider, modelID, catalog.Pricing{
		Input:     10.0, // $10/1M
		Output:    50.0,
		CacheRead: 1.0, // = 0.1 × Input — the cache discount we want to observe
		Source:    catalog.SourceUser,
	}); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}
	cat.Invalidate() // force fresh read from store

	// Compute expected savings.
	//   Without cache pricing: (1000 + 500) × Input = 1500 × $10/1M = $0.0150
	//   With cache pricing:    1000 × $10/1M + 500 × $1/1M
	//                         = $0.0100 + $0.0005 = $0.0105
	// AC25b assertion: actual must be CLOSER to the with-cache value.
	const withCacheSavings = 0.0105
	const treatAllAsInputSavings = 0.0150

	// Drive the handler through the actual HTTP surface.
	req := httptest.NewRequest(http.MethodGet, "/api/usage/summary", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsageSummary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		SubscriptionSavings float64 `json:"subscription_savings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v\nbody=%s", err, rec.Body.String())
	}

	// Must match the with-cache figure within float epsilon.
	if delta := resp.SubscriptionSavings - withCacheSavings; delta < -1e-9 || delta > 1e-9 {
		t.Errorf("subscription_savings = %v, want %v (cache pricing applied)",
			resp.SubscriptionSavings, withCacheSavings)
	}
	// AC25b's "cache pricing actually reduces" check: the actual MUST
	// differ from the all-input baseline by exactly the cache discount.
	if resp.SubscriptionSavings >= treatAllAsInputSavings {
		t.Errorf("subscription_savings = %v ≥ %v (no cache discount applied)",
			resp.SubscriptionSavings, treatAllAsInputSavings)
	}
}

// newCatalogWiredStore returns a fresh store + Registry, with the
// catalog seeded from the static config constants. Mirrors what
// production bootstrap (M1.11) will do.
func newCatalogWiredStore(t *testing.T) (store.Store, catalog.Registry) {
	t.Helper()
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cs := catalog.NewSQLiteStore(st.DB())
	if err := catalog.SeedFromConstants(context.Background(), cs); err != nil {
		t.Fatalf("SeedFromConstants: %v", err)
	}
	if err := catalog.SeedProviderMeta(context.Background(), cs); err != nil {
		t.Fatalf("SeedProviderMeta: %v", err)
	}
	return st, catalog.NewRegistry(cs)
}

// seedSubRowForSavings — direct INSERT of a subscription-cost-source
// usage_log row with explicit cache token columns. We can't use
// store.RecordUsage because it doesn't expose cost_source override
// on a per-call basis from this package; raw INSERT is the test
// path that internal/store/subscription_usage_cache_test.go already
// uses.
func seedSubRowForSavings(t *testing.T, st store.Store, provider, model string,
	inputTokens, outputTokens, cacheRead, cacheWrite int,
) {
	t.Helper()
	id := "savings-" + t.Name() + "-" + strconv.FormatUint(savingsTestCounter.Add(1), 10)
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err := st.DB().Exec(
		`INSERT INTO usage_log (id, request_id, provider, model, connection_id, api_key_id,
			input_tokens, output_tokens, total_tokens,
			cache_read_tokens, cache_write_tokens,
			cost, latency_ms, status, created_at, cost_source)
		 VALUES (?, ?, ?, ?, 'c1', '', ?, ?, ?, ?, ?, 0, 100, 'ok', ?, 'subscription')`,
		id, id, provider, model,
		inputTokens, outputTokens, inputTokens+outputTokens, cacheRead, cacheWrite, now,
	)
	if err != nil {
		t.Fatalf("seedSubRowForSavings: %v", err)
	}
}
