package catalog

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// credsForAll is a CredentialsLookup that returns a fixed
// ListerCredentials for every provider — adequate for tests that
// don't care about per-provider auth. authType is "apikey" so the
// 24h ticker's DiscoveryListerKey dispatch resolves to the
// provider's own lister (no mirror routing).
func credsForAll(_ string) (ListerCredentials, string, bool) {
	return ListerCredentials{BaseURL: "https://example.test", APIKey: "test"}, "apikey", true
}

// TestRefresh_TicksAfterInitialSettle — the ticker fires once after
// the initial settle delay. We bypass the 5min settle by setting
// SettleDelay=0 and use a manually-driven tick channel so the test
// is deterministic.
func TestRefresh_TicksAfterInitialSettle(t *testing.T) {
	cs, _, ctx := freshStore(t)
	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider: "openai", DiscoveryEnabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var calls atomic.Int32
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai": fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			calls.Add(1)
			return []Model{{Provider: "openai", ModelID: "m1"}}, nil
		}},
	})

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ticks := make(chan time.Time, 4)
	done := make(chan struct{})
	go func() {
		runBackgroundRefreshLoop(loopCtx, runner, credsForAll, ticks)
		close(done)
	}()

	// Fire one tick; the loop must dispatch DiscoverAll → DiscoverProvider("openai").
	ticks <- time.Now()

	// Wait for the lister to be called.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 1 {
		t.Fatalf("ticker did not invoke lister within 2s")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not exit on ctx cancel")
	}
}

// TestRefresh_RespectsBackoff — when a provider's NextDiscoveryAfter
// is in the future, the ticker MUST NOT invoke the lister. AC15 +
// AC14c interplay.
func TestRefresh_RespectsBackoff(t *testing.T) {
	cs, _, ctx := freshStore(t)
	frozen := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	// Backoff active — next run an hour from "now".
	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider:           "openai",
		DiscoveryEnabled:   true,
		BackoffStep:        2,
		NextDiscoveryAfter: frozen.Add(1 * time.Hour),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var calls atomic.Int32
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai": fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			calls.Add(1)
			return nil, nil
		}},
	})
	runner.Clock = func() time.Time { return frozen }

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		runBackgroundRefreshLoop(loopCtx, runner, credsForAll, ticks)
		close(done)
	}()

	ticks <- frozen
	// Give the loop a chance to process the tick.
	time.Sleep(100 * time.Millisecond)

	if calls.Load() != 0 {
		t.Errorf("lister called %d times; want 0 (backoff active)", calls.Load())
	}

	cancel()
	<-done
}

// TestRefresh_DispatchesAllEnabledProviders — DiscoverAll invokes
// every provider with DiscoveryEnabled=true. Skips disabled ones
// (e.g., github-copilot in the seed defaults).
func TestRefresh_DispatchesAllEnabledProviders(t *testing.T) {
	cs, _, ctx := freshStore(t)
	for _, p := range []ProviderMeta{
		{Provider: "openai", DiscoveryEnabled: true},
		{Provider: "anthropic", DiscoveryEnabled: true},
		{Provider: "github-copilot", DiscoveryEnabled: false},
	} {
		if err := cs.SetProviderMeta(ctx, p.Provider, p); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	called := map[string]*atomic.Int32{
		"openai":         {},
		"anthropic":      {},
		"github-copilot": {},
	}
	makeLister := func(name string) ModelLister {
		return fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			called[name].Add(1)
			return nil, nil
		}}
	}
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai":         makeLister("openai"),
		"anthropic":      makeLister("anthropic"),
		"github-copilot": makeLister("github-copilot"),
	})

	results := runner.DiscoverAll(ctx, credsForAll)

	// 2 enabled, 1 skipped → 2 results returned.
	if len(results) != 2 {
		t.Errorf("results len = %d, want 2 (skipped disabled)", len(results))
	}

	if called["openai"].Load() != 1 {
		t.Errorf("openai lister called %d times, want 1", called["openai"].Load())
	}
	if called["anthropic"].Load() != 1 {
		t.Errorf("anthropic lister called %d times, want 1", called["anthropic"].Load())
	}
	if called["github-copilot"].Load() != 0 {
		t.Errorf("github-copilot lister called %d times; want 0 (DiscoveryEnabled=false)",
			called["github-copilot"].Load())
	}
}

// TestRefresh_StopsOnContextDone — the loop exits cleanly when the
// context is cancelled. No goroutine leak.
func TestRefresh_StopsOnContextDone(t *testing.T) {
	cs, _, ctx := freshStore(t)
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{})

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runBackgroundRefreshLoop(loopCtx, runner, credsForAll, make(chan time.Time))
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// good
	case <-time.After(time.Second):
		t.Fatal("loop did not exit within 1s of ctx cancel")
	}
}

// TestRefresh_SkipsProviderWithMissingCredentials — when the
// CredentialsLookup returns false for a provider (no connection
// available right now), the ticker skips it gracefully without
// erroring.
func TestRefresh_SkipsProviderWithMissingCredentials(t *testing.T) {
	cs, _, ctx := freshStore(t)
	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider: "openai", DiscoveryEnabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var calls atomic.Int32
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai": fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			calls.Add(1)
			return nil, nil
		}},
	})

	results := runner.DiscoverAll(ctx, func(_ string) (ListerCredentials, string, bool) {
		return ListerCredentials{}, "", false // no creds available
	})

	if len(results) != 0 {
		t.Errorf("results len = %d, want 0 (no creds → skip)", len(results))
	}
	if calls.Load() != 0 {
		t.Errorf("lister called %d times; want 0", calls.Load())
	}
}
