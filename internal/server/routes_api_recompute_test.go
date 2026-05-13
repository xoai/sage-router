package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sage-router/internal/catalog"
	"sage-router/internal/store"
)

// Models Discovery M3.1 — POST /api/catalog/recompute (AC23 + AC24).
//
// Per ADR-3 §Part F: an admin endpoint that re-applies *current*
// catalog pricing to historical apikey usage_log rows in a date range.
// Subscription rows are skipped — their cost is 0 in usage_log and
// `subscription_savings` is computed at query time in the summary
// endpoint, so live pricing already flows through automatically.
//
// Request shape: {from, to, dry_run}. Response shape:
// {rows_inspected, rows_updated, total_cost_delta_usd, subscription_note}.
//
// Contract bullets verified here:
//   - apikey rows whose stored cost differs from EstimateCost are updated
//   - cost_source remains unchanged (the recompute only touches cost)
//   - subscription rows are not touched, even if EstimateCost would
//     produce a non-zero number
//   - dry_run=true returns the deltas without persisting
//   - JWT is required (middleware path); a missing cookie returns 401

func newRecomputeTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, db, _ := newCatalogTestServer(t)
	return srv, db
}

// seedRecomputeUsage inserts a usage_log row via the production
// RecordUsage method. Note: RecordUsage stamps created_at with
// time.Now() and ignores any caller-provided value, so the recompute
// tests' range MUST contain time.Now() to find the rows. Project
// memory: Windows/WSL clock granularity ~15ms — fine for our
// generously-wide ±1h window.
func seedRecomputeUsage(t *testing.T, db store.Store, id string, e store.UsageEntry) {
	t.Helper()
	e.ID = id
	e.RequestID = "req-" + id
	if e.Status == "" {
		e.Status = "ok"
	}
	if err := db.RecordUsage(&e); err != nil {
		t.Fatalf("RecordUsage %s: %v", id, err)
	}
}

// recomputeFixture seeds three rows (two apikey + one subscription)
// and returns a (from, to) window that comfortably contains them.
// The window anchors on time.Now() because RecordUsage uses the wall
// clock for created_at and ignores entry.CreatedAt.
func recomputeFixture(t *testing.T, db store.Store) (from, to time.Time) {
	t.Helper()
	now := time.Now().UTC()
	from = now.Add(-1 * time.Hour)
	to = now.Add(1 * time.Hour)

	// apikey rows with stored cost that DOES NOT match what EstimateCost
	// would produce from current seed pricing. Setting a deliberately
	// stale cost so recompute has work to do.
	seedRecomputeUsage(t, db, "u1-apikey", store.UsageEntry{
		Provider:     "anthropic",
		Model:        "claude-sonnet-4-6",
		InputTokens:  1000,
		OutputTokens: 500,
		Cost:         99.99, // stale — recompute should overwrite
		CostSource:   "apikey",
	})
	seedRecomputeUsage(t, db, "u2-apikey", store.UsageEntry{
		Provider:     "anthropic",
		Model:        "claude-haiku-4-5-20251001",
		InputTokens:  2000,
		OutputTokens: 1000,
		Cost:         42.0, // stale
		CostSource:   "apikey",
	})

	// Subscription row — cost is 0 in production; recompute must NOT touch it
	// even though EstimateCost would yield non-zero.
	seedRecomputeUsage(t, db, "u3-sub", store.UsageEntry{
		Provider:     "anthropic",
		Model:        "claude-sonnet-4-6",
		InputTokens:  1000,
		OutputTokens: 500,
		Cost:         0,
		CostSource:   "subscription",
	})

	return from, to
}

// recomputeBody is the request shape — local to the test file because
// the handler's struct is unexported.
type recomputeBody struct {
	From   string `json:"from"`
	To     string `json:"to"`
	DryRun bool   `json:"dry_run"`
}

// recomputeResponse is the response shape from /api/catalog/recompute.
type recomputeResponse struct {
	RowsInspected    int     `json:"rows_inspected"`
	RowsUpdated      int     `json:"rows_updated"`
	TotalCostDelta   float64 `json:"total_cost_delta_usd"`
	SubscriptionNote string  `json:"subscription_note"`
}

