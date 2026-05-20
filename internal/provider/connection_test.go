package provider

import (
	"testing"
	"time"
)

// helper creates a fresh idle connection for tests.
func newTestConn(t *testing.T) *Connection {
	t.Helper()
	return NewConnection("test-id", "openai", "test-conn", 1, "api_key")
}

// assertFacets fails the test unless the connection's three health facets all
// match. Replaces the pre-M2 assertState(State) helper — the facets are the
// source of truth (cycle 20260520-m2-circuit-breaker).
func assertFacets(t *testing.T, c *Connection, breaker BreakerState, authState AuthState, lifecycle LifecycleState) {
	t.Helper()
	if got := c.Breaker(); got != breaker {
		t.Fatalf("breaker = %s, want %s", got, breaker)
	}
	if got := c.Auth(); got != authState {
		t.Fatalf("auth = %s, want %s", got, authState)
	}
	if got := c.Lifecycle(); got != lifecycle {
		t.Fatalf("lifecycle = %s, want %s", got, lifecycle)
	}
}

func TestNewConnectionStartsIdle(t *testing.T) {
	c := newTestConn(t)
	assertFacets(t, c, BreakerClosed, AuthValid, LifecycleIdle)
}

func TestSuccessPath_IdleActiveIdle(t *testing.T) {
	c := newTestConn(t)

	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	assertFacets(t, c, BreakerClosed, AuthValid, LifecycleActive)

	if err := c.MarkSuccess(); err != nil {
		t.Fatalf("MarkSuccess: %v", err)
	}
	assertFacets(t, c, BreakerClosed, AuthValid, LifecycleIdle)
}

func TestAuthExpiredPath(t *testing.T) {
	c := newTestConn(t)

	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	assertFacets(t, c, BreakerClosed, AuthValid, LifecycleActive)

	if err := c.MarkAuthExpired(); err != nil {
		t.Fatalf("MarkAuthExpired: %v", err)
	}
	// MarkAuthExpired drives the Auth facet to expired and releases the
	// lifecycle back to Idle; the breaker is untouched.
	assertFacets(t, c, BreakerClosed, AuthExpired, LifecycleIdle)
}

func TestCooldownToIdle(t *testing.T) {
	c := newTestConn(t)

	// Drive to an open breaker: MarkUsed → OpenBreaker(rate_limit).
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := c.OpenBreaker(FailureRateLimit, 0, "gpt-4"); err != nil {
		t.Fatalf("OpenBreaker: %v", err)
	}
	assertFacets(t, c, BreakerOpen, AuthValid, LifecycleIdle)

	if err := c.ResetCooldown(); err != nil {
		t.Fatalf("ResetCooldown: %v", err)
	}
	assertFacets(t, c, BreakerClosed, AuthValid, LifecycleIdle)
}

func TestErroredToIdle(t *testing.T) {
	c := newTestConn(t)

	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := c.OpenBreaker(FailureTransient, 0, ""); err != nil {
		t.Fatalf("OpenBreaker: %v", err)
	}
	assertFacets(t, c, BreakerOpen, AuthValid, LifecycleIdle)

	if err := c.ResetCooldown(); err != nil {
		t.Fatalf("ResetCooldown: %v", err)
	}
	assertFacets(t, c, BreakerClosed, AuthValid, LifecycleIdle)
}

func TestDisableAndEnable(t *testing.T) {
	// Disable must work from every starting point, and Enable must always
	// return the connection to a clean Idle (breaker reset, auth untouched).
	states := []struct {
		name  string
		setup func(t *testing.T) *Connection
	}{
		{
			name:  "from Idle",
			setup: func(t *testing.T) *Connection { return newTestConn(t) },
		},
		{
			name: "from Active",
			setup: func(t *testing.T) *Connection {
				c := newTestConn(t)
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("setup MarkUsed: %v", err)
				}
				return c
			},
		},
		{
			name: "from an open breaker (rate-limited)",
			setup: func(t *testing.T) *Connection {
				c := newTestConn(t)
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("setup MarkUsed: %v", err)
				}
				if err := c.OpenBreaker(FailureRateLimit, 0, "gpt-4"); err != nil {
					t.Fatalf("setup OpenBreaker: %v", err)
				}
				return c
			},
		},
		{
			name: "from an open breaker (transient failure)",
			setup: func(t *testing.T) *Connection {
				c := newTestConn(t)
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("setup MarkUsed: %v", err)
				}
				if err := c.OpenBreaker(FailureTransient, 0, ""); err != nil {
					t.Fatalf("setup OpenBreaker: %v", err)
				}
				return c
			},
		},
	}

	for _, tt := range states {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.setup(t)

			if err := c.Disable(); err != nil {
				t.Fatalf("Disable: %v", err)
			}
			if got := c.Lifecycle(); got != LifecycleDisabled {
				t.Fatalf("after Disable: lifecycle = %s, want disabled", got)
			}

			if err := c.Enable(); err != nil {
				t.Fatalf("Enable: %v", err)
			}
			// Enable produces a fully-clean Idle connection — the breaker is
			// reset to CLOSED even when it was OPEN before the disable.
			assertFacets(t, c, BreakerClosed, AuthValid, LifecycleIdle)
		})
	}
}

