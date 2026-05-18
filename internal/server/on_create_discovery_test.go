package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/catalog"
	"sage-router/internal/provider"
	"sage-router/internal/store"
)

// testAuthAdapter is the minimum surface auth.AuthStore needs to read
// connection rows in tests. Mirrors cmd/sage-router/auth_wire.go's
// adapter but lives in the server-test package since the cmd adapter
// can't be imported here.
type testAuthAdapter struct {
	s store.Store
}

func (a *testAuthAdapter) GetConnection(id string) (*auth.ConnRow, error) {
	c, err := a.s.GetConnection(id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	return &auth.ConnRow{
		ID:           c.ID,
		Provider:     c.Provider,
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		ExpiresAt:    c.ExpiresAt,
		ProviderData: []byte(c.ProviderData),
	}, nil
}

func (a *testAuthAdapter) UpdateConnection(id string, updates map[string]any) error {
	return a.s.UpdateConnection(id, updates)
}

func (a *testAuthAdapter) BumpConnectionRefreshFailures(id string) (int, error) {
	return a.s.BumpConnectionRefreshFailures(id)
}

func (a *testAuthAdapter) GetConnectionRefreshFailures(id string) (int, error) {
	return a.s.GetConnectionRefreshFailures(id)
}

// fakeLister captures the credentials passed in + returns canned
// results. Used by the on-create-discovery hook tests so we don't
// need a httptest.Server per case.
type fakeLister struct {
	called    atomic.Int32
	lastCreds atomic.Value // catalog.ListerCredentials
	models    []catalog.Model
}

func (f *fakeLister) ListModels(_ context.Context, creds catalog.ListerCredentials) ([]catalog.Model, error) {
	f.called.Add(1)
	f.lastCreds.Store(creds)
	return f.models, nil
}

func newDiscoveryServer(t *testing.T, listers map[string]catalog.ModelLister) (*Server, store.Store, catalog.Store) {
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
		t.Fatalf("seed catalog: %v", err)
	}
	if err := catalog.SeedProviderMeta(context.Background(), cs); err != nil {
		t.Fatalf("seed provider meta: %v", err)
	}

	srv := &Server{
		deps: Dependencies{
			Store:            st,
			Catalog:          catalog.NewRegistry(cs),
			CatalogStore:     cs,
			Discovery:        catalog.NewDiscoveryRunner(cs, listers),
			ProviderSelector: provider.NewSelector(),
			// AuthStore is wired so buildListerCredentials can surface
			// provider-specific ExtraHeaders (e.g., ChatGPT-Account-ID)
			// for subscription connections — same pattern as production
			// main.go:179.
			AuthStore: auth.NewAuthStore(&testAuthAdapter{s: st}),
		},
	}
	return srv, st, cs
}

// waitForListerCalls polls until the lister has been invoked at least
// `want` times or `timeout` elapses. Fire-and-forget goroutines are
// fundamentally async; polling is the simplest sync mechanism.
func waitForListerCalls(t *testing.T, lister *fakeLister, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if lister.called.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("lister called %d times, want ≥%d within %v",
		lister.called.Load(), want, timeout)
}

// waitForListerNotCalled gives the goroutine a chance to run, then
// asserts it didn't. Used by negative-case tests (e.g.,
// subscription_discoverable=false skip).
func waitForListerNotCalled(t *testing.T, lister *fakeLister, settle time.Duration) {
	t.Helper()
	time.Sleep(settle)
	if got := lister.called.Load(); got != 0 {
		t.Fatalf("lister was called %d times; want 0", got)
	}
}

