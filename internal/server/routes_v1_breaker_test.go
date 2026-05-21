package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"sage-router/internal/executor"
	"sage-router/internal/provider"
	"sage-router/internal/store"
	"sage-router/pkg/canonical"
)

// M2 T9 — routes_v1.go circuit-breaker migration tests.
//
// AC6: markConnectionResult (the §3.2 classifier) emits FailureRateLimit for a
//      429 and FailureTransient for a transport error and a 5xx.
// AC9: the HALF_OPEN single-trial slot is released by the deferred
//      ReleaseHalfOpenTrial in executeRequest on every terminal path —
//      success, trial-failure, executor error, context-cancel, panic-unwind.

// ---------------------------------------------------------------------------
// AC6 — the classifier
// ---------------------------------------------------------------------------

func TestMarkConnectionResult_ClassifierEmitsCorrectFailureKind(t *testing.T) {
	t.Run("429 opens the breaker as rate_limit", func(t *testing.T) {
		c := provider.NewConnection("c1", "openai", "test", 0, "apikey")
		if err := c.MarkUsed(); err != nil {
			t.Fatalf("MarkUsed: %v", err)
		}
		s := fakeServerForStateMachine(t, c)
		s.markConnectionResult("c1", "gpt-5", 429, nil, nil, 0)

		if got := c.Breaker(); got != provider.BreakerOpen {
			t.Errorf("breaker = %v, want OPEN", got)
		}
		if got := c.FailureKind(); got != provider.FailureRateLimit {
			t.Errorf("failureKind = %v, want rate_limit", got)
		}
	})

	t.Run("429 cooldown honors the parsed Retry-After", func(t *testing.T) {
		c := provider.NewConnection("c2", "openai", "test", 0, "apikey")
		if err := c.MarkUsed(); err != nil {
			t.Fatalf("MarkUsed: %v", err)
		}
		s := fakeServerForStateMachine(t, c)
		retryAfter := 90 * time.Second
		before := time.Now()
		s.markConnectionResult("c2", "gpt-5", 429, nil, nil, retryAfter)

		// The classifier threads retryAfter into OpenBreaker → CooldownFor,
		// which returns retryAfter verbatim for the rate_limit kind.
		want := before.Add(retryAfter)
		if diff := c.CooldownUntil().Sub(want); diff > 2*time.Second || diff < -2*time.Second {
			t.Errorf("cooldownUntil = %v, want ~%v (Retry-After honored)", c.CooldownUntil(), want)
		}
	})

	t.Run("transport error opens the breaker as transient", func(t *testing.T) {
		c := provider.NewConnection("c3", "openai", "test", 0, "apikey")
		if err := c.MarkUsed(); err != nil {
			t.Fatalf("MarkUsed: %v", err)
		}
		s := fakeServerForStateMachine(t, c)
		s.markConnectionResult("c3", "gpt-5", 0, nil, errors.New("dial tcp: connection refused"), 0)

		if got := c.Breaker(); got != provider.BreakerOpen {
			t.Errorf("breaker = %v, want OPEN", got)
		}
		if got := c.FailureKind(); got != provider.FailureTransient {
			t.Errorf("failureKind = %v, want transient", got)
		}
		if c.LastError() == nil {
			t.Error("transport error should be recorded on the connection via SetLastError")
		}
	})

	t.Run("5xx opens the breaker as transient", func(t *testing.T) {
		c := provider.NewConnection("c4", "openai", "test", 0, "apikey")
		if err := c.MarkUsed(); err != nil {
			t.Fatalf("MarkUsed: %v", err)
		}
		s := fakeServerForStateMachine(t, c)
		s.markConnectionResult("c4", "gpt-5", 503, nil, nil, 0)

		if got := c.Breaker(); got != provider.BreakerOpen {
			t.Errorf("breaker = %v, want OPEN", got)
		}
		if got := c.FailureKind(); got != provider.FailureTransient {
			t.Errorf("failureKind = %v, want transient", got)
		}
		if c.LastError() == nil {
			t.Error("5xx should be recorded on the connection via SetLastError")
		}
	})
}

// ---------------------------------------------------------------------------
// AC9 — the deferred HALF_OPEN slot release
// ---------------------------------------------------------------------------

// panicExecutor panics inside Execute — drives the panic-unwind terminal path.
type panicExecutor struct{ providerID string }

func (p *panicExecutor) Provider() string { return p.providerID }
func (p *panicExecutor) Execute(ctx context.Context, req *executor.ExecuteRequest) (*executor.Result, error) {
	panic("simulated executor panic")
}

