package provider

import (
	"testing"
	"time"
)

func TestHealthChecker_CooldownRecovery(t *testing.T) {
	sel := NewSelector()
	c := NewConnection("c1", "anthropic", "test", 0, "apikey")
	sel.Register(c)

	// Put connection into cooldown with an already-expired timer.
	c.mu.Lock()
	c.state = StateCooldown
	c.cooldownUntil = time.Now().Add(-1 * time.Second)
	c.mu.Unlock()

	// Verify connection is not available (cooldown check uses time.Now inside IsAvailable,
	// but the cooldown is already expired so it should be available via IsAvailable).
	// The health checker should transition it back to Idle.
	hc := NewHealthChecker(sel, 100*time.Millisecond)
	hc.check() // Run a single check cycle.

	if c.State() != StateIdle {
		t.Fatalf("expected Idle after cooldown expiry, got %s", c.State())
	}
}

func TestHealthChecker_CooldownNotExpired(t *testing.T) {
	sel := NewSelector()
	c := NewConnection("c1", "anthropic", "test", 0, "apikey")
	sel.Register(c)

	// Put connection into cooldown with a future timer.
	c.mu.Lock()
	c.state = StateCooldown
	c.cooldownUntil = time.Now().Add(10 * time.Minute)
	c.mu.Unlock()

	hc := NewHealthChecker(sel, 100*time.Millisecond)
	hc.check()

	if c.State() != StateCooldown {
		t.Fatalf("expected Cooldown to remain, got %s", c.State())
	}
}

func TestHealthChecker_ErroredAutoRecovery(t *testing.T) {
	sel := NewSelector()
	c := NewConnection("c1", "openai", "test", 0, "apikey")
	sel.Register(c)

	// Put connection into errored state with lastUsedAt > 5 minutes ago.
	c.mu.Lock()
	c.state = StateErrored
	c.lastUsedAt = time.Now().Add(-6 * time.Minute)
	c.backoffLevel = 3
	c.mu.Unlock()

	hc := NewHealthChecker(sel, 100*time.Millisecond)
	hc.check()

	if c.State() != StateIdle {
		t.Fatalf("expected Idle after grace period, got %s", c.State())
	}
	if c.BackoffLevel() != 0 {
		t.Fatalf("expected backoff reset to 0, got %d", c.BackoffLevel())
	}
}

func TestHealthChecker_ErroredTooRecent(t *testing.T) {
	sel := NewSelector()
	c := NewConnection("c1", "openai", "test", 0, "apikey")
	sel.Register(c)

	// Errored but used recently — should NOT auto-recover.
	c.mu.Lock()
	c.state = StateErrored
	c.lastUsedAt = time.Now().Add(-1 * time.Minute)
	c.mu.Unlock()

	hc := NewHealthChecker(sel, 100*time.Millisecond)
	hc.check()

	if c.State() != StateErrored {
		t.Fatalf("expected Errored to remain (too recent), got %s", c.State())
	}
}

func TestHealthChecker_ModelLockCleanup(t *testing.T) {
	sel := NewSelector()
	c := NewConnection("c1", "anthropic", "test", 0, "apikey")
	sel.Register(c)

	// Add expired and active model locks.
	c.mu.Lock()
	c.modelLocks["expired-model"] = time.Now().Add(-1 * time.Second)
	c.modelLocks["active-model"] = time.Now().Add(10 * time.Minute)
	c.mu.Unlock()

	hc := NewHealthChecker(sel, 100*time.Millisecond)
	hc.check()

	c.mu.RLock()
	_, expiredExists := c.modelLocks["expired-model"]
	_, activeExists := c.modelLocks["active-model"]
	c.mu.RUnlock()

	if expiredExists {
		t.Fatal("expected expired model lock to be cleaned up")
	}
	if !activeExists {
		t.Fatal("expected active model lock to remain")
	}
}

func TestHealthChecker_StartStop(t *testing.T) {
	sel := NewSelector()
	hc := NewHealthChecker(sel, 50*time.Millisecond)
	hc.Start()
	time.Sleep(120 * time.Millisecond) // Let at least one tick run.
	hc.Stop()
	// If Stop() hangs, the test will timeout — that IS the test.
}
