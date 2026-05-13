package catalog

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// On-404 ad-hoc discovery (M2.7, AC16). TryDiscoverOnNotFound is the
// entrypoint executor error paths call when an upstream returns
// "model not found" (404 in OpenAI/Anthropic, similar elsewhere). It
// MUST honour two gates before invoking the lister:
//
//  1. Persistent backoff state (ShouldRunForProvider from M2.6) —
//     a provider whose 24h ticker is in cool-down does NOT get
//     re-triggered by every 404. Backoff is the load-bearing signal.
//
//  2. 5-minute in-memory debounce — back-to-back 404s for the same
//     provider only invoke the lister once. Prevents a stream of
//     failing requests from hammering /v1/models.

const fiveMin = 5 * time.Minute

// fakeCountingLister wraps a closure but also reports a per-test
// invocation count so debounce assertions are direct.
type fakeCountingLister struct {
	called atomic.Int32
	fn     func(ctx context.Context, creds ListerCredentials) ([]Model, error)
}

func (f *fakeCountingLister) ListModels(ctx context.Context, creds ListerCredentials) ([]Model, error) {
	f.called.Add(1)
	return f.fn(ctx, creds)
}

// TestTryDiscoverOnNotFound_TriggersDiscovery — AC16 happy path.
// Provider with discovery_enabled=true, no backoff, no recent debounce
// entry: the lister IS called, the result reflects the discovered rows,
// and the debounce map is stamped so a second immediate call would skip.
func TestTryDiscoverOnNotFound_TriggersDiscovery(t *testing.T) {
	cs, _, ctx := freshStore(t)

	lister := &fakeCountingLister{
		fn: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return []Model{
				{Provider: "openai", ModelID: "gpt-4o-new", Tier: TierFrontier},
			}, nil
		},
	}
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{"openai": lister})

	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	runner.Clock = func() time.Time { return now }

	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider:         "openai",
		DiscoveryEnabled: true,
	}); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	res := runner.TryDiscoverOnNotFound(ctx, "openai", ListerCredentials{BaseURL: "https://x"})
	if res.Err != nil {
		t.Fatalf("TryDiscoverOnNotFound err: %v", res.Err)
	}
	if res.Skipped {
		t.Fatalf("Skipped=true on the happy path; want false")
	}
	if res.Count != 1 {
		t.Errorf("Count = %d, want 1", res.Count)
	}
	if got := lister.called.Load(); got != 1 {
		t.Errorf("lister called %d times, want 1", got)
	}
}

// TestTryDiscoverOnNotFound_DebounceWithin5min — AC16. Two on-404
// triggers within the 5-minute window invoke the lister exactly once.
// The second returns Skipped without touching the upstream.
func TestTryDiscoverOnNotFound_DebounceWithin5min(t *testing.T) {
	cs, _, ctx := freshStore(t)

	lister := &fakeCountingLister{
		fn: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return []Model{{Provider: "openai", ModelID: "gpt-4o", Tier: TierFrontier}}, nil
		},
	}
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{"openai": lister})

	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	current := base
	runner.Clock = func() time.Time { return current }

	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider:         "openai",
		DiscoveryEnabled: true,
	}); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	// First call: runs.
	res1 := runner.TryDiscoverOnNotFound(ctx, "openai", ListerCredentials{BaseURL: "https://x"})
	if res1.Skipped || res1.Err != nil {
		t.Fatalf("first call: Skipped=%v Err=%v", res1.Skipped, res1.Err)
	}

	// Advance 2 minutes (well within 5min window).
	current = base.Add(2 * time.Minute)

	// Second call: skipped due to debounce. Backoff was reset by the
	// successful first call, so backoff is NOT the reason for skip.
	res2 := runner.TryDiscoverOnNotFound(ctx, "openai", ListerCredentials{BaseURL: "https://x"})
	if !res2.Skipped {
		t.Errorf("second call should be Skipped (debounce); got %+v", res2)
	}
	if got := lister.called.Load(); got != 1 {
		t.Errorf("lister called %d times across two on-404 triggers; want 1 (debounce)", got)
	}
}

// TestTryDiscoverOnNotFound_DebounceExpiresAfter5min — once the
// debounce window elapses, a subsequent 404 retriggers the lister.
// Backoff was reset by the previous success, so this isolates the
// debounce-timer logic.
func TestTryDiscoverOnNotFound_DebounceExpiresAfter5min(t *testing.T) {
	cs, _, ctx := freshStore(t)

	lister := &fakeCountingLister{
		fn: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return []Model{{Provider: "openai", ModelID: "gpt-4o", Tier: TierFrontier}}, nil
		},
	}
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{"openai": lister})

	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	current := base
	runner.Clock = func() time.Time { return current }

	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider:         "openai",
		DiscoveryEnabled: true,
	}); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	_ = runner.TryDiscoverOnNotFound(ctx, "openai", ListerCredentials{})

	// Advance past the debounce window.
	current = base.Add(fiveMin + time.Second)

	res := runner.TryDiscoverOnNotFound(ctx, "openai", ListerCredentials{})
	if res.Skipped {
		t.Errorf("call past debounce window should run; got Skipped")
	}
	if got := lister.called.Load(); got != 2 {
		t.Errorf("lister called %d times; want 2 (initial + post-window)", got)
	}
}