// selectClaimedHalfOpenConn registers one connection, forces its breaker to
// HALF_OPEN, then selects it through the production selectConnection path so
// Select claims the single trial slot (TryClaimHalfOpenTrial). It returns the
// *ConnectionInfo for executeRequest and the underlying *provider.Connection
// for slot assertions.
func selectClaimedHalfOpenConn(t *testing.T, srv *Server, db store.Store, providerID, model string) (*ConnectionInfo, *provider.Connection) {
	t.Helper()
	connID := addConnection(t, srv, db, providerID, "primary", "apikey")
	pc := srv.deps.ProviderSelector.ConnectionByID(connID)
	if pc == nil {
		t.Fatalf("connection %s not registered in the selector", connID)
	}
	// Force HALF_OPEN so Select consumes the single trial slot when it picks
	// this connection — the state a request path must release on exit.
	pc.SetFacetsForTest(provider.BreakerHalfOpen, provider.AuthValid, provider.LifecycleIdle)
	conn, _, err := srv.selectConnection(providerID, model, nil, provider.SelectDefault)
	if err != nil || conn == nil {
		t.Fatalf("selectConnection: %v", err)
	}
	if !pc.HalfOpenInFlightForTest() {
		t.Fatal("precondition: Select should have claimed the HALF_OPEN trial slot")
	}
	return conn, pc
}

// AC9 path 1 — normal success. executeRequest returns a Result; the deferred
// ReleaseHalfOpenTrial fires on the normal return. (The breaker stays HALF_OPEN
// here — MarkSuccess closes it later in forwardResult — the assertion is only
// that the trial slot itself was freed.)
func TestExecuteRequest_HalfOpenSlotReleasedOnSuccess(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  &mockExecutor{providerID: "openai"}, // default: 200 + canned body
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	conn, pc := selectClaimedHalfOpenConn(t, srv, db, "openai", "gpt-4o")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	result, err := srv.executeRequest(r.Context(), r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-ho-success", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body})
	if err != nil {
		t.Fatalf("executeRequest: unexpected err %v", err)
	}
	if result != nil && result.Body != nil {
		result.Body.Close()
	}
	if pc.HalfOpenInFlightForTest() {
		t.Error("HALF_OPEN trial slot leaked after a successful request")
	}
}

// AC9 path 2 — trial failure (upstream 5xx). The trial ran and the upstream
// rejected it; markConnectionResult opens the breaker AND the deferred release
// fires (idempotent — both leave the slot free). This test pins the end state,
// not the defer exclusively — paths 1/4/5 pin the defer as the sole releaser.
func TestExecuteRequest_HalfOpenSlotReleasedOnTrialFailure(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  newMock503Executor("openai"),
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	conn, pc := selectClaimedHalfOpenConn(t, srv, db, "openai", "gpt-4o")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	result, err := srv.executeRequest(r.Context(), r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-ho-trialfail", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body})
	if err != nil {
		t.Fatalf("executeRequest: a 5xx is a Result, not an error — got err %v", err)
	}
	if result != nil && result.Body != nil {
		result.Body.Close()
	}
	if pc.HalfOpenInFlightForTest() {
		t.Error("HALF_OPEN trial slot leaked after a failed (5xx) trial")
	}
}

// AC9 path 3 — executor error (transport failure). executeRequest returns
// (nil, err) after exhausting connection-level fallback; markConnectionResult
// opens the breaker AND the deferred release fires on the error return. Like
// path 2, this pins the end state — paths 1/4/5 pin the defer exclusively.
func TestExecuteRequest_HalfOpenSlotReleasedOnExecutorError(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  newMockNetErrExecutor("openai", errors.New("dial tcp: connection refused")),
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	conn, pc := selectClaimedHalfOpenConn(t, srv, db, "openai", "gpt-4o")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	_, err := srv.executeRequest(r.Context(), r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-ho-execerr", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body})
	if err == nil {
		t.Fatal("executeRequest: expected a network error on the exhausted-fallback path")
	}
	if pc.HalfOpenInFlightForTest() {
		t.Error("HALF_OPEN trial slot leaked after an executor error")
	}
}

// AC9 path 4 — context cancellation. The fallback loop returns at its first
// ctx.Err() check without marking the connection; the deferred release is the
// only thing that frees the slot.
func TestExecuteRequest_HalfOpenSlotReleasedOnContextCancel(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  &mockExecutor{providerID: "openai"},
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	conn, pc := selectClaimedHalfOpenConn(t, srv, db, "openai", "gpt-4o")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	ctx, cancel := context.WithCancel(r.Context())
	cancel() // cancel BEFORE the call so the loop returns at its first ctx check

	result, err := srv.executeRequest(ctx, r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-ho-cancel", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body})
	if err == nil {
		t.Fatal("executeRequest: expected a context-cancel error")
	}
	if result != nil && result.Body != nil {
		result.Body.Close()
	}
	if pc.HalfOpenInFlightForTest() {
		t.Error("HALF_OPEN trial slot leaked after context cancellation")
	}
}

// AC9 path 5 — panic unwind. The executor panics; the deferred release fires
// during stack unwinding before the panic reaches the Recovery middleware.
func TestExecuteRequest_HalfOpenSlotReleasedOnPanic(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  &panicExecutor{providerID: "openai"},
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	conn, pc := selectClaimedHalfOpenConn(t, srv, db, "openai", "gpt-4o")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected executeRequest to panic (panic-unwind path)")
			}
		}()
		_, _ = srv.executeRequest(r.Context(), r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-ho-panic", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body})
	}()

	if pc.HalfOpenInFlightForTest() {
		t.Error("HALF_OPEN trial slot leaked after a panic")
	}
}
