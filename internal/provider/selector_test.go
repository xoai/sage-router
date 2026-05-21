package provider

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSelectEmptySelector(t *testing.T) {
	s := NewSelector()
	_, err := s.Select("openai", "gpt-4", nil, SelectDefault)
	if !errors.Is(err, ErrNoConnections) {
		t.Fatalf("expected ErrNoConnections, got %v", err)
	}
}

func TestSelectSingleConnection(t *testing.T) {
	s := NewSelector()
	c := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	s.Register(c)

	res, err := s.Select("openai", "gpt-4", nil, SelectDefault)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if res.Connection == nil {
		t.Fatal("Select returned nil connection")
	}
	if res.Connection.ID != "c1" {
		t.Errorf("expected c1, got %s", res.Connection.ID)
	}
}

func TestSelectPriorityOrdering(t *testing.T) {
	s := NewSelector()

	// Register higher priority (5) first, then lower (1).
	c5 := NewConnection("c5", "openai", "conn-5", 5, "api_key")
	c1 := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	s.Register(c5)
	s.Register(c1)

	res, err := s.Select("openai", "gpt-4", nil, SelectDefault)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if res.Connection.ID != "c1" {
		t.Errorf("expected c1 (priority 1), got %s (priority %d)",
			res.Connection.ID, res.Connection.Priority)
	}
}

func TestSelectExclusion(t *testing.T) {
	s := NewSelector()
	c1 := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	c2 := NewConnection("c2", "openai", "conn-2", 2, "api_key")
	s.Register(c1)
	s.Register(c2)

	res, err := s.Select("openai", "gpt-4", []string{"c1"}, SelectDefault)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if res.Connection.ID != "c2" {
		t.Errorf("expected c2 (c1 excluded), got %s", res.Connection.ID)
	}
}

func TestSelectAllExcluded(t *testing.T) {
	s := NewSelector()
	c1 := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	s.Register(c1)

	_, err := s.Select("openai", "gpt-4", []string{"c1"}, SelectDefault)
	if !errors.Is(err, ErrAllUnavailable) {
		t.Fatalf("expected ErrAllUnavailable, got %v", err)
	}
}

func TestSelectRateLimitedSkipped(t *testing.T) {
	s := NewSelector()

	c1 := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	c2 := NewConnection("c2", "openai", "conn-2", 2, "api_key")
	s.Register(c1)
	s.Register(c2)

	// Open c1's breaker (rate limited) so the selector skips it.
	if err := c1.MarkUsed(); err != nil {
		t.Fatalf("c1 MarkUsed: %v", err)
	}
	if err := c1.OpenBreaker(FailureRateLimit, 0, "gpt-4"); err != nil {
		t.Fatalf("c1 OpenBreaker: %v", err)
	}

	res, err := s.Select("openai", "gpt-4", nil, SelectDefault)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if res.Connection.ID != "c2" {
		t.Errorf("expected c2 (c1 in cooldown), got %s", res.Connection.ID)
	}
}

func TestSelectAllRateLimited(t *testing.T) {
	s := NewSelector()

	c1 := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	c2 := NewConnection("c2", "openai", "conn-2", 2, "api_key")
	s.Register(c1)
	s.Register(c2)

	// Open both breakers (rate limited).
	if err := c1.MarkUsed(); err != nil {
		t.Fatalf("c1 MarkUsed: %v", err)
	}
	if err := c1.OpenBreaker(FailureRateLimit, 0, "gpt-4"); err != nil {
		t.Fatalf("c1 OpenBreaker: %v", err)
	}

	if err := c2.MarkUsed(); err != nil {
		t.Fatalf("c2 MarkUsed: %v", err)
	}
	if err := c2.OpenBreaker(FailureRateLimit, 0, "gpt-4"); err != nil {
		t.Fatalf("c2 OpenBreaker: %v", err)
	}

	res, err := s.Select("openai", "gpt-4", nil, SelectDefault)
	if !errors.Is(err, ErrAllUnavailable) {
		t.Fatalf("expected ErrAllUnavailable, got %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil SelectResult even on ErrAllUnavailable")
	}
	if !res.AllRateLimited {
		t.Error("expected AllRateLimited=true")
	}
	if res.EarliestRetry.IsZero() {
		t.Error("expected non-zero EarliestRetry")
	}
}

