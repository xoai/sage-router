package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"sage-router/internal/store"
)

// Cycle 20260517-usage-page-filters T5 — pin the multi-key filter
// behavior on /api/usage and /api/usage/summary against the real
// route handlers. Tests verify the HTTP-level chain:
//
//   query string (?api_key_id=A&api_key_id=B)
//     → r.URL.Query()["api_key_id"]  (net/http []string)
//     → UsageFilter.APIKeyIDs
//     → buildUsageFilter IN clause
//     → DB returns subset
//
// Memory anchor [ba734a82]: spec-review demands route binding pinning,
// not just handler citation. Each test uses server.go's actual
// mux dispatch (via s.mux not direct call) where practical.

var multikeyCounter atomic.Uint64

// mustInsertWithKey — seeds a usage_log row with explicit api_key_id.
// The existing mustInsertAt at routes_api_usage_timeframe_test.go:21
// hardcodes empty string; we need explicit key values here.
func mustInsertWithKey(t *testing.T, st store.Store, apiKeyID, provider, model, costSource, createdAt string, inputTokens, outputTokens int) {
	t.Helper()
	id := "mk-" + t.Name() + "-" + strconv.FormatUint(multikeyCounter.Add(1), 10)
	_, err := st.DB().Exec(
		`INSERT INTO usage_log (id, request_id, provider, model, connection_id, api_key_id,
			input_tokens, output_tokens, total_tokens,
			cache_read_tokens, cache_write_tokens,
			cost, latency_ms, status, created_at, cost_source)
		 VALUES (?, ?, ?, ?, 'c1', ?, ?, ?, ?, 0, 0, 0, 100, 'ok', ?, ?)`,
		id, id, provider, model, apiKeyID,
		inputTokens, outputTokens, inputTokens+outputTokens,
		createdAt, costSource,
	)
	if err != nil {
		t.Fatalf("mustInsertWithKey: %v", err)
	}
}

// TestHandleGetUsage_MultiKeyFilter — happy path: ?api_key_id=A&api_key_id=B
// returns only A and B rows; C is excluded.
func TestHandleGetUsage_MultiKeyFilter(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	mustInsertWithKey(t, st, "key-A", "openai", "gpt-4.1-nano", "apikey", "2026-05-10T00:00:00Z", 100, 50)
	mustInsertWithKey(t, st, "key-B", "openai", "gpt-4.1-nano", "apikey", "2026-05-11T00:00:00Z", 100, 50)
	mustInsertWithKey(t, st, "key-C", "openai", "gpt-4.1-nano", "apikey", "2026-05-12T00:00:00Z", 100, 50)

	req := httptest.NewRequest(http.MethodGet, "/api/usage?api_key_id=key-A&api_key_id=key-B", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []store.UsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2; body=%s", len(got), rec.Body.String())
	}
	for _, r := range got {
		if r.APIKeyID == "key-C" {
			t.Errorf("key-C row leaked into multi-key filter response: %+v", r)
		}
		if r.APIKeyID != "key-A" && r.APIKeyID != "key-B" {
			t.Errorf("unexpected api_key_id %q in response", r.APIKeyID)
		}
	}
}

// TestHandleGetUsage_SingleKey_Backcompat — old single-key URL still works.
// Spec AC8: backward compat is non-negotiable.
func TestHandleGetUsage_SingleKey_Backcompat(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	mustInsertWithKey(t, st, "key-A", "openai", "gpt-4.1-nano", "apikey", "2026-05-10T00:00:00Z", 100, 50)
	mustInsertWithKey(t, st, "key-B", "openai", "gpt-4.1-nano", "apikey", "2026-05-11T00:00:00Z", 100, 50)

	req := httptest.NewRequest(http.MethodGet, "/api/usage?api_key_id=key-A", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []store.UsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].APIKeyID != "key-A" {
		t.Fatalf("want 1 row for key-A; got len=%d body=%s", len(got), rec.Body.String())
	}
}

