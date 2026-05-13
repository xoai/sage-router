package refresh

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/provider"
)

// fakeStore is a minimal in-memory implementation of CredentialStore for
// loop tests. Tracks bump/reset counts so assertions can verify the
// counter math.
type fakeStore struct {
	mu                sync.Mutex
	creds             map[string]*auth.Credential
	failures          map[string]int
	bumpCalls         int
	resetCalls        int
	putCalls          int
	putErr            error
	getErr            error
	bumpErr           error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		creds:    map[string]*auth.Credential{},
		failures: map[string]int{},
	}
}

func (f *fakeStore) GetCredential(id string) (*auth.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	c, ok := f.creds[id]
	if !ok {
		return nil, nil
	}
	// Return a copy so the test's mutations don't bleed into the store.
	cp := *c
	return &cp, nil
}

func (f *fakeStore) PutCredential(id string, cred *auth.Credential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	if f.putErr != nil {
		return f.putErr
	}
	cp := *cred
	f.creds[id] = &cp
	return nil
}

func (f *fakeStore) BumpRefreshFailure(id string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bumpCalls++
	if f.bumpErr != nil {
		return 0, f.bumpErr
	}
	f.failures[id]++
	return f.failures[id], nil
}

func (f *fakeStore) ResetRefreshFailures(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalls++
	f.failures[id] = 0
	return nil
}

func newSubscriptionConn(id, providerID string) *provider.Connection {
	return provider.NewConnection(id, providerID, id, 0, auth.AuthTypeSubscription)
}

func newAPIKeyConn(id, providerID string) *provider.Connection {
	return provider.NewConnection(id, providerID, id, 0, auth.AuthTypeAPIKey)
}

// markAuthExpired drives a fresh connection through Idle→Active→AuthExpired.
func markAuthExpired(t *testing.T, c *provider.Connection) {
	t.Helper()
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := c.MarkAuthExpired(); err != nil {
		t.Fatalf("MarkAuthExpired: %v", err)
	}
}

// withTestLoop builds a Loop with very short timings so the test doesn't
// sit idle.
func withTestLoop(t *testing.T, store CredentialStore, refresher RefreshFunc, snap ConnSnapshot) *Loop {
	t.Helper()
	l := NewLoop(store, refresher, snap)
	l.interval = 20 * time.Millisecond
	l.rateLimit = 1 * time.Millisecond
	return l
}

// ── Filtering ──

func TestLoop_SkipsAPIKeyConnections(t *testing.T) {
	apikey := newAPIKeyConn("a1", "openai")
	store := newFakeStore()
	store.creds["a1"] = &auth.Credential{
		AccessToken:  "k",
		RefreshToken: "r",
		ExpiresAt:    time.Now().Add(-1 * time.Hour), // already expired
	}
	var calls int32
	refresher := func(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
		atomic.AddInt32(&calls, 1)
		return cred, nil
	}
	l := NewLoop(store, refresher, func() []*provider.Connection {
		return []*provider.Connection{apikey}
	})

	l.tick(context.Background())
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("refresher called for apikey connection (%d times)", got)
	}
}

// ── Idle-with-expiring-cred path ──

func TestLoop_ProactivelyRefreshesIdleExpiringConn(t *testing.T) {
	conn := newSubscriptionConn("s1", "openai")
	// Conn starts in Idle.
	store := newFakeStore()
	store.creds["s1"] = &auth.Credential{
		AccessToken:  "old",
		RefreshToken: "rtk",
		ExpiresAt:    time.Now().Add(2 * time.Minute), // within 5-min buffer
	}
	var calls int32
	refresher := func(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
		atomic.AddInt32(&calls, 1)
		return &auth.Credential{
			AccessToken:  "new",
			RefreshToken: cred.RefreshToken,
			ExpiresAt:    time.Now().Add(1 * time.Hour),
		}, nil
	}
	l := NewLoop(store, refresher, func() []*provider.Connection {
		return []*provider.Connection{conn}
	})

	l.tick(context.Background())

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("refresher calls = %d, want 1", got)
	}
	if store.putCalls != 1 {
		t.Errorf("PutCredential calls = %d, want 1", store.putCalls)
	}
	// State should remain Idle — no state transition for proactive refresh.
	if got, want := conn.State(), provider.StateIdle; got != want {
		t.Errorf("state = %v, want %v (no transition on proactive refresh)", got, want)
	}
	// Cache should be invalidated so next request loads from updated DB.
	if conn.HasCredential() {
		t.Error("cred cache should be invalidated after proactive refresh")
	}
}