// callRecompute issues the POST and returns the decoded response.
func callRecompute(t *testing.T, srv *Server, body recomputeBody, withAuth bool) (*httptest.ResponseRecorder, *recomputeResponse) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/catalog/recompute", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if withAuth {
		req.AddCookie(catalogTestSessionCookie(t, srv))
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var resp recomputeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return rec, &resp
}

// TestRecompute_ApiKeyRowsUpdated — AC23. The two apikey rows seeded
// with stale costs are updated to the current EstimateCost output.
// cost_source on each row is preserved (the recompute only touches
// the cost column). Subscription rows in the same window are NOT
// inspected (rows_inspected counts apikey only per ADR-3 §Part F).
func TestRecompute_ApiKeyRowsUpdated(t *testing.T) {
	srv, db := newRecomputeTestServer(t)
	from, to := recomputeFixture(t, db)

	rec, resp := callRecompute(t, srv, recomputeBody{
		From: from.Format(time.RFC3339),
		To:   to.Format(time.RFC3339),
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if resp.RowsInspected != 2 {
		t.Errorf("rows_inspected = %d, want 2 (the two apikey rows in window)", resp.RowsInspected)
	}
	if resp.RowsUpdated != 2 {
		t.Errorf("rows_updated = %d, want 2 (both apikey rows had stale costs)", resp.RowsUpdated)
	}

	// Both apikey rows were updated AND cost_source unchanged.
	entries, err := db.ListUsageInRange(context.Background(), from, to, "apikey")
	if err != nil {
		t.Fatalf("ListUsageInRange: %v", err)
	}
	for _, e := range entries {
		if e.Cost == 99.99 || e.Cost == 42.0 {
			t.Errorf("%s: cost not updated (still %g)", e.ID, e.Cost)
		}
		if e.CostSource != "apikey" {
			t.Errorf("%s: cost_source = %q after recompute, want %q (recompute must not touch cost_source)",
				e.ID, e.CostSource, "apikey")
		}
	}
}

// TestRecompute_SubscriptionRowsSkipped — AC24. Subscription rows
// have cost=0 by contract; even though EstimateCost would produce a
// non-zero number for the same token mix, the recompute does NOT
// touch them. The subscription_note in the response documents that
// subscription_savings is computed at /api/usage/summary read time.
func TestRecompute_SubscriptionRowsSkipped(t *testing.T) {
	srv, db := newRecomputeTestServer(t)
	from, to := recomputeFixture(t, db)

	rec, resp := callRecompute(t, srv, recomputeBody{
		From: from.Format(time.RFC3339),
		To:   to.Format(time.RFC3339),
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if resp.SubscriptionNote == "" {
		t.Errorf("subscription_note empty; should document that savings are query-time")
	}

	// Subscription row's cost is still 0.
	subs, err := db.ListUsageInRange(context.Background(), from, to, "subscription")
	if err != nil {
		t.Fatalf("ListUsageInRange: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subscription row, got %d", len(subs))
	}
	if subs[0].Cost != 0 {
		t.Errorf("subscription row cost = %g after recompute, want 0", subs[0].Cost)
	}
}

// TestRecompute_DryRunDoesNotPersist — dry_run=true returns the
// would-be deltas in the response shape but does NOT write to the DB.
// Operators use this to preview the impact of a pricing change before
// committing.
func TestRecompute_DryRunDoesNotPersist(t *testing.T) {
	srv, db := newRecomputeTestServer(t)
	from, to := recomputeFixture(t, db)

	rec, resp := callRecompute(t, srv, recomputeBody{
		From:   from.Format(time.RFC3339),
		To:     to.Format(time.RFC3339),
		DryRun: true,
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if resp.RowsUpdated != 2 {
		t.Errorf("dry_run: rows_updated = %d, want 2 (counts deltas without persisting)", resp.RowsUpdated)
	}

	// Crucially: the stored cost is still the stale value.
	entries, err := db.ListUsageInRange(context.Background(), from, to, "apikey")
	if err != nil {
		t.Fatalf("ListUsageInRange: %v", err)
	}
	for _, e := range entries {
		if e.ID == "u1-apikey" && e.Cost != 99.99 {
			t.Errorf("dry_run wrote to disk: u1 cost = %g, want 99.99 preserved", e.Cost)
		}
		if e.ID == "u2-apikey" && e.Cost != 42.0 {
			t.Errorf("dry_run wrote to disk: u2 cost = %g, want 42.0 preserved", e.Cost)
		}
	}
}

// TestRecompute_Rejects400OnBadInput — negative-path coverage for
// parseRecomputeTime and the from/to ordering check (M3.1 review
// MAJOR-1). The handler has 4 distinct 400 paths; this table-driven
// test pins each so a future maintainer who tweaks the parser sees
// every contract violation flagged at once.
func TestRecompute_Rejects400OnBadInput(t *testing.T) {
	srv, _ := newRecomputeTestServer(t)

	cases := []struct {
		name      string
		rawBody   []byte // when set, sent verbatim (overrides body field)
		body      recomputeBody
		expectSub string // substring that must appear in the 400 body
	}{
		{
			name:      "MalformedJSONBody",
			rawBody:   []byte(`{not even close to json`),
			expectSub: "invalid request body",
		},
		{
			name:      "MalformedFrom",
			body:      recomputeBody{From: "not-a-date", To: "2026-05-12T00:00:00Z"},
			expectSub: "from:",
		},
		{
			name:      "MalformedTo",
			body:      recomputeBody{From: "2026-05-01T00:00:00Z", To: "garbage"},
			expectSub: "to:",
		},
		{
			name:      "FromEqualsTo",
			body:      recomputeBody{From: "2026-05-01T00:00:00Z", To: "2026-05-01T00:00:00Z"},
			expectSub: "from must be before to",
		},
		{
			name:      "FromAfterTo",
			body:      recomputeBody{From: "2026-05-12T00:00:00Z", To: "2026-05-01T00:00:00Z"},
			expectSub: "from must be before to",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var body []byte
			if c.rawBody != nil {
				body = c.rawBody
			} else {
				b, err := json.Marshal(c.body)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				body = b
			}
			req := httptest.NewRequest(http.MethodPost, "/api/catalog/recompute", bytes.NewReader(body))
			req.AddCookie(catalogTestSessionCookie(t, srv))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400; body: %s", c.name, rec.Code, rec.Body.String())
			}
			if c.expectSub != "" && !strings.Contains(rec.Body.String(), c.expectSub) {
				t.Errorf("%s: 400 body should mention %q; got: %s", c.name, c.expectSub, rec.Body.String())
			}
		})
	}
}

// TestRecompute_RequiresAuth — admin endpoint protection. A request
// without a JWT cookie is rejected at the middleware layer with 401.
func TestRecompute_RequiresAuth(t *testing.T) {
	srv, _ := newRecomputeTestServer(t)

	rec, _ := callRecompute(t, srv, recomputeBody{
		From: "2026-05-01T00:00:00Z",
		To:   "2026-05-12T00:00:00Z",
	}, false)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated recompute: status = %d, want 401", rec.Code)
	}
}

// TestRecompute_NoStaleRowsNoOp — a window where every apikey row's
// stored cost already matches EstimateCost returns 0 rows_updated.
// The endpoint is safe to invoke repeatedly without side effects on
// already-correct rows.
func TestRecompute_NoStaleRowsNoOp(t *testing.T) {
	srv, db := newRecomputeTestServer(t)
	from, to := recomputeFixture(t, db)

	// Run once to bring everything current.
	rec, _ := callRecompute(t, srv, recomputeBody{
		From: from.Format(time.RFC3339),
		To:   to.Format(time.RFC3339),
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("first recompute: status = %d", rec.Code)
	}

	// Second run should find nothing to update.
	_, resp := callRecompute(t, srv, recomputeBody{
		From: from.Format(time.RFC3339),
		To:   to.Format(time.RFC3339),
	}, true)
	if resp.RowsUpdated != 0 {
		t.Errorf("second recompute: rows_updated = %d, want 0 (already current)", resp.RowsUpdated)
	}
	if resp.RowsInspected != 2 {
		t.Errorf("rows_inspected = %d, want 2 (still inspects, doesn't write)", resp.RowsInspected)
	}
}

// Compile-time guard: tests use catalog package to seed model rows
// (UpsertModel is called via newCatalogTestServer). Keep the import.
var _ = catalog.Model{}
