package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sage-router/internal/catalog"
	"sage-router/internal/store"
)

// TestHandleGetUsage_EnrichesEstimatedAPICost — AC-B3.
// handleGetUsage must populate UsageEntry.EstimatedAPICost on every
// returned row via Catalog.EstimateCost. Mirrors the existing
// query-time pattern at routes_api.go:843-861 (subscription_savings).
func TestHandleGetUsage_EnrichesEstimatedAPICost(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	// One subscription row + one apikey row, both with non-trivial tokens
	// so EstimateCost returns > 0.
	seedSubRowForSavings(t, st, "anthropic", "claude-haiku-4-5-20251001", 1000, 200, 0, 0)
	mustInsertAt(t, st, "apikey", "anthropic", "claude-haiku-4-5-20251001", "2026-05-15T00:00:00Z", 1000, 200)

	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got []store.UsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; body=%s", len(got), rec.Body.String())
	}
	for _, e := range got {
		if e.EstimatedAPICost <= 0 {
			t.Errorf("row provider=%q model=%q cost_source=%q: EstimatedAPICost = %f, want > 0",
				e.Provider, e.Model, e.CostSource, e.EstimatedAPICost)
		}
	}
}

// TestHandleGetUsage_EstimatedAPICostEqualsPerRowEstimate — AC-B3 + R3.
// Plan-review M2: do NOT compare to summary.subscription_savings
// (different aggregation order). Compare directly to what
// Catalog.EstimateCost returns for the same per-row tokens via the
// same code path the handler uses.
func TestHandleGetUsage_EstimatedAPICostEqualsPerRowEstimate(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	const (
		provider = "anthropic"
		modelID  = "claude-haiku-4-5-20251001"
		inTok    = 1000
		outTok   = 200
	)
	seedSubRowForSavings(t, st, provider, modelID, inTok, outTok, 0, 0)

	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got []store.UsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}

	// Compute expected via the SAME Catalog.EstimateCost call the handler
	// uses (per AC-B3). Equality, not within-epsilon-of-summary.
	expected := cat.EstimateCost(provider, modelID, catalog.TokenBreakdown{
		Input:  inTok,
		Output: outTok,
	})
	if got[0].EstimatedAPICost != expected {
		t.Errorf("EstimatedAPICost = %v, want %v (same-path equality)",
			got[0].EstimatedAPICost, expected)
	}
	// Sanity: expected itself must be > 0 for the seed pricing (else the
	// catalog didn't load and the assertion is degenerate).
	if expected <= 0 {
		t.Fatalf("expected EstimateCost = %v ≤ 0 — catalog not wired correctly", expected)
	}
}