func TestLoop_SkipsIdleWithFreshCred(t *testing.T) {
	conn := newSubscriptionConn("s1", "openai")
	store := newFakeStore()
	store.creds["s1"] = &auth.Credential{
		AccessToken:  "fine",
		RefreshToken: "rtk",
		ExpiresAt:    time.Now().Add(2 * time.Hour), // well outside buffer
	}
	var calls int32
	refresher := func(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
		atomic.AddInt32(&calls, 1)
		return cred, nil
	}
	l := NewLoop(store, refresher, func() []*provider.Connection {
		return []*provider.Connection{conn}
	})

	l.tick(context.Background())

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("refresher should not be called for fresh cred; got %d calls", got)
	}
}

// ── AuthExpired-driven path ──

func TestLoop_AuthExpiredHappyPath(t *testing.T) {
	conn := newSubscriptionConn("s1", "openai")
	markAuthExpired(t, conn)

	store := newFakeStore()
	store.creds["s1"] = &auth.Credential{
		AccessToken: "old", RefreshToken: "rtk", ExpiresAt: time.Now().Add(-1 * time.Hour),
	}
	refresher := func(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
		return &auth.Credential{
			AccessToken:  "fresh",
			RefreshToken: cred.RefreshToken,
			ExpiresAt:    time.Now().Add(1 * time.Hour),
		}, nil
	}
	l := NewLoop(store, refresher, func() []*provider.Connection {
		return []*provider.Connection{conn}
	})

	l.tick(context.Background())

	// AuthExpired → Refreshing → Active.
	if got, want := conn.State(), provider.StateActive; got != want {
		t.Errorf("state = %v, want %v", got, want)
	}
	if store.resetCalls != 1 {
		t.Errorf("ResetRefreshFailures calls = %d, want 1 after success", store.resetCalls)
	}
}

func TestLoop_RevokedRefreshTokenDisablesConn(t *testing.T) {
	conn := newSubscriptionConn("s1", "openai")
	markAuthExpired(t, conn)

	store := newFakeStore()
	store.creds["s1"] = &auth.Credential{
		AccessToken: "x", RefreshToken: "revoked-rtk", ExpiresAt: time.Now().Add(-1 * time.Hour),
	}
	refresher := func(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
		return nil, ErrRefreshTokenRevoked
	}
	l := NewLoop(store, refresher, func() []*provider.Connection {
		return []*provider.Connection{conn}
	})

	l.tick(context.Background())

	if got, want := conn.State(), provider.StateDisabled; got != want {
		t.Errorf("state = %v, want %v (revoked token should Disable on first failure)", got, want)
	}
	if store.failures["s1"] != 1 {
		t.Errorf("refresh_failures = %d, want 1", store.failures["s1"])
	}
}