// TestHandleGetUsage_NoKeyFilter — absent api_key_id param returns all rows.
// Pins R4: r.URL.Query()["api_key_id"] returns nil when absent, and
// len(nil) == 0 so no filter is applied.
func TestHandleGetUsage_NoKeyFilter(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	mustInsertWithKey(t, st, "key-A", "openai", "gpt-4.1-nano", "apikey", "2026-05-10T00:00:00Z", 100, 50)
	mustInsertWithKey(t, st, "key-B", "openai", "gpt-4.1-nano", "apikey", "2026-05-11T00:00:00Z", 100, 50)
	mustInsertWithKey(t, st, "key-C", "openai", "gpt-4.1-nano", "apikey", "2026-05-12T00:00:00Z", 100, 50)

	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []store.UsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want all 3 rows; got len=%d", len(got))
	}
}

// TestHandleGetUsageSummary_MultiKey — summary follows the filter
// (spec AC9c reversal of MINOR-3). When user filters by 2 keys, the
// summary totals reflect only those 2 keys' rows.
func TestHandleGetUsageSummary_MultiKey(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	mustInsertWithKey(t, st, "key-A", "openai", "gpt-4.1-nano", "apikey", "2026-05-10T00:00:00Z", 100, 50)
	mustInsertWithKey(t, st, "key-B", "openai", "gpt-4.1-nano", "apikey", "2026-05-11T00:00:00Z", 100, 50)
	mustInsertWithKey(t, st, "key-C", "openai", "gpt-4.1-nano", "apikey", "2026-05-12T00:00:00Z", 1000, 500)

	req := httptest.NewRequest(http.MethodGet, "/api/usage/summary?api_key_id=key-A&api_key_id=key-B", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsageSummary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got store.UsageSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Only key-A (150 total) + key-B (150 total) = 300 tokens.
	// key-C's 1500 must be excluded.
	if got.TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2", got.TotalRequests)
	}
	if got.TotalTokens != 300 {
		t.Errorf("TotalTokens = %d, want 300 (key-C must be excluded)", got.TotalTokens)
	}
}

// TestHandleGetUsageSummary_SingleKey_Backcompat — AC8 counterpart
// for the summary endpoint. Old single-key URL ?api_key_id=X must
// continue to filter the summary correctly (the summary follows the
// filter per AC9c, so this also pins selection-scoping for size=1).
func TestHandleGetUsageSummary_SingleKey_Backcompat(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	mustInsertWithKey(t, st, "key-A", "openai", "gpt-4.1-nano", "apikey", "2026-05-10T00:00:00Z", 100, 50)
	mustInsertWithKey(t, st, "key-B", "openai", "gpt-4.1-nano", "apikey", "2026-05-11T00:00:00Z", 1000, 500)

	req := httptest.NewRequest(http.MethodGet, "/api/usage/summary?api_key_id=key-A", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsageSummary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got store.UsageSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalRequests != 1 {
		t.Errorf("TotalRequests = %d, want 1 (only key-A)", got.TotalRequests)
	}
	if got.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150 (key-B excluded)", got.TotalTokens)
	}
}

// TestHandleGetUsageSummary_NoKeyFilter — without api_key_id param,
// summary covers all rows (whole-system view, today's behavior preserved
// in the no-filter case).
func TestHandleGetUsageSummary_NoKeyFilter(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	mustInsertWithKey(t, st, "key-A", "openai", "gpt-4.1-nano", "apikey", "2026-05-10T00:00:00Z", 100, 50)
	mustInsertWithKey(t, st, "key-B", "openai", "gpt-4.1-nano", "apikey", "2026-05-11T00:00:00Z", 100, 50)
	mustInsertWithKey(t, st, "key-C", "openai", "gpt-4.1-nano", "apikey", "2026-05-12T00:00:00Z", 1000, 500)

	req := httptest.NewRequest(http.MethodGet, "/api/usage/summary", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsageSummary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got store.UsageSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalRequests != 3 {
		t.Errorf("TotalRequests = %d, want 3 (all keys)", got.TotalRequests)
	}
	if got.TotalTokens != 1800 {
		t.Errorf("TotalTokens = %d, want 1800 (150+150+1500)", got.TotalTokens)
	}
}
