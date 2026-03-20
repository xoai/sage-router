package provider

import (
	"errors"
	"testing"
)

func TestSelectEmptySelector(t *testing.T) {
	s := NewSelector()
	_, err := s.Select("openai", "gpt-4", nil)
	if !errors.Is(err, ErrNoConnections) {
		t.Fatalf("expected ErrNoConnections, got %v", err)
	}
}

func TestSelectSingleConnection(t *testing.T) {
	s := NewSelector()
	c := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	s.Register(c)

	res, err := s.Select("openai", "gpt-4", nil)
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

	res, err := s.Select("openai", "gpt-4", nil)
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

	res, err := s.Select("openai", "gpt-4", []string{"c1"})
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

	_, err := s.Select("openai", "gpt-4", []string{"c1"})
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

	// Put c1 into Cooldown: Idle -> Active -> RateLimited -> Cooldown
	if err := c1.MarkUsed(); err != nil {
		t.Fatalf("c1 MarkUsed: %v", err)
	}
	if err := c1.MarkRateLimited("gpt-4", 1); err != nil {
		t.Fatalf("c1 MarkRateLimited: %v", err)
	}

	res, err := s.Select("openai", "gpt-4", nil)
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

	// Put both into Cooldown.
	if err := c1.MarkUsed(); err != nil {
		t.Fatalf("c1 MarkUsed: %v", err)
	}
	if err := c1.MarkRateLimited("gpt-4", 1); err != nil {
		t.Fatalf("c1 MarkRateLimited: %v", err)
	}

	if err := c2.MarkUsed(); err != nil {
		t.Fatalf("c2 MarkUsed: %v", err)
	}
	if err := c2.MarkRateLimited("gpt-4", 2); err != nil {
		t.Fatalf("c2 MarkRateLimited: %v", err)
	}

	res, err := s.Select("openai", "gpt-4", nil)
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

	res, err := s.Select("openai", "gpt-4", nil)
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

	_, err := s.Select("anthropic", "claude-3", nil)
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

	res, err := s.Select("openai", "", nil)
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
	res, err := s.Select("openai", "", []string{"c1", "c2"})
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

	// Put both into Cooldown with different backoff levels.
	if err := c1.MarkUsed(); err != nil {
		t.Fatalf("c1 MarkUsed: %v", err)
	}
	if err := c1.MarkRateLimited("gpt-4", 0); err != nil {
		t.Fatalf("c1 MarkRateLimited: %v", err)
	}

	if err := c2.MarkUsed(); err != nil {
		t.Fatalf("c2 MarkUsed: %v", err)
	}
	if err := c2.MarkRateLimited("gpt-4", 3); err != nil {
		t.Fatalf("c2 MarkRateLimited: %v", err)
	}

	res, err := s.Select("openai", "gpt-4", nil)
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

	// EarliestRetry should be the earlier cooldown (c1 with backoff 0 = 1s).
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

	res, err := s.Select("openai", "", nil)
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

	res2, err := s.Select("openai", "", nil)
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
	res3, err := s.Select("openai", "", nil)
	if err != nil {
		t.Fatalf("Select with cA Active: %v", err)
	}
	if res3.Connection.ID != "cB" {
		t.Errorf("expected cB, got %s", res3.Connection.ID)
	}

	if err := cB.MarkUsed(); err != nil {
		t.Fatalf("cB MarkUsed 2: %v", err)
	}
	_, err = s.Select("openai", "", nil)
	if !errors.Is(err, ErrAllUnavailable) {
		t.Fatalf("expected ErrAllUnavailable when both Active, got %v", err)
	}
}

// TestSelectorNoConnections verifies that ErrNoConnections is returned when
// selecting for an unknown provider.
func TestSelectorNoConnections(t *testing.T) {
	s := NewSelector()

	_, err := s.Select("unknown-provider", "model", nil)
	if !errors.Is(err, ErrNoConnections) {
		t.Fatalf("expected ErrNoConnections, got %v", err)
	}

	// Also verify with connections registered under a different provider.
	c := NewConnection("c1", "openai", "conn-1", 1, "api_key")
	s.Register(c)

	_, err = s.Select("anthropic", "claude-3", nil)
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