func TestLoop_TransientFailureDoesNotDisableUntilThreshold(t *testing.T) {
	conn := newSubscriptionConn("s1", "openai")
	markAuthExpired(t, conn)

	store := newFakeStore()
	store.creds["s1"] = &auth.Credential{
		AccessToken: "x", RefreshToken: "rtk", ExpiresAt: time.Now().Add(-1 * time.Hour),
	}
	transient := errors.New("upstream 503 busy")
	refresher := func(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
		return nil, transient
	}
	l := NewLoop(store, refresher, func() []*provider.Connection {
		return []*provider.Connection{conn}
	})

	// First failure: AuthExpired → Refreshing → Errored. Not yet disabled.
	l.tick(context.Background())
	if got, want := conn.State(), provider.StateErrored; got != want {
		t.Errorf("after 1 failure: state = %v, want %v", got, want)
	}
	if store.failures["s1"] != 1 {
		t.Errorf("after 1 failure: refresh_failures = %d, want 1", store.failures["s1"])
	}

	// Move back to AuthExpired manually to simulate a fresh upstream 401
	// reaching the request path between sweeps.
	if err := conn.ResetCooldown(); err != nil {
		t.Fatalf("reset to idle: %v", err)
	}
	if err := conn.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := conn.MarkAuthExpired(); err != nil {
		t.Fatalf("MarkAuthExpired: %v", err)
	}
	l.tick(context.Background())
	// Now failures = 2. Still NOT disabled (threshold = 3).
	if got, want := conn.State(), provider.StateErrored; got != want {
		t.Errorf("after 2 failures: state = %v, want %v", got, want)
	}
	if store.failures["s1"] != 2 {
		t.Errorf("after 2 failures: refresh_failures = %d, want 2", store.failures["s1"])
	}

	// Third failure → should Disable.
	if err := conn.ResetCooldown(); err != nil {
		t.Fatalf("reset to idle: %v", err)
	}
	if err := conn.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := conn.MarkAuthExpired(); err != nil {
		t.Fatalf("MarkAuthExpired: %v", err)
	}
	l.tick(context.Background())
	if got, want := conn.State(), provider.StateDisabled; got != want {
		t.Errorf("after 3 failures: state = %v, want %v (auto-disable)", got, want)
	}
}

// ── Lifecycle ──

func TestLoop_RunRespectsContextCancellation(t *testing.T) {
	store := newFakeStore()
	refresher := func(ctx context.Context, c *auth.Credential) (*auth.Credential, error) {
		return c, nil
	}
	l := withTestLoop(t, store, refresher, func() []*provider.Connection { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()
	// Let one or two ticks land, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Loop exited promptly.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Loop.Run did not exit within 500ms of ctx cancel")
	}
}

func TestLoop_TickIsResilientToGetCredentialError(t *testing.T) {
	conn := newSubscriptionConn("s1", "openai")
	markAuthExpired(t, conn)
	store := newFakeStore()
	store.getErr = errors.New("disk error")
	var calls int32
	refresher := func(ctx context.Context, c *auth.Credential) (*auth.Credential, error) {
		atomic.AddInt32(&calls, 1)
		return c, nil
	}
	l := NewLoop(store, refresher, func() []*provider.Connection {
		return []*provider.Connection{conn}
	})

	// Should not panic, should not call refresher.
	l.tick(context.Background())

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("refresher should not be called when GetCredential fails; got %d", got)
	}
	// State should remain AuthExpired (we never marked Refreshing).
	if got, want := conn.State(), provider.StateAuthExpired; got != want {
		t.Errorf("state = %v, want %v", got, want)
	}
}

func TestLoop_SkipsConnectionsInRefreshingState(t *testing.T) {
	conn := newSubscriptionConn("s1", "openai")
	// Drive into Refreshing via the legitimate state path.
	if err := conn.MarkUsed(); err != nil {
		t.Fatal(err)
	}
	if err := conn.MarkAuthExpired(); err != nil {
		t.Fatal(err)
	}
	if err := conn.MarkRefreshing(); err != nil {
		t.Fatal(err)
	}

	store := newFakeStore()
	store.creds["s1"] = &auth.Credential{
		AccessToken: "x", RefreshToken: "rtk", ExpiresAt: time.Now().Add(-1 * time.Hour),
	}
	var calls int32
	refresher := func(ctx context.Context, c *auth.Credential) (*auth.Credential, error) {
		atomic.AddInt32(&calls, 1)
		return c, nil
	}
	l := NewLoop(store, refresher, func() []*provider.Connection {
		return []*provider.Connection{conn}
	})

	l.tick(context.Background())
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("refresher should not run for Refreshing-state conn; got %d calls", got)
	}
}
