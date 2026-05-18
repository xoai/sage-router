package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"sage-router/internal/store"
)

// timeframeTestCounter — distinct usage IDs across rapid seed calls.
// Same pattern as savingsTestCounter (Windows clock granularity
// workaround per memory `391bd38e699a…`).
var timeframeTestCounter atomic.Uint64

// mustInsertAt — direct INSERT of a usage_log row at a specific
// timestamp + cost_source. Used by the timeframe tests because
// store.RecordUsage always uses time.Now() (sqlite.go:716-737) so
// can't seed historical rows. Matches the seedSubRowForSavings
// pattern at subscription_savings_test.go:128.
func mustInsertAt(t *testing.T, st store.Store, costSource, provider, model, createdAt string, inputTokens, outputTokens int) {
	t.Helper()
	id := "timeframe-" + t.Name() + "-" + strconv.FormatUint(timeframeTestCounter.Add(1), 10)
	// usage_log.created_at format matches what timeStr produces in
	// sqlite.go's RecordUsage (sqlite.go:717: "2006-01-02 15:04:05") —
	// but buildUsageFilter compares against the RFC3339-Z format string
	// (queries.go:54). The two formats lexicographically order
	// identically for UTC times because the date prefix is the same and
	// SQLite TEXT compares left-to-right. Storing in RFC3339-Z directly
	// lets the From/To filter work without conversion.
	_, err := st.DB().Exec(
		`INSERT INTO usage_log (id, request_id, provider, model, connection_id, api_key_id,
			input_tokens, output_tokens, total_tokens,
			cache_read_tokens, cache_write_tokens,
			cost, latency_ms, status, created_at, cost_source)
		 VALUES (?, ?, ?, ?, 'c1', '', ?, ?, ?, 0, 0, 0, 100, 'ok', ?, ?)`,
		id, id, provider, model,
		inputTokens, outputTokens, inputTokens+outputTokens,
		createdAt, costSource,
	)
	if err != nil {
		t.Fatalf("mustInsertAt: %v", err)
	}
}

// TestHandleGetUsage_FromToHonored — AC-C3 + AC-X2 timeframe-positive.
// Seed 3 rows at known timestamps; query a window that includes only
// the middle one; assert exactly that row is returned.
func TestHandleGetUsage_FromToHonored(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	mustInsertAt(t, st, "apikey", "openai", "gpt-4.1-nano", "2026-05-10T00:00:00Z", 100, 50)
	mustInsertAt(t, st, "apikey", "openai", "gpt-4.1-nano", "2026-05-13T00:00:00Z", 100, 50)
	mustInsertAt(t, st, "subscription", "anthropic", "claude-haiku-4-5-20251001", "2026-05-15T00:00:00Z", 200, 50)

	req := httptest.NewRequest(http.MethodGet,
		"/api/usage?from=2026-05-12T00:00:00Z&to=2026-05-14T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []store.UsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1; body=%s", len(got), rec.Body.String())
	}
	if got[0].Provider != "openai" || got[0].Model != "gpt-4.1-nano" {
		t.Errorf("returned wrong row: provider=%q model=%q", got[0].Provider, got[0].Model)
	}
}

// TestHandleGetUsage_MalformedFromReturns400 — AC-C4 envelope.
func TestHandleGetUsage_MalformedFromReturns400(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	req := httptest.NewRequest(http.MethodGet, "/api/usage?from=not-a-date", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsage(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "must be RFC3339") {
		t.Errorf("expected 'must be RFC3339' in error, got: %s", rec.Body.String())
	}
	// Verify the canonical envelope shape: {"error":{"message":"..."}}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Message == "" {
		t.Errorf("envelope error.message empty; body=%s", rec.Body.String())
	}
}

// TestHandleGetUsage_NoTimezoneRejected — AC-C4 strict RFC3339.
// Naive "2026-05-15T00:00:00" without Z or ±HH:MM must return 400.
// Plan-review M3: the dashboard's toISOString() always emits Z, so the
// strict-form contract is enforceable.
func TestHandleGetUsage_NoTimezoneRejected(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	req := httptest.NewRequest(http.MethodGet, "/api/usage?from=2026-05-15T00:00:00", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsage(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (RFC3339 requires timezone); body=%s",
			rec.Code, rec.Body.String())
	}
}

// TestHandleGetUsage_TimezoneNormalized — R2 mitigation.
// A +07:00 query for a "Vietnam day" must match against UTC created_at.
// queries.go:54 calls f.From.UTC() — this test pins that behavior.
func TestHandleGetUsage_TimezoneNormalized(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	// Row stored at UTC midnight on 2026-05-15.
	mustInsertAt(t, st, "apikey", "openai", "gpt-4.1-nano", "2026-05-15T00:00:00Z", 100, 50)

	// Query window: 2026-05-14 17:00 +07 == 2026-05-14 10:00 UTC (before)
	//          to:   2026-05-15 17:00 +07 == 2026-05-15 10:00 UTC (after)
	// The row (2026-05-15 00:00 UTC) falls inside.
	// URL-encode the + as %2B.
	url := "/api/usage?from=2026-05-14T17:00:00%2B07:00&to=2026-05-15T17:00:00%2B07:00"
	req := httptest.NewRequest(http.MethodGet, url, nil)
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
		t.Fatalf("len(got) = %d, want 1 (UTC normalization regression?); body=%s",
			len(got), rec.Body.String())
	}
}

// TestHandleGetUsageSummary_FromToHonored — AC-C3 + AC-C6.
// Summary totals must filter on the from/to window, including
// subscription_savings (verified because SubscriptionUsageGroups uses
// the same buildUsageFilter — sqlite.go:819).
func TestHandleGetUsageSummary_FromToHonored(t *testing.T) {
	st, cat := newCatalogWiredStore(t)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	// 3 rows, only the middle one in window.
	mustInsertAt(t, st, "apikey", "openai", "gpt-4.1-nano", "2026-05-10T00:00:00Z", 100, 50)
	mustInsertAt(t, st, "apikey", "openai", "gpt-4.1-nano", "2026-05-13T00:00:00Z", 200, 100)
	mustInsertAt(t, st, "apikey", "openai", "gpt-4.1-nano", "2026-05-15T00:00:00Z", 100, 50)

	req := httptest.NewRequest(http.MethodGet,
		"/api/usage/summary?from=2026-05-12T00:00:00Z&to=2026-05-14T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	s.handleGetUsageSummary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var summary store.UsageSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if summary.TotalRequests != 1 {
		t.Errorf("TotalRequests = %d, want 1 (window filter not applied?)", summary.TotalRequests)
	}
	if summary.TotalTokens != 300 {
		t.Errorf("TotalTokens = %d, want 300", summary.TotalTokens)
	}
}