func TestConnectionByID(t *testing.T) {
	s := NewSelector()

	c1 := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	s.Register(c1)

	got := s.ConnectionByID("c1")
	if got == nil {
		t.Fatal("ConnectionByID(c1) returned nil")
	}
	if got.ID != "c1" {
		t.Errorf("expected c1, got %s", got.ID)
	}

	// Non-existent ID.
	if s.ConnectionByID("missing") != nil {
		t.Error("ConnectionByID(missing): expected nil")
	}
}

func TestRemove(t *testing.T) {
	s := NewSelector()

	c1 := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	c2 := NewConnection("c2", "openai", "conn-2", 2, "api_key")
	s.Register(c1)
	s.Register(c2)

	s.Remove("c1")

	if s.ConnectionByID("c1") != nil {
		t.Error("c1 should have been removed")
	}

	res, err := s.Select("openai", "gpt-4", nil, SelectDefault)
	if err != nil {
		t.Fatalf("Select after remove: %v", err)
	}
	if res.Connection.ID != "c2" {
		t.Errorf("expected c2, got %s", res.Connection.ID)
	}
}

func TestRegisterReplacesSameID(t *testing.T) {
	s := NewSelector()

	c1 := NewConnection("c1", "openai", "conn-old", 10, "api_key")
	s.Register(c1)

	// Replace with same ID but different name and priority.
	c1New := NewConnection("c1", "openai", "conn-new", 1, "api_key")
	s.Register(c1New)

	got := s.ConnectionByID("c1")
	if got == nil {
		t.Fatal("ConnectionByID(c1) returned nil after replacement")
	}
	if got.Name != "conn-new" {
		t.Errorf("expected replaced name conn-new, got %s", got.Name)
	}
	if got.Priority != 1 {
		t.Errorf("expected replaced priority 1, got %d", got.Priority)
	}

	// Ensure there's only one connection for openai, not two.
	all := s.AllConnections("openai")
	if len(all) != 1 {
		t.Errorf("expected 1 connection after replacement, got %d", len(all))
	}
}

func TestSelectWrongProvider(t *testing.T) {
	s := NewSelector()

	c := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	s.Register(c)

	_, err := s.Select("anthropic", "claude-3", nil, SelectDefault)
	if !errors.Is(err, ErrNoConnections) {
		t.Fatalf("expected ErrNoConnections for wrong provider, got %v", err)
	}
}

// TestSelectorPriorityOrdering verifies that the connection with the lower
// priority number is selected first when multiple connections are registered.
func TestSelectorPriorityOrdering(t *testing.T) {
	s := NewSelector()

	// Register in reverse order to verify sort, not insertion order.
	c10 := NewConnection("c10", "openai", "conn-10", 10, "api_key")
	c3 := NewConnection("c3", "openai", "conn-3", 3, "api_key")
	c1 := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	s.Register(c10)
	s.Register(c3)
	s.Register(c1)

	res, err := s.Select("openai", "", nil, SelectDefault)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if res.Connection.ID != "c1" {
		t.Errorf("expected c1 (priority 1), got %s (priority %d)",
			res.Connection.ID, res.Connection.Priority)
	}
}

// TestSelectorExclusion verifies that excluded IDs are skipped during selection.
func TestSelectorExclusion(t *testing.T) {
	s := NewSelector()

	c1 := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	c2 := NewConnection("c2", "openai", "conn-2", 2, "api_key")
	c3 := NewConnection("c3", "openai", "conn-3", 3, "api_key")
	s.Register(c1)
	s.Register(c2)
	s.Register(c3)

	// Exclude the two best-priority connections.
	res, err := s.Select("openai", "", []string{"c1", "c2"}, SelectDefault)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if res.Connection.ID != "c3" {
		t.Errorf("expected c3 (c1 and c2 excluded), got %s", res.Connection.ID)
	}
}

