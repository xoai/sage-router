package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"sage-router/internal/store"
)

// putPricing issues an authenticated PUT to /api/catalog/pricing for
// anthropic/claude-sonnet-4-6 with the given input+output prices.
// Cache + thinking fields are zero — they're required by the
// strict-replace PUT contract (M2.10) but the seeded usage row has no
// cache or thinking tokens, so they don't influence the savings ratio.
func putPricing(t *testing.T, srv *Server, inputPrice, outputPrice float64) {
	t.Helper()
	body := fmt.Sprintf(`{
		"input_price": %v,
		"output_price": %v,
		"cache_read_price": 0,
		"cache_write_price": 0,
		"thinking_price": 0
	}`, inputPrice, outputPrice)
	req := httptest.NewRequest(http.MethodPut,
		"/api/catalog/pricing/anthropic/claude-sonnet-4-6",
		bytes.NewReader([]byte(body)))
	req.AddCookie(catalogTestSessionCookie(t, srv))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT pricing (%v/%v): status = %d, want 200; body: %s",
			inputPrice, outputPrice, rec.Code, rec.Body.String())
	}
}

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
//  2. PUT a baseline pricing override (1.0/1.0) so the test's
//     reference point is independent of whatever the seed catalog
//     happens to contain. Capture savings under the baseline.
//  3. PUT a doubled override (2.0/2.0). Capture savings again.
//  4. Assert the ratio is ~2.0.
//  5. Verify the underlying usage_log row's cost is STILL 0.
//
// Carryover #46 — the original version PUT 6.0/30.0 once and assumed
// the seed had 3.0/15.0, which meant a seed-price drift in production
// would fail the test for a non-bug reason. The baseline-then-double
// pattern below decouples the assertion from seed values entirely.
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

	// Establish a known baseline pricing via PUT. This replaces the
	// "compare against seed values" pattern with "compare against an
	// explicitly-set baseline" — seed-drift-resistant.
	putPricing(t, srv, 1.0, 1.0)
	baselineSavings := fetchSubscriptionSavings(t, srv)
	if baselineSavings <= 0 {
		t.Fatalf("baseline subscription_savings = %g, want > 0; PUT must persist + Registry.Invalidate must propagate",
			baselineSavings)
	}

	// Double the prices via a second PUT.
	putPricing(t, srv, 2.0, 2.0)

	// Second summary read — savings should now reflect the doubled
	// pricing. AC25's contract holds when the ratio is ~2.0 (the
	// M1.9b rewire flowed PUT→Registry.Invalidate→EstimateCost).
	postSavings := fetchSubscriptionSavings(t, srv)
	ratio := postSavings / baselineSavings
	if ratio < 1.99 || ratio > 2.01 {
		t.Errorf("subscription_savings did not double under 2x baseline override: baseline=%g post=%g ratio=%g (want ~2.0)",
			baselineSavings, postSavings, ratio)
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
	// Carryover #48 — use the dedicated helper that constructs deps
	// with Catalog: nil from the start, instead of mutating
	// srv.deps.Catalog after construction. The latter fights the
	// constructor-injection design and would mask a regression where
	// the constructor itself started rejecting nil Catalog.
	srv, db := newCatalogTestServerWithoutCatalog(t)

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

