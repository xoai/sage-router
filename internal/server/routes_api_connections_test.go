package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"sage-router/internal/provider"
	"sage-router/internal/store"
)

// M2 T10 — /api/connections facet projection (AC13).
//
// handleListConnections projects the three health facets (breaker/auth/
// lifecycle) AND a derived legacy `runtime_state` string, so a client reading
// the old `state`/`runtime_state` fields keeps working while the dashboard's
// own migration to the facet fields is deferred (spec §8).

// listConnections fires handleListConnections and decodes the JSON array.
func listConnections(t *testing.T, srv *Server) []map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/connections", nil)
	rec := httptest.NewRecorder()
	srv.handleListConnections(rec, req)
	if rec.Code != 200 {
		t.Fatalf("handleListConnections: status %d: %s", rec.Code, rec.Body.String())
	}
	var resp []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal /api/connections response: %v", err)
	}
	return resp
}

// AC13 — the projection emits breaker/auth/lifecycle and a derived legacy
// runtime_state; the DB-persisted `state` field is untouched.
func TestHandleListConnections_FacetProjection(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(pc *provider.Connection)
		breaker   string
		auth      string
		lifecycle string
		runtime   string // derived legacy runtime_state
	}{
		{
			name:    "fresh connection — closed/valid/idle",
			setup:   func(pc *provider.Connection) {},
			breaker: "closed", auth: "valid", lifecycle: "idle", runtime: "idle",
		},
		{
			name:    "rate limited — breaker open, runtime_state cooldown",
			setup:   func(pc *provider.Connection) { _ = pc.OpenBreaker(provider.FailureRateLimit, 0, "") },
			breaker: "open", auth: "valid", lifecycle: "idle", runtime: "cooldown",
		},
		{
			// The breaker-open split: a transient (non-rate-limit) failure
			// derives the legacy "errored", not "cooldown" — preserving the
			// cooldown/errored distinction the legacy connection state carried.
			name:    "transient failure — breaker open, runtime_state errored",
			setup:   func(pc *provider.Connection) { _ = pc.OpenBreaker(provider.FailureTransient, 0, "") },
			breaker: "open", auth: "valid", lifecycle: "idle", runtime: "errored",
		},
		{
			name: "auth expired — runtime_state auth_expired",
			setup: func(pc *provider.Connection) {
				pc.SetFacetsForTest(provider.BreakerClosed, provider.AuthExpired, provider.LifecycleIdle)
			},
			breaker: "closed", auth: "expired", lifecycle: "idle", runtime: "auth_expired",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, db := setupTestServer(t, nil)
			connID := addConnection(t, srv, db, "openai", "primary", "apikey")
			pc := srv.deps.ProviderSelector.ConnectionByID(connID)
			if pc == nil {
				t.Fatal("connection not registered in the selector")
			}
			tc.setup(pc)

			resp := listConnections(t, srv)
			if len(resp) != 1 {
				t.Fatalf("expected 1 connection, got %d", len(resp))
			}
			conn := resp[0]

			if got := conn["breaker"]; got != tc.breaker {
				t.Errorf("breaker = %v, want %q", got, tc.breaker)
			}
			if got := conn["auth"]; got != tc.auth {
				t.Errorf("auth = %v, want %q", got, tc.auth)
			}
			if got := conn["lifecycle"]; got != tc.lifecycle {
				t.Errorf("lifecycle = %v, want %q", got, tc.lifecycle)
			}
			// The derived legacy runtime_state — what an un-migrated dashboard reads.
			if got := conn["runtime_state"]; got != tc.runtime {
				t.Errorf("runtime_state = %v, want %q", got, tc.runtime)
			}
			// The DB-persisted `state` is independent of the in-memory facets
			// and stays at its persisted value (AC13 — old field still works).
			if got := conn["state"]; got != "idle" {
				t.Errorf("DB state = %v, want idle (persisted, untouched by runtime facets)", got)
			}
		})
	}
}

// AC13 — the facet fields use omitempty: an unregistered connection (a DB row
// not yet in the Selector) renders without them, exactly as before M2.
func TestHandleListConnections_FacetsOmittedWhenUnregistered(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	// Insert directly — addConnection would also register it in the Selector.
	if err := db.CreateConnection(&store.Connection{
		ID: "orphan-facet-1", Provider: "openai", Name: "orphan",
		AuthType: "apikey", APIKey: "k", State: "idle",
	}); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	resp := listConnections(t, srv)
	if len(resp) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(resp))
	}
	for _, field := range []string{"breaker", "auth", "lifecycle", "runtime_state"} {
		if _, ok := resp[0][field]; ok {
			t.Errorf("%s should be omitted for an unregistered connection, got %v", field, resp[0][field])
		}
	}
	if got := resp[0]["state"]; got != "idle" {
		t.Errorf("DB state = %v, want idle", got)
	}
}

// legacyRuntimeState is a pure function — pin its facet→legacy-string mapping
// directly, including the breaker-open failure-kind split.
func TestLegacyRuntimeState(t *testing.T) {
	cases := []struct {
		name      string
		breaker   provider.BreakerState
		auth      provider.AuthState
		lifecycle provider.LifecycleState
		failure   provider.FailureKind
		want      string
	}{
		{"closed valid idle", provider.BreakerClosed, provider.AuthValid, provider.LifecycleIdle, "", "idle"},
		{"closed valid active", provider.BreakerClosed, provider.AuthValid, provider.LifecycleActive, "", "active"},
		{"disabled wins over all", provider.BreakerOpen, provider.AuthExpired, provider.LifecycleDisabled, provider.FailureRateLimit, "disabled"},
		{"refreshing wins over breaker", provider.BreakerOpen, provider.AuthRefreshing, provider.LifecycleIdle, provider.FailureTransient, "refreshing"},
		{"auth expired wins over breaker", provider.BreakerOpen, provider.AuthExpired, provider.LifecycleIdle, provider.FailureTransient, "auth_expired"},
		{"breaker open rate_limit -> cooldown", provider.BreakerOpen, provider.AuthValid, provider.LifecycleIdle, provider.FailureRateLimit, "cooldown"},
		{"breaker open transient -> errored", provider.BreakerOpen, provider.AuthValid, provider.LifecycleIdle, provider.FailureTransient, "errored"},
		{"breaker half_open transient -> errored", provider.BreakerHalfOpen, provider.AuthValid, provider.LifecycleIdle, provider.FailureTransient, "errored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyRuntimeState(tc.breaker, tc.auth, tc.lifecycle, tc.failure); got != tc.want {
				t.Errorf("legacyRuntimeState(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}