// postCreateConnection encodes a connection struct and POSTs it to
// handleCreateConnection. Returns the HTTP status code.
func postCreateConnection(t *testing.T, srv *Server, conn store.Connection) int {
	t.Helper()
	body, err := json.Marshal(conn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/connections", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleCreateConnection(rec, req)
	return rec.Code
}

// TestCreateConnection_ApiKey_TriggersDiscovery — AC13. POST an
// apikey-auth connection for a discovery_enabled provider; verify
// the lister is invoked within a reasonable window.
func TestCreateConnection_ApiKey_TriggersDiscovery(t *testing.T) {
	lister := &fakeLister{
		models: []catalog.Model{
			{Provider: "openai", ModelID: "gpt-4.1", Tier: catalog.TierFrontier},
		},
	}
	srv, _, cs := newDiscoveryServer(t, map[string]catalog.ModelLister{
		"openai": lister,
	})

	code := postCreateConnection(t, srv, store.Connection{
		Provider: "openai",
		Name:     "openai-1",
		AuthType: "apikey",
		APIKey:   "sk-test",
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}

	waitForListerCalls(t, lister, 1, 2*time.Second)

	// The catalog Store reflects the discovered row with
	// source=discovery.
	models, err := cs.ListModels(context.Background(), "openai")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	found := false
	for _, m := range models {
		if m.ModelID == "gpt-4.1" {
			// Seed already had this row with source=seed. Discovery
			// upserted with source=discovery (precedence: discovery
			// overwrites seed).
			if m.Source != catalog.SourceDiscovery {
				t.Errorf("post-discovery source = %q, want %q",
					m.Source, catalog.SourceDiscovery)
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("gpt-4.1 row missing after discovery")
	}

	// Credentials forwarded correctly.
	creds := lister.lastCreds.Load().(catalog.ListerCredentials)
	if creds.APIKey != "sk-test" {
		t.Errorf("creds.APIKey = %q, want sk-test", creds.APIKey)
	}
	if creds.BaseURL == "" {
		t.Errorf("creds.BaseURL empty; should be derived from KnownProviders[\"openai\"]")
	}
}

// TestCreateConnection_Subscription_DiscoverableTrue_TriggersDiscovery —
// AC19 +ve case. Provider has subscription_discoverable=true (default
// for openai); a subscription-auth connection MUST trigger discovery
// using the OAuth access token.
func TestCreateConnection_Subscription_DiscoverableTrue_TriggersDiscovery(t *testing.T) {
	lister := &fakeLister{
		models: []catalog.Model{
			{Provider: "openai", ModelID: "gpt-4.1", Tier: catalog.TierFrontier},
		},
	}
	// Subscription openai dispatches to the openrouter-mirror lister
	// per catalog.DiscoveryListerKey (fix 20260514-openrouter-fallback).
	// Register the fake under that key so the test exercises the
	// production dispatch path.
	srv, _, _ := newDiscoveryServer(t, map[string]catalog.ModelLister{
		"openai":                   lister,
		"openai@codex-subscription": lister,
	})

	code := postCreateConnection(t, srv, store.Connection{
		Provider:    "openai",
		Name:        "openai-codex",
		AuthType:    "subscription",
		AccessToken: "sub-bearer-token",
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	waitForListerCalls(t, lister, 1, 2*time.Second)

	creds := lister.lastCreds.Load().(catalog.ListerCredentials)
	if creds.AccessToken != "sub-bearer-token" {
		t.Errorf("creds.AccessToken = %q, want sub-bearer-token", creds.AccessToken)
	}
}

// TestCreateConnection_Subscription_DiscoverableFalse_SkipsDiscovery —
// AC19 -ve case. github-copilot has subscription_discoverable=false
// in the seed. A subscription-auth connection must NOT trigger
// discovery — the static M3 allowlist remains authoritative.
func TestCreateConnection_Subscription_DiscoverableFalse_SkipsDiscovery(t *testing.T) {
	// Use a lister keyed on github-copilot so the test fails if the
	// hook bypasses the gate.
	lister := &fakeLister{}
	srv, _, _ := newDiscoveryServer(t, map[string]catalog.ModelLister{
		"github-copilot": lister,
	})

	code := postCreateConnection(t, srv, store.Connection{
		Provider:    "github-copilot",
		Name:        "copilot-sub",
		AuthType:    "subscription",
		AccessToken: "ghp-token",
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}

	// Give the goroutine a chance — but it must NOT call the lister.
	waitForListerNotCalled(t, lister, 200*time.Millisecond)
}

// TestCreateConnection_ApikeyOnNonDiscoverableProvider_StillTriggers —
// subscription_discoverable governs SUBSCRIPTION-auth only. apikey
// connections discover regardless (the gate is about whether the
// OAuth token can be used as a discovery credential, not whether
// the provider supports discovery at all).
func TestCreateConnection_ApikeyOnNonDiscoverableProvider_StillTriggers(t *testing.T) {
	// gemini has subscription_discoverable=false (Gemini CLI uses a
	// hard-coded model list, no OAuth-listable endpoint) but
	// discovery_enabled=true for API-key calls.
	lister := &fakeLister{}
	srv, _, _ := newDiscoveryServer(t, map[string]catalog.ModelLister{
		"gemini": lister,
	})

	code := postCreateConnection(t, srv, store.Connection{
		Provider: "gemini",
		Name:     "gemini-apikey",
		AuthType: "apikey",
		APIKey:   "AIza-test",
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	waitForListerCalls(t, lister, 1, 2*time.Second)
}

// TestCreateConnection_DiscoveryDisabled_SkipsDiscovery — when the
// provider has discovery_enabled=false (github-copilot via the
// seed), apikey connections also must skip. This is the "we don't
// know how to discover for this provider" case.
func TestCreateConnection_DiscoveryDisabled_SkipsDiscovery(t *testing.T) {
	lister := &fakeLister{}
	srv, _, _ := newDiscoveryServer(t, map[string]catalog.ModelLister{
		"github-copilot": lister,
	})

	code := postCreateConnection(t, srv, store.Connection{
		Provider: "github-copilot",
		Name:     "copilot-apikey",
		AuthType: "apikey",
		APIKey:   "ghp-personal",
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	waitForListerNotCalled(t, lister, 200*time.Millisecond)
}

// TestCreateConnection_ResponseIsFast — the discovery hook must NOT
// block the HTTP response. Asserts the handler returns 201 quickly
// even when the lister deliberately stalls.
func TestCreateConnection_ResponseIsFast(t *testing.T) {
	stallStarted := make(chan struct{})
	stallRelease := make(chan struct{})
	t.Cleanup(func() { close(stallRelease) })

	lister := &stallingLister{started: stallStarted, release: stallRelease}
	srv, _, _ := newDiscoveryServer(t, map[string]catalog.ModelLister{
		"openai": lister,
	})

	start := time.Now()
	code := postCreateConnection(t, srv, store.Connection{
		Provider: "openai",
		Name:     "openai-1",
		AuthType: "apikey",
		APIKey:   "sk-test",
	})
	elapsed := time.Since(start)

	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("handler took %v; expected fast response (≤500ms) despite stalling lister", elapsed)
	}
	// Confirm the goroutine DID start (so the test wasn't a false
	// negative from a missing-wire).
	select {
	case <-stallStarted:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("discovery goroutine did not start")
	}
}

type stallingLister struct {
	started chan struct{}
	release chan struct{}
}

func (s *stallingLister) ListModels(ctx context.Context, _ catalog.ListerCredentials) ([]catalog.Model, error) {
	close(s.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return nil, nil
	}
}