// TestSelectorAllRateLimited verifies AllRateLimited flag and EarliestRetry
// when every connection is in cooldown.
func TestSelectorAllRateLimited(t *testing.T) {
	s := NewSelector()

	c1 := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	c2 := NewConnection("c2", "openai", "conn-2", 2, "api_key")
	s.Register(c1)
	s.Register(c2)

	// Open both breakers with distinct cooldowns (c1 sooner than c2) so the
	// EarliestRetry assertion below is deterministic — CooldownFor honors the
	// Retry-After verbatim for the rate_limit kind.
	if err := c1.MarkUsed(); err != nil {
		t.Fatalf("c1 MarkUsed: %v", err)
	}
	if err := c1.OpenBreaker(FailureRateLimit, 1*time.Second, "gpt-4"); err != nil {
		t.Fatalf("c1 OpenBreaker: %v", err)
	}

	if err := c2.MarkUsed(); err != nil {
		t.Fatalf("c2 MarkUsed: %v", err)
	}
	if err := c2.OpenBreaker(FailureRateLimit, 8*time.Second, "gpt-4"); err != nil {
		t.Fatalf("c2 OpenBreaker: %v", err)
	}

	res, err := s.Select("openai", "gpt-4", nil, SelectDefault)
	if !errors.Is(err, ErrAllUnavailable) {
		t.Fatalf("expected ErrAllUnavailable, got %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil SelectResult")
	}
	if !res.AllRateLimited {
		t.Error("expected AllRateLimited=true")
	}
	if res.EarliestRetry.IsZero() {
		t.Error("expected non-zero EarliestRetry")
	}

	// EarliestRetry should be the earlier cooldown (c1 — 1s Retry-After).
	// c1 cooldown < c2 cooldown, so earliest should be c1's.
	c1Until := c1.CooldownUntil()
	c2Until := c2.CooldownUntil()
	if c1Until.After(c2Until) {
		t.Errorf("c1 cooldown (%v) should be before c2 cooldown (%v)", c1Until, c2Until)
	}
	if !res.EarliestRetry.Equal(c1Until) {
		t.Errorf("EarliestRetry = %v, want %v (c1 cooldown)", res.EarliestRetry, c1Until)
	}
}

// TestSelectorRoundRobin verifies that the consecutive-uses tiebreaker in the
// selector spreads traffic across equal-priority connections. When one
// connection is Active (in-flight, higher consecutiveUses and higher state
// priority), the selector prefers the other Idle connection.
func TestSelectorRoundRobin(t *testing.T) {
	s := NewSelector()

	// Two connections with the same user priority.
	cA := NewConnection("cA", "openai", "conn-A", 1, "api_key")
	cB := NewConnection("cB", "openai", "conn-B", 1, "api_key")
	s.Register(cA)
	s.Register(cB)

	// When cA is Active (in-flight), the selector should prefer cB (Idle).
	if err := cA.MarkUsed(); err != nil {
		t.Fatalf("cA MarkUsed: %v", err)
	}

	res, err := s.Select("openai", "", nil, SelectDefault)
	if err != nil {
		t.Fatalf("Select while cA Active: %v", err)
	}
	if res.Connection.ID != "cB" {
		t.Errorf("expected cB when cA is Active, got %s", res.Connection.ID)
	}

	// Now also mark cB as Active; cA has uses=1, cB has uses=1.
	// Both Active means neither is "available" in the Idle sense, but the
	// selector should still pick the one with fewer state-priority or uses.
	// Actually, Active connections are not available, so this returns
	// ErrAllUnavailable. Return cA to Idle first.
	if err := cA.MarkSuccess(); err != nil {
		t.Fatalf("cA MarkSuccess: %v", err)
	}

	// cA is now Idle (uses=0), cB is still Idle (uses=0, never MarkUsed'd above
	// because we only Selected it, didn't MarkUsed). Both at 0 uses.
	// MarkUsed cB to make it Active, then Select should pick cA.
	if err := cB.MarkUsed(); err != nil {
		t.Fatalf("cB MarkUsed: %v", err)
	}

	res2, err := s.Select("openai", "", nil, SelectDefault)
	if err != nil {
		t.Fatalf("Select while cB Active: %v", err)
	}
	if res2.Connection.ID != "cA" {
		t.Errorf("expected cA when cB is Active, got %s", res2.Connection.ID)
	}

	// Return cB to Idle.
	if err := cB.MarkSuccess(); err != nil {
		t.Fatalf("cB MarkSuccess: %v", err)
	}

	// Simulate concurrent in-flight requests: mark cA as used (Active),
	// select again (should get cB), mark cB as used, now both Active,
	// select should fail with ErrAllUnavailable.
	if err := cA.MarkUsed(); err != nil {
		t.Fatalf("cA MarkUsed 2: %v", err)
	}
	res3, err := s.Select("openai", "", nil, SelectDefault)
	if err != nil {
		t.Fatalf("Select with cA Active: %v", err)
	}
	if res3.Connection.ID != "cB" {
		t.Errorf("expected cB, got %s", res3.Connection.ID)
	}

	if err := cB.MarkUsed(); err != nil {
		t.Fatalf("cB MarkUsed 2: %v", err)
	}
	_, err = s.Select("openai", "", nil, SelectDefault)
	if !errors.Is(err, ErrAllUnavailable) {
		t.Fatalf("expected ErrAllUnavailable when both Active, got %v", err)
	}
}

