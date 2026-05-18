package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sage-router/internal/store"
)

// keyTestCounter — distinct IDs across rapid seed calls. Same pattern
// as savingsTestCounter in subscription_savings_test.go (Windows
// clock-granularity workaround per memory `391bd38e699a…`).
var keyTestCounter atomic.Uint64

// newServerWithKeysStore returns a Server backed by a fresh in-memory
// store with migrations applied. Dependencies struct includes only
// Store — the handlers under test (handleListAPIKeys) don't reference
// Auth or other deps.
func newServerWithKeysStore(t *testing.T) (*Server, store.Store) {
	t.Helper()
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &Server{deps: Dependencies{Store: st}}, st
}

// seedKey inserts an api_keys row directly. Hash is derived
// deterministically from t.Name() + counter to avoid duplicate-key
// collisions on Windows where clock granularity is coarse (~15ms).
func seedKey(t *testing.T, st store.Store, name, routing string, budget float64) {
	t.Helper()
	n := keyTestCounter.Add(1)
	id := fmt.Sprintf("key-%s-%d", strings.ReplaceAll(t.Name(), "/", "_"), n)
	hash := fmt.Sprintf("hash-%s", id)
	prefix := "sk-sage-" + fmt.Sprintf("%04d", n)
	_, err := st.DB().Exec(
		`INSERT INTO api_keys (id, name, key_hash, prefix, budget_monthly, budget_hard_limit, allowed_models, rate_limit_rpm, routing_strategy, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, name, hash, prefix, budget, false, "*", 0, routing,
		time.Now().UTC().Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		t.Fatalf("seedKey: %v", err)
	}
}

// TestHandleListAPIKeys_LimitOffsetHonored — AC-C1, AC-C2.
// Seed 30 keys; query ?limit=10&offset=10; assert 10 items + total=30.
func TestHandleListAPIKeys_LimitOffsetHonored(t *testing.T) {
	s, st := newServerWithKeysStore(t)
	for i := 0; i < 30; i++ {
		seedKey(t, st, fmt.Sprintf("key-%02d", i), "", 0)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/keys?limit=10&offset=10", nil)
	rec := httptest.NewRecorder()
	s.handleListAPIKeys(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var page store.APIKeyPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 10 {
		t.Errorf("len(Items) = %d, want 10", len(page.Items))
	}
	if page.Total != 30 {
		t.Errorf("Total = %d, want 30", page.Total)
	}
	if page.Limit != 10 || page.Offset != 10 {
		t.Errorf("Limit/Offset = %d/%d, want 10/10", page.Limit, page.Offset)
	}
}

// TestHandleListAPIKeys_SearchFilters — AC-C3.
func TestHandleListAPIKeys_SearchFilters(t *testing.T) {
	s, st := newServerWithKeysStore(t)
	seedKey(t, st, "prod-key", "", 0)
	seedKey(t, st, "dev-key", "", 0)
	seedKey(t, st, "ml-team", "", 0)

	req := httptest.NewRequest(http.MethodGet, "/api/keys?search=prod", nil)
	rec := httptest.NewRecorder()
	s.handleListAPIKeys(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var page store.APIKeyPage
	json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Total != 1 {
		t.Errorf("Total = %d, want 1", page.Total)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "prod-key" {
		t.Errorf("returned wrong rows: %+v", page.Items)
		return
	}
}

// TestHandleListAPIKeys_SearchCaseFolding — AC-C3 + spec-review m5.
// SQLite default LIKE is ASCII-only case-insensitive; the
// LOWER(name) LIKE LOWER(?) pattern must work for mixed case.
func TestHandleListAPIKeys_SearchCaseFolding(t *testing.T) {
	s, st := newServerWithKeysStore(t)
	seedKey(t, st, "Production", "", 0)

	for _, q := range []string{"PRODUCTION", "production", "Production", "prod"} {
		req := httptest.NewRequest(http.MethodGet, "/api/keys?search="+q, nil)
		rec := httptest.NewRecorder()
		s.handleListAPIKeys(rec, req)
		var page store.APIKeyPage
		json.Unmarshal(rec.Body.Bytes(), &page)
		if page.Total != 1 {
			t.Errorf("search=%q: Total = %d, want 1", q, page.Total)
		}
	}
}

// TestHandleListAPIKeys_RoutingFilter — AC-C3 + R4 (routing-default mapping).
func TestHandleListAPIKeys_RoutingFilter(t *testing.T) {
	s, st := newServerWithKeysStore(t)
	seedKey(t, st, "a", "fast", 0)
	seedKey(t, st, "b", "balanced", 0)
	seedKey(t, st, "c", "", 0) // default (empty-strategy)

	cases := []struct {
		query    string
		wantTot  int
		wantName string
	}{
		{"routing=fast", 1, "a"},
		{"routing=balanced", 1, "b"},
		{"routing=default", 1, "c"},
		{"routing=cheap", 0, ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/keys?"+c.query, nil)
		rec := httptest.NewRecorder()
		s.handleListAPIKeys(rec, req)
		var page store.APIKeyPage
		json.Unmarshal(rec.Body.Bytes(), &page)
		if page.Total != c.wantTot {
			t.Errorf("query=%q: Total=%d, want %d", c.query, page.Total, c.wantTot)
		}
		if c.wantTot > 0 && len(page.Items) > 0 && page.Items[0].Name != c.wantName {
			t.Errorf("query=%q: name=%q, want %q", c.query, page.Items[0].Name, c.wantName)
		}
	}
}

// TestHandleListAPIKeys_HasBudgetFilter — AC-C3.
func TestHandleListAPIKeys_HasBudgetFilter(t *testing.T) {
	s, st := newServerWithKeysStore(t)
	seedKey(t, st, "capped", "", 100.0)
	seedKey(t, st, "uncapped", "", 0.0)

	req := httptest.NewRequest(http.MethodGet, "/api/keys?has_budget=true", nil)
	rec := httptest.NewRecorder()
	s.handleListAPIKeys(rec, req)
	var page store.APIKeyPage
	json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Total != 1 || len(page.Items) == 0 || page.Items[0].Name != "capped" {
		t.Errorf("has_budget=true: %+v, want capped", page.Items)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/keys?has_budget=false", nil)
	rec = httptest.NewRecorder()
	s.handleListAPIKeys(rec, req)
	json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Total != 1 || len(page.Items) == 0 || page.Items[0].Name != "uncapped" {
		t.Errorf("has_budget=false: %+v, want uncapped", page.Items)
	}
}

// TestHandleListAPIKeys_InvalidLimitReturns400 — AC-C1 + cycle 20260515
// envelope contract.
func TestHandleListAPIKeys_InvalidLimitReturns400(t *testing.T) {
	s, _ := newServerWithKeysStore(t)
	req := httptest.NewRequest(http.MethodGet, "/api/keys?limit=-1", nil)
	rec := httptest.NewRecorder()
	s.handleListAPIKeys(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	// Verify canonical nested envelope shape {error: {message: "..."}}.
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

// TestHandleListAPIKeys_SortHonored — AC-A2 post-review.
// Seed keys with predictable names + timestamps; query ?sort=name&dir=asc
// then dir=desc; verify ordering matches.
func TestHandleListAPIKeys_SortHonored(t *testing.T) {
	s, st := newServerWithKeysStore(t)
	// Insert in random order; sort should impose deterministic order.
	seedKey(t, st, "charlie", "", 0)
	seedKey(t, st, "alpha", "", 0)
	seedKey(t, st, "bravo", "", 0)

	cases := []struct {
		query   string
		want    []string
	}{
		{"sort=name&dir=asc", []string{"alpha", "bravo", "charlie"}},
		{"sort=name&dir=desc", []string{"charlie", "bravo", "alpha"}},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/keys?"+c.query, nil)
		rec := httptest.NewRecorder()
		s.handleListAPIKeys(rec, req)
		var page store.APIKeyPage
		json.Unmarshal(rec.Body.Bytes(), &page)
		got := make([]string, len(page.Items))
		for i, k := range page.Items {
			got[i] = k.Name
		}
		if !sliceEq(got, c.want) {
			t.Errorf("query=%q: got %v, want %v", c.query, got, c.want)
		}
	}
}

// TestHandleListAPIKeys_InvalidSortReturns400 — AC-A2 + SQL injection
// guard. Whitelist rejection is the ONLY barrier between user input and
// raw ORDER BY at sqlite.go — any value not in {name, created_at} must
// be rejected at the handler.
func TestHandleListAPIKeys_InvalidSortReturns400(t *testing.T) {
	s, _ := newServerWithKeysStore(t)
	cases := []string{
		"sort=key_hash",                                    // existing column, but not whitelisted
		"sort=name%3BDROP%20TABLE%20api_keys",              // URL-encoded injection attempt
		"sort=foo",
	}
	for _, q := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/keys?"+q, nil)
		rec := httptest.NewRecorder()
		s.handleListAPIKeys(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query=%q: status=%d, want 400", q, rec.Code)
		}
	}
}

// TestHandleListAPIKeys_InvalidDirReturns400 — AC-A2 whitelist.
func TestHandleListAPIKeys_InvalidDirReturns400(t *testing.T) {
	s, _ := newServerWithKeysStore(t)
	req := httptest.NewRequest(http.MethodGet, "/api/keys?sort=name&dir=random", nil)
	rec := httptest.NewRecorder()
	s.handleListAPIKeys(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

// sliceEq — small helper for ordered string-slice equality.
func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHandleListAPIKeys_ResponseShapeChange — R1/R8 contract pin.
// Memory `b51e91989eb2…` premise-drift discipline: this test fails if
// any future change reverts to the bare-array response.
func TestHandleListAPIKeys_ResponseShapeChange(t *testing.T) {
	s, _ := newServerWithKeysStore(t)
	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	rec := httptest.NewRecorder()
	s.handleListAPIKeys(rec, req)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode top-level: %v (body=%s)", err, rec.Body.String())
	}
	for _, key := range []string{"items", "total", "limit", "offset"} {
		if _, ok := top[key]; !ok {
			t.Errorf("missing envelope field %q; body=%s", key, rec.Body.String())
		}
	}
}