func TestInvalidTransition_MarkSuccessFromIdle(t *testing.T) {
	c := newTestConn(t)
	assertFacets(t, c, BreakerClosed, AuthValid, LifecycleIdle)

	err := c.MarkSuccess()
	if err == nil {
		t.Fatal("MarkSuccess from Idle: expected error, got nil")
	}
}

func TestBackoffLevelResetsOnSuccess(t *testing.T) {
	c := newTestConn(t)

	// Escalate the backoff level. Each OpenBreaker bumps it by one; the
	// breaker must pass through HALF_OPEN before it can open again.
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := c.OpenBreaker(FailureRateLimit, 0, "gpt-4"); err != nil {
			t.Fatalf("OpenBreaker #%d: %v", i+1, err)
		}
		if i < 2 {
			if err := c.ToHalfOpen(); err != nil {
				t.Fatalf("ToHalfOpen #%d: %v", i+1, err)
			}
		}
	}
	if c.BackoffLevel() != 3 {
		t.Fatalf("expected backoff 3 after 3 OpenBreaker calls, got %d", c.BackoffLevel())
	}

	// ResetCooldown zeroes the backoff.
	if err := c.ResetCooldown(); err != nil {
		t.Fatalf("ResetCooldown: %v", err)
	}
	if c.BackoffLevel() != 0 {
		t.Fatalf("expected backoff 0 after ResetCooldown, got %d", c.BackoffLevel())
	}

	// A full success cycle also resets the backoff. Re-escalate, then drive
	// a use→success round and confirm MarkSuccess cleared it.
	if err := c.OpenBreaker(FailureRateLimit, 0, "gpt-4"); err != nil {
		t.Fatalf("re-OpenBreaker: %v", err)
	}
	if c.BackoffLevel() == 0 {
		t.Fatalf("expected a non-zero backoff after OpenBreaker")
	}
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := c.MarkSuccess(); err != nil {
		t.Fatalf("MarkSuccess: %v", err)
	}
	if c.BackoffLevel() != 0 {
		t.Fatalf("expected backoff 0 after MarkSuccess, got %d", c.BackoffLevel())
	}
}

func TestConsecutiveUsesIncrementsAndResets(t *testing.T) {
	c := newTestConn(t)

	if c.ConsecutiveUses() != 0 {
		t.Fatalf("initial ConsecutiveUses: expected 0, got %d", c.ConsecutiveUses())
	}

	// First use.
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed 1: %v", err)
	}
	if c.ConsecutiveUses() != 1 {
		t.Fatalf("after 1st MarkUsed: expected 1, got %d", c.ConsecutiveUses())
	}

	// Complete and use again.
	if err := c.MarkSuccess(); err != nil {
		t.Fatalf("MarkSuccess: %v", err)
	}
	if c.ConsecutiveUses() != 0 {
		t.Fatalf("after MarkSuccess: expected 0, got %d", c.ConsecutiveUses())
	}

	// Two consecutive uses without success.
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed 2: %v", err)
	}
	// Need to go back to Idle to MarkUsed again: simulate success first.
	if err := c.MarkSuccess(); err != nil {
		t.Fatalf("MarkSuccess 2: %v", err)
	}
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed 3: %v", err)
	}
	if c.ConsecutiveUses() != 1 {
		t.Fatalf("after 3rd MarkUsed (reset happened): expected 1, got %d", c.ConsecutiveUses())
	}
}

func TestConnectionFields(t *testing.T) {
	c := NewConnection("c1", "anthropic", "My Connection", 5, "oauth")
	if c.ID != "c1" {
		t.Errorf("ID: got %q, want %q", c.ID, "c1")
	}
	if c.Provider != "anthropic" {
		t.Errorf("Provider: got %q, want %q", c.Provider, "anthropic")
	}
	if c.Name != "My Connection" {
		t.Errorf("Name: got %q, want %q", c.Name, "My Connection")
	}
	if c.Priority != 5 {
		t.Errorf("Priority: got %d, want %d", c.Priority, 5)
	}
	if c.AuthType != "oauth" {
		t.Errorf("AuthType: got %q, want %q", c.AuthType, "oauth")
	}
}