// TestSelectorNoConnections verifies that ErrNoConnections is returned when
// selecting for an unknown provider.
func TestSelectorNoConnections(t *testing.T) {
	s := NewSelector()

	_, err := s.Select("unknown-provider", "model", nil, SelectDefault)
	if !errors.Is(err, ErrNoConnections) {
		t.Fatalf("expected ErrNoConnections, got %v", err)
	}

	// Also verify with connections registered under a different provider.
	c := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	s.Register(c)

	_, err = s.Select("anthropic", "claude-3", nil, SelectDefault)
	if !errors.Is(err, ErrNoConnections) {
		t.Fatalf("expected ErrNoConnections for unregistered provider, got %v", err)
	}
}

// TestSelectorRegisterReplace verifies that registering a connection with an
// existing ID replaces the old one without duplicating entries.
func TestSelectorRegisterReplace(t *testing.T) {
	s := NewSelector()

	original := NewConnection("c1", "openai", "conn-old", 10, "api_key")
	s.Register(original)

	// Verify original is in place.
	got := s.ConnectionByID("c1")
	if got.Name != "conn-old" {
		t.Fatalf("expected conn-old, got %s", got.Name)
	}

	// Replace with same ID, different attributes.
	replacement := NewConnection("c1", "openai", "conn-new", 1, "oauth")
	s.Register(replacement)

	got = s.ConnectionByID("c1")
	if got == nil {
		t.Fatal("ConnectionByID(c1) returned nil after replacement")
	}
	if got.Name != "conn-new" {
		t.Errorf("expected replaced name conn-new, got %s", got.Name)
	}
	if got.Priority != 1 {
		t.Errorf("expected replaced priority 1, got %d", got.Priority)
	}
	if got.AuthType != "oauth" {
		t.Errorf("expected replaced authType oauth, got %s", got.AuthType)
	}

	// Only one connection should exist for the provider.
	all := s.AllConnections("openai")
	if len(all) != 1 {
		t.Errorf("expected 1 connection after replacement, got %d", len(all))
	}
}

