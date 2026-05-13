package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sage-router/internal/store"
)

// Models Discovery M3.2 — AC25 verification.
//
// No new production code in this task. The point is to prove that
// M1.9b's rewire of `routes_api.go: handleGetUsageSummary` to call
// `catalog.Registry.EstimateCost` actually flows changes in the
// pricing catalog through to the `subscription_savings` field in
// `/api/usage/summary`, with no row-level mutation in `usage_log`.
//
// If this test fails, the bug is in M1.9b, not in M3. Per ADR-3 §F
// the contract is: subscription rows have cost=0; their savings are
// computed at query time via EstimateCost; a PUT pricing override
// flows through to the next read.

// TestSummary_SubscriptionSavingsReflectsLivePricing — AC25.
//
// Scenario:
//  1. Seed one subscription usage row for anthropic/claude-sonnet-4-6
//     with non-trivial token counts (so EstimateCost returns > 0).
//  2. Fetch /api/usage/summary; capture initial subscription_savings
//     (computed against the seed catalog).
//  3. PUT a user pricing override that doubles input + output prices.
//  4. Re-fetch /api/usage/summary; subscription_savings must now be
//     larger (the new pricing applies retroactively at query time).
//  5. Verify the underlying usage_log row's cost is STILL 0 — the
//     subscription contract is "no per-row cost; savings computed
//     against current pricing on read."
//
// The exact numerical contract: with input=1000, output=500 tokens,
// initial savings should equal `1000 * inSeed + 500 * outSeed` /
// 1_000_000. After the PUT doubles both prices, savings must double.
// The relative-doubling check is more robust to seed-price drift than
// pinning absolute USD values.
func TestSummary_SubscriptionSavingsReflectsLivePricing(t *testing.T) {
	srv, db, _ := newCatalogTestServer(t)

	// Seed one subscription row via RecordUsage (production write
	// path so the timestamp lands in the lookup window the summary
	// query will scan).
	const inputTokens = 1000
	const outputTokens = 500
	if err := db.RecordUsage(&store.UsageEntry{
		ID:           "sub-row-1",
		RequestID:    "req-sub-1",
		Provider:     "anthropic",
		Model:        "claude-sonnet-4-6",
		ConnectionID: "c1",
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Cost:         0, // subscription contract
		CostSource:   "subscription",
		Status:       "ok",
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	// First summary read — savings against seed catalog pricing.
	preSavings := fetchSubscriptionSavings(t, srv)
	if preSavings <= 0 {
		t.Fatalf("initial subscription_savings = %g, want > 0; seed pricing for claude-sonnet-4-6 must be non-zero",
			preSavings)
	}

	// PUT a user pricing override that doubles input + output prices.
	// The seed catalog has input=3, output=15 for claude-sonnet-4-6 —
	// set 6/30 so input+output savings are exactly doubled.
	//
	// Cache prices are present only to satisfy M2.10's strict-replace
	// PUT contract (all five fields required). The seeded usage row
	// has CacheReadTokens=0 and CacheWriteTokens=0, so the cache
	// price values don't influence the ratio. AC25b's cache-aware
	// path is independently covered by
	// `TestHandleUsageSummary_SubscriptionSavingsUsesCacheReadPrice`
	// in subscription_savings_test.go (M1.9b era).
	putBody := []byte(`{
		"input_price": 6.0,
		"output_price": 30.0,
		"cache_read_price": 0.6,
		"cache_write_price": 7.5,
		"thinking_price": 0
	}`)
	req := httptest.NewRequest(http.MethodPut,
		"/api/catalog/pricing/anthropic/claude-sonnet-4-6",
		bytes.NewReader(putBody))
	req.AddCookie(catalogTestSessionCookie(t, srv))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT pricing: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Second summary read — savings should now reflect the doubled
	// pricing. AC25's contract holds when the ratio is ~2.0 (the
	// M1.9b rewire flowed PUT→Registry.Invalidate→EstimateCost).
	postSavings := fetchSubscriptionSavings(t, srv)
	ratio := postSavings / preSavings
	if ratio < 1.99 || ratio > 2.01 {
		t.Errorf("subscription_savings did not double: pre=%g post=%g ratio=%g (want ~2.0)",
			preSavings, postSavings, ratio)
	}

	// Underlying usage_log row's cost is STILL 0 — the
	// subscription_savings change is computed-at-read-time, not a
	// row mutation. AC24's contract from M3.1 carries forward here.
	got, err := db.QueryUsage(store.UsageFilter{})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(got))
	}
	if got[0].Cost != 0 {
		t.Errorf("usage_log row cost = %g, want 0 (subscription rows never carry a cost; AC24)",
			got[0].Cost)
	}
	if got[0].CostSource != "subscription" {
		t.Errorf("cost_source = %q, want subscription (recompute path must not have touched it)", got[0].CostSource)
	}
}

// fetchSubscriptionSavings does GET /api/usage/summary with auth and
// returns the SubscriptionSavings field. Centralized so the two reads
// in the test are clearly comparable.
func fetchSubscriptionSavings(t *testing.T, srv *Server) float64 {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/usage/summary", nil)
	req.AddCookie(catalogTestSessionCookie(t, srv))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/usage/summary: status = %d, want 200; body: %s",
			rec.Code, rec.Body.String())
	}
	var summary store.UsageSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	return summary.SubscriptionSavings
}

// TestSummary_SubscriptionSavingsZeroWhenCatalogMissing — defensive
// path: if `s.deps.Catalog` is nil (test rig with partial wiring),
// handleGetUsageSummary falls back to savings=0 and doesn't 500.
// The M2.12 reviews flagged the importance of these fallbacks; this
// test pins the contract.
func TestSummary_SubscriptionSavingsZeroWhenCatalogMissing(t *testing.T) {
	srv, db, _ := newCatalogTestServer(t)
	srv.deps.Catalog = nil // partial wiring — emulates a test setup that didn't include the catalog

	if err := db.RecordUsage(&store.UsageEntry{
		ID:           "sub-row-1",
		RequestID:    "req-sub-1",
		Provider:     "anthropic",
		Model:        "claude-sonnet-4-6",
		ConnectionID: "c1",
		InputTokens:  1000,
		OutputTokens: 500,
		Cost:         0,
		CostSource:   "subscription",
		Status:       "ok",
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	got := fetchSubscriptionSavings(t, srv)
	if got != 0 {
		t.Errorf("savings = %g without catalog wired, want 0 (RC1 fallback)", got)
	}
}

