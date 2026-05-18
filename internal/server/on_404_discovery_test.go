package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sage-router/internal/catalog"
	"sage-router/internal/executor"
	"sage-router/internal/provider"
	"sage-router/internal/store"
)

// Models Discovery M2.7 — on-404 ad-hoc discovery (AC16). When an
// executor returns 404 ("model not found"), the upstream-error path
// in executeRequestWithCtx fires-and-forgets DiscoveryRunner
// .TryDiscoverOnNotFound so the catalog refreshes for the next
// request. The DiscoveryRunner's gates (backoff + 5-min debounce)
// keep request storms from hammering /v1/models.
//
// The integration test exercises the wiring end-to-end:
// 1. POST /v1/chat/completions to a model the upstream "doesn't know"
// 2. Mock executor returns 404
// 3. Within a short window, the catalog lister is called once
//
// Backoff and debounce are tested as unit tests against the runner
// itself (see internal/catalog/on404_test.go) — the integration test
// only proves the routes_v1 hook reaches the runner.

// new404Executor returns a mockExecutor that always responds 404.
func new404Executor(providerID string) *mockExecutor {
	return &mockExecutor{
		providerID: providerID,
		handler: func(_ *executor.ExecuteRequest) (*executor.Result, error) {
			return &executor.Result{
				StatusCode: http.StatusNotFound,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`)),
				Latency:    5 * time.Millisecond,
			}, nil
		},
	}
}

// countingDiscoveryLister is a tiny ModelLister that records each
// invocation so the integration test can assert "the on-404 hook
// reached the discovery runner."
type countingDiscoveryLister struct {
	called atomic.Int32
}

func (c *countingDiscoveryLister) ListModels(_ context.Context, _ catalog.ListerCredentials) ([]catalog.Model, error) {
	c.called.Add(1)
	return []catalog.Model{
		{Provider: "openai", ModelID: "discovered-new", Tier: catalog.TierStrong},
	}, nil
}

// wireDiscoveryOnto attaches a DiscoveryRunner + CatalogStore to an
// existing test server. The setupTestServer helper doesn't include
// catalog deps by default — the on-404 hook is opt-in.
func wireDiscoveryOnto(t *testing.T, srv *Server, db store.Store, listers map[string]catalog.ModelLister) (catalog.Store, *catalog.DiscoveryRunner) {
	t.Helper()
	cs := catalog.NewSQLiteStore(db.DB())
	if err := catalog.SeedFromConstants(context.Background(), cs); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := catalog.SeedProviderMeta(context.Background(), cs); err != nil {
		t.Fatalf("seed provider meta: %v", err)
	}
	runner := catalog.NewDiscoveryRunner(cs, listers)
	srv.deps.Catalog = catalog.NewRegistry(cs)
	srv.deps.CatalogStore = cs
	srv.deps.Discovery = runner
	return cs, runner
}

// waitForCount polls atomic.Int32 until it reaches `want` or `timeout`
// elapses. Fire-and-forget goroutines are inherently async; polling
// is the simplest sync primitive that doesn't require plumbing
// channels through production code.
func waitForCount(t *testing.T, c *atomic.Int32, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("counter reached %d, want ≥%d within %v", c.Load(), want, timeout)
}

// TestExecutor404_TriggersDiscovery — AC16 wiring. A 404 from the
// executor reaches the on-404 hook in executeRequestWithCtx, which
// invokes DiscoveryRunner.TryDiscoverOnNotFound asynchronously.
func TestExecutor404_TriggersDiscovery(t *testing.T) {
	srv, db := setupTestServer(t, map[string]executor.Executor{
		"openai":  new404Executor("openai"),
		"default": new404Executor("default"),
	})
	addConnection(t, srv, db, "openai", "primary", "apikey")

	lister := &countingDiscoveryLister{}
	wireDiscoveryOnto(t, srv, db, map[string]catalog.ModelLister{
		"openai": lister,
	})

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "openai/unknown-model",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	// The 404 IS forwarded to the client (no fallback chain for 404).
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 forwarded to client, got %d: %s", w.Code, w.Body.String())
	}

	// Within a short window, the on-404 goroutine MUST have triggered
	// the lister. 2s is generous — the runner's call is in-process
	// against an in-memory SQLite + a no-op fake lister.
	waitForCount(t, &lister.called, 1, 2*time.Second)
}

// TestExecutor404_DebounceWithin5min — two 404 responses for the
// same provider in quick succession must invoke the lister only
// once. The runner-level debounce (5 min) catches the second call.
func TestExecutor404_DebounceWithin5min(t *testing.T) {
	srv, db := setupTestServer(t, map[string]executor.Executor{
		"openai":  new404Executor("openai"),
		"default": new404Executor("default"),
	})
	addConnection(t, srv, db, "openai", "primary", "apikey")

	lister := &countingDiscoveryLister{}
	wireDiscoveryOnto(t, srv, db, map[string]catalog.ModelLister{
		"openai": lister,
	})

	// First 404: triggers.
	w1 := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/unknown-model-1",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w1.Code != http.StatusNotFound {
		t.Fatalf("first request: expected 404, got %d", w1.Code)
	}
	waitForCount(t, &lister.called, 1, 2*time.Second)

	// Second 404 for the same provider, well within the 5-min debounce
	// window. Settle for 300ms (longer than the goroutine schedule
	// latency) and assert no second invocation occurred.
	w2 := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/unknown-model-2",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w2.Code != http.StatusNotFound {
		t.Fatalf("second request: expected 404, got %d", w2.Code)
	}
	time.Sleep(300 * time.Millisecond)
	if got := lister.called.Load(); got != 1 {
		t.Errorf("lister called %d times across two 404s; want 1 (debounce)", got)
	}
}

// TestExecutor404_SubscriptionDiscoverableFalse_Skipped — AC19 parity
// with M2.4. A subscription-auth connection on a provider with
// SubscriptionDiscoverable=false must NOT trigger the lister on a 404,
// even though discovery_enabled=true. Without this gate the on-404
// path would silently undo the M2.4 protection — every 404 to a
// subscription connection on a non-discoverable provider would issue
// a /v1/models call using the subscription token.
//
// Implementation note: we use `openai` (translator registered in
// setupTestServer) and override its ProviderMeta to set
// SubscriptionDiscoverable=false. Using gemini in the test would
// short-circuit before reaching the on-404 hook because gemini lacks
// a translator in the default test wiring.
func TestExecutor404_SubscriptionDiscoverableFalse_Skipped(t *testing.T) {
	srv, db := setupTestServer(t, map[string]executor.Executor{
		"openai":  new404Executor("openai"),
		"default": new404Executor("default"),
	})

	// Subscription-auth connection on openai. ExchangedToken populated
	// so the M2b.8 pre-flight at selectConnection allows the connection
	// to reach the upstream (the test asserts the upstream 404 path; an
	// empty exchanged token would short-circuit at pre-flight and skip
	// the upstream call entirely).
	conn := &store.Connection{
		ID:          "conn-openai-sub",
		Provider:    "openai",
		Name:        "openai-sub",
		AuthType:    "subscription",
		AccessToken: "sub-bearer-token",
		// ExchangedToken removed at M2.6.6 — column dropped via migration 013.
		Priority: 0,
		State:    "idle",
	}
	if err := db.CreateConnection(conn); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	srv.deps.ProviderSelector.Register(
		provider.NewConnection(conn.ID, conn.Provider, conn.Name, conn.Priority, conn.AuthType),
	)

	lister := &countingDiscoveryLister{}
	cs, _ := wireDiscoveryOnto(t, srv, db, map[string]catalog.ModelLister{
		"openai": lister,
	})

	// Override openai's seeded SubscriptionDiscoverable=true → false
	// to exercise the AC19 gate. discovery_enabled stays true so the
	// only reason the helper should skip is the subscription gate.
	if err := cs.SetProviderMeta(context.Background(), "openai", catalog.ProviderMeta{
		Provider:                 "openai",
		DiscoveryEnabled:         true,
		SubscriptionDiscoverable: false,
	}); err != nil {
		t.Fatalf("override openai meta: %v", err)
	}

	// Use a model in the openai subscription allowlist
	// (internal/auth/providers/registry.go) so the selector's
	// CanServeModel check passes — otherwise the request 503s in the
	// selector before reaching the executor. The mock executor returns
	// 404 regardless of model name; the point is to exercise the
	// on-404 hook with a subscription connection.
	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// Give the goroutine a chance — the AC19 gate must short-circuit
	// before TryDiscoverOnNotFound is called.
	time.Sleep(300 * time.Millisecond)
	if got := lister.called.Load(); got != 0 {
		t.Errorf("lister called %d times for subscription_discoverable=false; want 0", got)
	}
}

// TestExecutor404_ApiKeyOnNonDiscoverableProvider_StillTriggers —
// SubscriptionDiscoverable governs SUBSCRIPTION-auth only. An apikey
// connection on a provider with SubscriptionDiscoverable=false still
// triggers on-404 discovery, parity with M2.4's apikey behaviour.
// Same fixture as the subscription-skip test but with AuthType=apikey:
// the gate is auth-type-scoped, not provider-scoped.
func TestExecutor404_ApiKeyOnNonDiscoverableProvider_StillTriggers(t *testing.T) {
	srv, db := setupTestServer(t, map[string]executor.Executor{
		"openai":  new404Executor("openai"),
		"default": new404Executor("default"),
	})
	addConnection(t, srv, db, "openai", "openai-apikey", "apikey")

	lister := &countingDiscoveryLister{}
	cs, _ := wireDiscoveryOnto(t, srv, db, map[string]catalog.ModelLister{
		"openai": lister,
	})

	// Same SubscriptionDiscoverable=false override.
	if err := cs.SetProviderMeta(context.Background(), "openai", catalog.ProviderMeta{
		Provider:                 "openai",
		DiscoveryEnabled:         true,
		SubscriptionDiscoverable: false,
	}); err != nil {
		t.Fatalf("override openai meta: %v", err)
	}

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/unknown-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	waitForCount(t, &lister.called, 1, 2*time.Second)
}

// TestExecutor404_RespectsBackoff — a provider with active persistent
// backoff (NextDiscoveryAfter > now) does NOT have its lister called
// even when a 404 lands. Backoff is the load-bearing gate that
// outlives the 5-min debounce window.
func TestExecutor404_RespectsBackoff(t *testing.T) {
	srv, db := setupTestServer(t, map[string]executor.Executor{
		"openai":  new404Executor("openai"),
		"default": new404Executor("default"),
	})
	addConnection(t, srv, db, "openai", "primary", "apikey")

	lister := &countingDiscoveryLister{}
	cs, _ := wireDiscoveryOnto(t, srv, db, map[string]catalog.ModelLister{
		"openai": lister,
	})

	// Seed openai's ProviderMeta with active backoff: 2h cool-down,
	// step 2 (so it's clearly in the cool-down state, not a fresh
	// failure).
	if err := cs.SetProviderMeta(context.Background(), "openai", catalog.ProviderMeta{
		Provider:           "openai",
		DiscoveryEnabled:   true,
		BackoffStep:        2,
		NextDiscoveryAfter: time.Now().Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("seed backoff meta: %v", err)
	}

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/unknown-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}

	// Settle for the goroutine to definitely have run if it was going
	// to. The backoff gate must block the lister.
	time.Sleep(300 * time.Millisecond)
	if got := lister.called.Load(); got != 0 {
		t.Errorf("lister called %d times during backoff; want 0", got)
	}
}

// extraHeadersCapturingLister records the ListerCredentials it received.
// Unlike countingDiscoveryLister it preserves the creds, so the on-404
// path's ExtraHeaders threading can be asserted.
type extraHeadersCapturingLister struct {
	called    atomic.Int32
	lastCreds atomic.Value // catalog.ListerCredentials
}

func (l *extraHeadersCapturingLister) ListModels(_ context.Context, creds catalog.ListerCredentials) ([]catalog.Model, error) {
	l.called.Add(1)
	l.lastCreds.Store(creds)
	return nil, nil
}

// TestOnNotFoundDiscovery_ThreadsExtraHeaders — initiative
// 20260513-openai-discovery. The on-404 path at routes_api.go:441-446
// threads conn.Credentials.ExtraHeaders into ListerCredentials so the
// 404-triggered discovery refresh sends ChatGPT-Account-ID like the
// on-create path does. Without this test, a future change to
// routes_v1.go:1025-1035 (which populates conn.Credentials.ExtraHeaders
// for subscription connections) could silently break the on-404 path.
//
// Exercises triggerOnNotFoundDiscovery directly with a *ConnectionInfo
// that has pre-populated ExtraHeaders — matches the shape the request-
// time path builds.
func TestOnNotFoundDiscovery_ThreadsExtraHeaders(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	lister := &extraHeadersCapturingLister{}
	// Subscription openai on-404 dispatches to the openrouter-mirror
	// lister key per catalog.DiscoveryListerKey (fix
	// 20260514-openrouter-fallback). Register the fake under both
	// keys so the test exercises the production dispatch path while
	// preserving its ExtraHeaders contract assertion.
	_, _ = wireDiscoveryOnto(t, srv, db, map[string]catalog.ModelLister{
		"openai":                   lister,
		"openai@codex-subscription": lister,
	})

	conn := &ConnectionInfo{
		ID: "c-on404-extra-test",
		Credentials: &executor.Credentials{
			ConnectionID: "c-on404-extra-test",
			AuthType:     "subscription",
			AccessToken:  "oauth-jwt-on404",
			ExtraHeaders: map[string]string{
				"ChatGPT-Account-ID": "acct-on404-555",
			},
		},
	}

	// Direct call — bypasses the HTTP path so the assertion is hermetic
	// against routes_v1's wiring (which is exercised by the AC16
	// integration test above).
	srv.triggerOnNotFoundDiscovery("openai", conn)

	waitForCount(t, &lister.called, 1, 2*time.Second)

	got, ok := lister.lastCreds.Load().(catalog.ListerCredentials)
	if !ok {
		t.Fatal("lister.lastCreds did not store catalog.ListerCredentials")
	}
	if got.AccessToken != "oauth-jwt-on404" {
		t.Errorf("AccessToken = %q, want oauth-jwt-on404", got.AccessToken)
	}
	if got.ExtraHeaders == nil {
		t.Fatal("ExtraHeaders = nil; on-404 path dropped the headers")
	}
	if h := got.ExtraHeaders["ChatGPT-Account-ID"]; h != "acct-on404-555" {
		t.Errorf("ExtraHeaders[ChatGPT-Account-ID] = %q, want acct-on404-555", h)
	}
}