// TestSelect_HalfOpenSingleTrialRaceSafe (AC8) pins the HALF_OPEN single-trial
// gate: when many goroutines concurrently Select the same HALF_OPEN
// connection, exactly one wins the trial slot (Select claims it via the
// compare-and-set in TryClaimHalfOpenTrial) and the rest get ErrAllUnavailable.
// Run under `go test -race`.
func TestSelect_HalfOpenSingleTrialRaceSafe(t *testing.T) {
	s := NewSelector()
	c := NewConnection("c1", "openai", "conn-1", 0, "api_key")
	s.Register(c)

	// Drive the breaker OPEN → HALF_OPEN so the connection is a probe candidate.
	if err := c.OpenBreaker(FailureTransient, 0, ""); err != nil {
		t.Fatalf("OpenBreaker: %v", err)
	}
	if err := c.ToHalfOpen(); err != nil {
		t.Fatalf("ToHalfOpen: %v", err)
	}

	const goroutines = 64
	var wg sync.WaitGroup
	var winners int32
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			res, err := s.Select("openai", "gpt-4", nil, SelectDefault)
			if err == nil && res != nil && res.Connection != nil {
				atomic.AddInt32(&winners, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&winners); got != 1 {
		t.Errorf("HALF_OPEN single-trial gate: %d goroutines won the pick, want exactly 1", got)
	}
}

// TestSelect_StrategyParameter (M3 T4 — the regression gate) pins that the new
// SelectStrategy parameter dispatches without disturbing selection: every
// strategy value routes through Select and returns a valid connection.
// SelectDefault must reproduce today's user-priority ordering exactly; the
// SelectP2C / SelectResetAware arms are placeholders here (T5/T6 fill them) so
// this test only asserts they dispatch — it does not pin their order.
func TestSelect_StrategyParameter(t *testing.T) {
	newSelector := func() *Selector {
		s := NewSelector()
		// Register out of priority order to prove the sort, not insertion.
		s.Register(NewConnection("c3", "openai", "conn-3", 3, "api_key"))
		s.Register(NewConnection("c1", "openai", "conn-1", 1, "api_key"))
		s.Register(NewConnection("c2", "openai", "conn-2", 2, "api_key"))
		return s
	}

	for _, strategy := range []SelectStrategy{SelectDefault, SelectP2C, SelectResetAware} {
		s := newSelector()
		res, err := s.Select("openai", "gpt-4", nil, strategy)
		if err != nil {
			t.Fatalf("strategy %d: Select: %v", strategy, err)
		}
		if res.Connection == nil {
			t.Fatalf("strategy %d: Select returned nil connection", strategy)
		}
	}

	// SelectDefault is the regression baseline — lowest user priority wins.
	s := newSelector()
	res, err := s.Select("openai", "gpt-4", nil, SelectDefault)
	if err != nil {
		t.Fatalf("SelectDefault: %v", err)
	}
	if res.Connection.ID != "c1" {
		t.Errorf("SelectDefault: expected c1 (priority 1), got %s", res.Connection.ID)
	}
}

// TestSelect_P2C (M3 T5 — AC6) pins the power-of-two-choices ordering: traffic
// spreads across equal candidates, the less-recently-used of the two sampled
// picks is ordered first, and <=1 candidate degrades to SelectDefault.
func TestSelect_P2C(t *testing.T) {
	t.Run("spreads traffic across equal candidates", func(t *testing.T) {
		s := NewSelector()
		// Three equal candidates, all never-used (zero LastUsedAt).
		s.Register(NewConnection("c1", "openai", "conn-1", 1, "api_key"))
		s.Register(NewConnection("c2", "openai", "conn-2", 1, "api_key"))
		s.Register(NewConnection("c3", "openai", "conn-3", 1, "api_key"))
		s.seedRNGForTest(42)

		counts := map[string]int{}
		const iters = 300
		for i := 0; i < iters; i++ {
			res, err := s.Select("openai", "gpt-4", nil, SelectP2C)
			if err != nil {
				t.Fatalf("iter %d: Select: %v", i, err)
			}
			counts[res.Connection.ID]++
		}
		if len(counts) < 2 {
			t.Fatalf("p2c sent all traffic to %v — expected a spread", counts)
		}
		for _, id := range []string{"c1", "c2", "c3"} {
			if counts[id] == 0 {
				t.Errorf("p2c never selected %s — counts=%v", id, counts)
			}
		}
	})

	t.Run("prefers less-recently-used of the two picks", func(t *testing.T) {
		s := NewSelector()
		// `older` is never used (zero LastUsedAt); `newer` is touched so its
		// LastUsedAt is recent, then returned to Idle so it stays selectable.
		s.Register(NewConnection("older", "openai", "conn-older", 1, "api_key"))
		newer := NewConnection("newer", "openai", "conn-newer", 1, "api_key")
		s.Register(newer)
		if err := newer.MarkUsed(); err != nil {
			t.Fatalf("newer MarkUsed: %v", err)
		}
		if err := newer.MarkSuccess(); err != nil {
			t.Fatalf("newer MarkSuccess: %v", err)
		}
		s.seedRNGForTest(1)

		// With exactly two candidates p2c samples both every time; the older
		// LastUsedAt must be ordered first regardless of the RNG draw.
		for i := 0; i < 30; i++ {
			res, err := s.Select("openai", "gpt-4", nil, SelectP2C)
			if err != nil {
				t.Fatalf("iter %d: Select: %v", i, err)
			}
			if res.Connection.ID != "older" {
				t.Fatalf("iter %d: p2c picked %s, want older (least-recently-used)", i, res.Connection.ID)
			}
		}
	})

	t.Run("single candidate degrades to SelectDefault", func(t *testing.T) {
		s := NewSelector()
		s.Register(NewConnection("only", "openai", "conn-only", 1, "api_key"))
		res, err := s.Select("openai", "gpt-4", nil, SelectP2C)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if res.Connection == nil || res.Connection.ID != "only" {
			t.Fatalf("p2c with one candidate: got %v, want only", res.Connection)
		}
	})
}