// TestTryDiscoverOnNotFound_RespectsBackoff — AC16. Persistent backoff
// state overrides on-404 triggers. Even with an empty debounce map, a
// provider in cool-down does NOT have its lister called.
func TestTryDiscoverOnNotFound_RespectsBackoff(t *testing.T) {
	cs, _, ctx := freshStore(t)

	lister := &fakeCountingLister{
		fn: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return nil, nil
		},
	}
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{"openai": lister})

	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	runner.Clock = func() time.Time { return now }

	// Seed provider with active backoff: NextDiscoveryAfter is 1h in
	// the future. ShouldRunForProvider returns false → skip.
	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider:           "openai",
		DiscoveryEnabled:   true,
		BackoffStep:        2,
		NextDiscoveryAfter: now.Add(1 * time.Hour),
	}); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	res := runner.TryDiscoverOnNotFound(ctx, "openai", ListerCredentials{})
	if !res.Skipped {
		t.Errorf("backoff-active provider should Skip on-404; got %+v", res)
	}
	if got := lister.called.Load(); got != 0 {
		t.Errorf("lister called %d times during backoff; want 0", got)
	}
}

// TestTryDiscoverOnNotFound_DiscoveryDisabledSkipped — provider with
// discovery_enabled=false (e.g. github-copilot in the seed) must be
// skipped. Same code path as the backoff case via ShouldRunForProvider.
func TestTryDiscoverOnNotFound_DiscoveryDisabledSkipped(t *testing.T) {
	cs, _, ctx := freshStore(t)

	lister := &fakeCountingLister{
		fn: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return nil, nil
		},
	}
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"github-copilot": lister,
	})

	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	runner.Clock = func() time.Time { return now }

	if err := cs.SetProviderMeta(ctx, "github-copilot", ProviderMeta{
		Provider:         "github-copilot",
		DiscoveryEnabled: false,
	}); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	res := runner.TryDiscoverOnNotFound(ctx, "github-copilot", ListerCredentials{})
	if !res.Skipped {
		t.Errorf("discovery_enabled=false should Skip; got %+v", res)
	}
	if got := lister.called.Load(); got != 0 {
		t.Errorf("lister called %d times for disabled provider; want 0", got)
	}
}

// TestTryDiscoverOnNotFound_NoProviderMetaSkipped — calling for a
// provider without a meta row is fail-closed: skip, do not fabricate
// defaults. The 24h ticker is responsible for seeding meta rows; an
// on-404 trigger for an unseeded provider would be a wiring bug.
func TestTryDiscoverOnNotFound_NoProviderMetaSkipped(t *testing.T) {
	cs, _, ctx := freshStore(t)

	lister := &fakeCountingLister{
		fn: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return nil, nil
		},
	}
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{"openai": lister})

	res := runner.TryDiscoverOnNotFound(ctx, "openai", ListerCredentials{})
	if !res.Skipped {
		t.Errorf("missing ProviderMeta should Skip; got %+v", res)
	}
	if got := lister.called.Load(); got != 0 {
		t.Errorf("lister called %d times without meta row; want 0", got)
	}
}

// TestTryDiscoverOnNotFound_FailureStillStampsDebounce — when the
// lister errors, the debounce map IS still stamped. A storm of 404s
// from a genuinely-broken upstream must not produce a storm of
// lister calls; the persistent backoff state machine governs longer
// retry intervals, the in-memory debounce governs the next 5 min.
//
// Isolation: a naive version of this test would let DiscoverProvider's
// recordError side effect advance BackoffStep on the first failure,
// then the second call would be skipped by backoff (NextDiscoveryAfter
// = now + 1h) regardless of debounce — the assertion would be green
// for the wrong reason. To pin debounce as the sole gate that catches
// the second call, we reset ProviderMeta back to the no-backoff state
// after the first failure. Then only the in-memory debounce map can
// produce a Skipped result.
func TestTryDiscoverOnNotFound_FailureStillStampsDebounce(t *testing.T) {
	cs, _, ctx := freshStore(t)

	lister := &fakeCountingLister{
		fn: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return nil, context.DeadlineExceeded
		},
	}
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{"openai": lister})

	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	current := base
	runner.Clock = func() time.Time { return current }

	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider:         "openai",
		DiscoveryEnabled: true,
	}); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	res1 := runner.TryDiscoverOnNotFound(ctx, "openai", ListerCredentials{})
	if res1.Err == nil {
		t.Fatalf("expected lister error to propagate; got %+v", res1)
	}

	// Reset persistent backoff state to isolate debounce. After this,
	// ShouldRunForProvider returns true for any sane `now`, so a second
	// call can ONLY be skipped by the in-memory debounce map.
	if err := cs.SetProviderMeta(ctx, "openai", ProviderMeta{
		Provider:           "openai",
		DiscoveryEnabled:   true,
		BackoffStep:        0,
		NextDiscoveryAfter: time.Time{},
	}); err != nil {
		t.Fatalf("reset backoff: %v", err)
	}

	// Advance well into the 5-min window. With backoff cleared, only
	// the debounce stamp recorded during the failed first call can
	// produce a Skip.
	current = base.Add(30 * time.Second)
	res2 := runner.TryDiscoverOnNotFound(ctx, "openai", ListerCredentials{})
	if !res2.Skipped {
		t.Errorf("debounce-only gate failed: second call should Skip; got %+v", res2)
	}
	if got := lister.called.Load(); got != 1 {
		t.Errorf("lister called %d times after failure; want 1 (debounce only)", got)
	}

	// Sanity: past the debounce window, the same scenario fires again.
	current = base.Add(fiveMin + time.Second)
	res3 := runner.TryDiscoverOnNotFound(ctx, "openai", ListerCredentials{})
	if res3.Skipped {
		t.Errorf("post-debounce-window call should run; got Skipped")
	}
	if got := lister.called.Load(); got != 2 {
		t.Errorf("lister called %d times after debounce expiry; want 2", got)
	}
}
