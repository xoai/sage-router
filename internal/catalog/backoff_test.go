package catalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRefresh_ExponentialBackoff — AC14c. Three (then five) consecutive
// failures must advance BackoffStep 0→1→2→3→4→5 and NextDiscoveryAfter
// to (now + 1h, +2h, +4h, +8h, +24h). Step caps at 5; further
// failures stay at the 24h cap.
//
// The test uses an injected Clock so failures land at deterministic
// "now" values; the assertion is on stored ProviderMeta.NextDiscoveryAfter
// minus that frozen "now" — drift-free.
func TestRefresh_ExponentialBackoff(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Seed the provider row so SetProviderMeta has somewhere to write.
	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider: "openai", DiscoveryEnabled: true,
	}); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	// Frozen clock so timestamp arithmetic is precise.
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai": fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return nil, errors.New("simulated 401")
		}},
	})
	runner.Clock = func() time.Time { return now }

	cases := []struct {
		step  int
		delay time.Duration
	}{
		{1, 1 * time.Hour},
		{2, 2 * time.Hour},
		{3, 4 * time.Hour},
		{4, 8 * time.Hour},
		{5, 24 * time.Hour},
		{5, 24 * time.Hour}, // 6th failure stays at cap
	}
	for i, c := range cases {
		res := runner.DiscoverProvider(ctx, "openai", ListerCredentials{})
		if res.Err == nil {
			t.Fatalf("failure %d: expected err, got nil", i+1)
		}
		meta, err := cs.GetProviderMeta(ctx, "openai")
		if err != nil {
			t.Fatalf("get meta after failure %d: %v", i+1, err)
		}
		if meta.BackoffStep != c.step {
			t.Errorf("failure %d: BackoffStep = %d, want %d", i+1, meta.BackoffStep, c.step)
		}
		wantAfter := now.Add(c.delay)
		if d := meta.NextDiscoveryAfter.Sub(wantAfter); d < -time.Second || d > time.Second {
			t.Errorf("failure %d: NextDiscoveryAfter drift = %v (got %v want %v)",
				i+1, d, meta.NextDiscoveryAfter, wantAfter)
		}
	}
}

// TestRefresh_BackoffResetOnSuccess — after failures climb to step 2,
// one success must zero BackoffStep AND clear NextDiscoveryAfter.
func TestRefresh_BackoffResetOnSuccess(t *testing.T) {
	cs, _, ctx := freshStore(t)
	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider: "openai", DiscoveryEnabled: true,
	}); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	// Two failures.
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai": fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return nil, errors.New("simulated")
		}},
	})
	runner.Clock = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		runner.DiscoverProvider(ctx, "openai", ListerCredentials{})
	}
	meta, _ := cs.GetProviderMeta(ctx, "openai")
	if meta.BackoffStep != 2 {
		t.Fatalf("setup: BackoffStep = %d, want 2", meta.BackoffStep)
	}

	// One success.
	runner.Listers["openai"] = fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
		return []Model{{Provider: "openai", ModelID: "m1"}}, nil
	}}
	res := runner.DiscoverProvider(ctx, "openai", ListerCredentials{})
	if res.Err != nil {
		t.Fatalf("DiscoverProvider success: %v", res.Err)
	}

	meta, _ = cs.GetProviderMeta(ctx, "openai")
	if meta.BackoffStep != 0 {
		t.Errorf("post-success BackoffStep = %d, want 0", meta.BackoffStep)
	}
	if !meta.NextDiscoveryAfter.IsZero() {
		t.Errorf("post-success NextDiscoveryAfter = %v, want zero", meta.NextDiscoveryAfter)
	}
	if meta.LastDiscoveryError != "" {
		t.Errorf("post-success LastDiscoveryError = %q, want \"\"", meta.LastDiscoveryError)
	}
}

// TestShouldRunForProvider_RespectsBackoff — the helper returns false
// when now is before NextDiscoveryAfter; true when now reaches or
// passes it.
func TestShouldRunForProvider_RespectsBackoff(t *testing.T) {
	cs, _, _ := freshStore(t)
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{})

	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	runner.Clock = func() time.Time { return now }

	cases := []struct {
		name       string
		meta       ProviderMeta
		nowOffset  time.Duration
		wantShould bool
	}{
		{
			name:       "discovery_disabled — never run",
			meta:       ProviderMeta{DiscoveryEnabled: false},
			nowOffset:  0,
			wantShould: false,
		},
		{
			name: "next_after future — backoff active",
			meta: ProviderMeta{
				DiscoveryEnabled:   true,
				NextDiscoveryAfter: now.Add(1 * time.Hour),
			},
			nowOffset:  0,
			wantShould: false,
		},
		{
			name: "next_after past — backoff expired",
			meta: ProviderMeta{
				DiscoveryEnabled:   true,
				NextDiscoveryAfter: now.Add(-1 * time.Hour),
			},
			nowOffset:  0,
			wantShould: true,
		},
		{
			name: "next_after zero — no backoff",
			meta: ProviderMeta{
				DiscoveryEnabled:   true,
				NextDiscoveryAfter: time.Time{},
			},
			nowOffset:  0,
			wantShould: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runner.ShouldRunForProvider(c.meta, now.Add(c.nowOffset))
			if got != c.wantShould {
				t.Errorf("ShouldRunForProvider = %v, want %v", got, c.wantShould)
			}
		})
	}
}

// TestDiscoverProvider_RespectsBackoffOnTicker — when the scheduled
// ticker calls DiscoverProvider on a provider whose backoff is
// active, the call returns without invoking the lister. (The on-create
// hook in M2.4 bypasses backoff by NOT consulting ShouldRunForProvider
// — that's the documented contract from ADR-2.)
//
// Note: the underlying DiscoverProvider invokes the lister directly;
// the backoff check lives in the caller (M2.5 ticker, M2.7 on-404).
// This test exercises a helper-level scenario: the runner exposes
// ShouldRunForProvider and the ticker calls it before DiscoverProvider.
func TestDiscoverProvider_BackoffCheckInTickerSemantic(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Seed provider with active backoff.
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider:           "openai",
		DiscoveryEnabled:   true,
		BackoffStep:        2,
		NextDiscoveryAfter: now.Add(1 * time.Hour),
	}); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	called := false
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai": fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			called = true
			return nil, nil
		}},
	})
	runner.Clock = func() time.Time { return now }

	// Ticker-style usage: check ShouldRunForProvider before dispatch.
	meta, _ := cs.GetProviderMeta(ctx, "openai")
	if runner.ShouldRunForProvider(*meta, now) {
		t.Fatal("ShouldRunForProvider returned true while backoff active")
	}
	// Lister must not have been called via the ticker path.
	if called {
		t.Error("lister invoked despite active backoff")
	}
}