// TestConnectionLifecyclePaths is a table-driven test verifying the four main
// transition paths through the connection's facets.
func TestConnectionLifecyclePaths(t *testing.T) {
	type facets struct {
		breaker   BreakerState
		auth      AuthState
		lifecycle LifecycleState
	}
	tests := []struct {
		name   string
		steps  func(t *testing.T, c *Connection)
		expect facets
	}{
		{
			name: "success path: Idle -> Active -> Idle",
			steps: func(t *testing.T, c *Connection) {
				t.Helper()
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("MarkUsed: %v", err)
				}
				assertFacets(t, c, BreakerClosed, AuthValid, LifecycleActive)
				if err := c.MarkSuccess(); err != nil {
					t.Fatalf("MarkSuccess: %v", err)
				}
			},
			expect: facets{BreakerClosed, AuthValid, LifecycleIdle},
		},
		{
			name: "rate-limit path: Idle -> Active -> breaker OPEN",
			steps: func(t *testing.T, c *Connection) {
				t.Helper()
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("MarkUsed: %v", err)
				}
				assertFacets(t, c, BreakerClosed, AuthValid, LifecycleActive)
				if err := c.OpenBreaker(FailureRateLimit, 0, "gpt-4"); err != nil {
					t.Fatalf("OpenBreaker: %v", err)
				}
			},
			expect: facets{BreakerOpen, AuthValid, LifecycleIdle},
		},
		{
			name: "transient-failure path: Idle -> Active -> breaker OPEN",
			steps: func(t *testing.T, c *Connection) {
				t.Helper()
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("MarkUsed: %v", err)
				}
				assertFacets(t, c, BreakerClosed, AuthValid, LifecycleActive)
				if err := c.OpenBreaker(FailureTransient, 0, ""); err != nil {
					t.Fatalf("OpenBreaker: %v", err)
				}
			},
			expect: facets{BreakerOpen, AuthValid, LifecycleIdle},
		},
		{
			name: "auth-failure path: Idle -> Active -> AuthExpired",
			steps: func(t *testing.T, c *Connection) {
				t.Helper()
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("MarkUsed: %v", err)
				}
				assertFacets(t, c, BreakerClosed, AuthValid, LifecycleActive)
				if err := c.MarkAuthExpired(); err != nil {
					t.Fatalf("MarkAuthExpired: %v", err)
				}
			},
			expect: facets{BreakerClosed, AuthExpired, LifecycleIdle},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewConnection("td-id", "openai", "td-conn", 1, "api_key")
			assertFacets(t, c, BreakerClosed, AuthValid, LifecycleIdle)
			tt.steps(t, c)
			assertFacets(t, c, tt.expect.breaker, tt.expect.auth, tt.expect.lifecycle)
		})
	}
}

// TestConnectionConsecutiveUses verifies the counter increments on MarkUsed
// and resets on MarkSuccess.
func TestConnectionConsecutiveUses(t *testing.T) {
	c := NewConnection("cu-id", "openai", "cu-conn", 1, "api_key")

	if c.ConsecutiveUses() != 0 {
		t.Fatalf("initial ConsecutiveUses: expected 0, got %d", c.ConsecutiveUses())
	}

	// First use increments.
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed 1: %v", err)
	}
	if c.ConsecutiveUses() != 1 {
		t.Fatalf("after 1st MarkUsed: expected 1, got %d", c.ConsecutiveUses())
	}

	// MarkSuccess resets to zero.
	if err := c.MarkSuccess(); err != nil {
		t.Fatalf("MarkSuccess 1: %v", err)
	}
	if c.ConsecutiveUses() != 0 {
		t.Fatalf("after MarkSuccess: expected 0, got %d", c.ConsecutiveUses())
	}

	// Use twice in sequence (with success in between to return to Idle).
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed 2: %v", err)
	}
	if c.ConsecutiveUses() != 1 {
		t.Fatalf("after 2nd MarkUsed: expected 1, got %d", c.ConsecutiveUses())
	}

	if err := c.MarkSuccess(); err != nil {
		t.Fatalf("MarkSuccess 2: %v", err)
	}
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed 3: %v", err)
	}
	// After MarkSuccess reset + another use, counter should be 1 again.
	if c.ConsecutiveUses() != 1 {
		t.Fatalf("after 3rd MarkUsed: expected 1, got %d", c.ConsecutiveUses())
	}
}

// ---------------------------------------------------------------------------
// Cycle 20260516-connection-runtime-state — snapshot accessor tests.
// Verify that ModelDenylistSnapshot + ModelLocksSnapshot:
//   - return an independent copy (mutating the result doesn't affect the conn)
//   - exclude entries with expired expiries (match the live-filter semantics)
//   - return a non-nil empty map when there are no active entries
//   - are race-free under concurrent state transitions

func TestConnection_ModelDenylistSnapshot(t *testing.T) {
	c := NewConnection("conn-1", "openai", "primary", 0, "subscription")

	// Mix of active + expired entries via the test helper.
	c.addModelDenylistFor("gpt-5-nano", 1*time.Hour)
	c.addModelDenylistFor("gpt-4o", 1*time.Hour)
	c.addModelDenylistFor("expired-model", -1*time.Hour) // already expired

	snap := c.ModelDenylistSnapshot()

	if _, ok := snap["gpt-5-nano"]; !ok {
		t.Errorf("expected gpt-5-nano in snapshot (active entry)")
	}
	if _, ok := snap["gpt-4o"]; !ok {
		t.Errorf("expected gpt-4o in snapshot (active entry)")
	}
	if _, ok := snap["expired-model"]; ok {
		t.Errorf("expected expired-model NOT in snapshot (expired entry should be excluded)")
	}
	if len(snap) != 2 {
		t.Errorf("expected exactly 2 active entries, got %d: %v", len(snap), snap)
	}

	// Mutating the snapshot must not affect the connection.
	delete(snap, "gpt-5-nano")
	snap["bogus"] = time.Now().Add(1 * time.Hour)
	snap2 := c.ModelDenylistSnapshot()
	if _, ok := snap2["gpt-5-nano"]; !ok {
		t.Errorf("snapshot mutation leaked into connection — gpt-5-nano removed")
	}
	if _, ok := snap2["bogus"]; ok {
		t.Errorf("snapshot mutation leaked into connection — bogus added")
	}
}

func TestConnection_ModelLocksSnapshot(t *testing.T) {
	c := NewConnection("conn-1", "openai", "primary", 0, "subscription")
	_ = c.MarkUsed()
	// OpenBreaker with a non-empty model records a per-model lock.
	if err := c.OpenBreaker(FailureRateLimit, 0, "gpt-5-nano"); err != nil {
		t.Fatalf("OpenBreaker: %v", err)
	}

	snap := c.ModelLocksSnapshot()
	if expiry, ok := snap["gpt-5-nano"]; !ok {
		t.Errorf("expected gpt-5-nano in lock snapshot after OpenBreaker")
	} else if !time.Now().Before(expiry) {
		t.Errorf("expected lock expiry in the future, got %v", expiry)
	}

	// Snapshot is a copy.
	snap["fake-model"] = time.Now().Add(1 * time.Hour)
	snap2 := c.ModelLocksSnapshot()
	if _, ok := snap2["fake-model"]; ok {
		t.Errorf("snapshot mutation leaked into connection")
	}
}

func TestConnection_SnapshotExcludesExpired_Boundary(t *testing.T) {
	c := NewConnection("conn-1", "openai", "primary", 0, "subscription")

	// Use addModelDenylistFor with explicit TTLs spanning the boundary.
	// Note: the helper uses time.Now() + ttl, so a negative TTL yields a past expiry.
	c.addModelDenylistFor("just-expired", -1*time.Nanosecond)
	c.addModelDenylistFor("barely-active", 1*time.Second)

	snap := c.ModelDenylistSnapshot()
	if _, ok := snap["just-expired"]; ok {
		t.Errorf("just-expired entry should be excluded from snapshot")
	}
	if _, ok := snap["barely-active"]; !ok {
		t.Errorf("barely-active entry should be included in snapshot")
	}
}

func TestConnection_SnapshotConcurrentWithTransition(t *testing.T) {
	// go test -race detector confirms no data race between snapshot reads
	// and state-mutating writes (RecordModelRejection).
	c := NewConnection("conn-1", "openai", "primary", 0, "subscription")
	_ = c.MarkUsed()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			c.RecordModelRejection("model-" + string(rune('a'+i%26)))
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		_ = c.ModelDenylistSnapshot()
		_ = c.ModelLocksSnapshot()
	}
	<-done
}

func TestConnection_SnapshotEmptyReturnsNonNil(t *testing.T) {
	c := NewConnection("conn-1", "openai", "primary", 0, "subscription")
	// Fresh connection — no denylist or locks.
	dn := c.ModelDenylistSnapshot()
	lk := c.ModelLocksSnapshot()
	if dn == nil {
		t.Errorf("ModelDenylistSnapshot should return empty non-nil map, got nil")
	}
	if lk == nil {
		t.Errorf("ModelLocksSnapshot should return empty non-nil map, got nil")
	}
	if len(dn) != 0 || len(lk) != 0 {
		t.Errorf("expected empty snapshots, got denylist=%v locks=%v", dn, lk)
	}
}
