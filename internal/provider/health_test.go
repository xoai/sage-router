package provider

import (
	"testing"
	"time"
)

// TestHealthChecker_OpenToHalfOpenOnCooldownExpiry: once an OPEN breaker's
// cooldown has elapsed, the health checker promotes it to HALF_OPEN so the
// Selector can run a single trial request (M2 spec §6 — timer-driven recovery).
func TestHealthChecker_OpenToHalfOpenOnCooldownExpiry(t *testing.T) {
	sel := NewSelector()
	c := NewConnection("c1", "anthropic", "test", 0, "apikey")
	sel.Register(c)

	c.SetFacetsForTest(BreakerOpen, AuthValid, LifecycleIdle)
	c.mu.Lock()
	c.cooldownUntil = time.Now().Add(-1 * time.Second) // already elapsed
	c.mu.Unlock()

	hc := NewHealthChecker(sel, 100*time.Millisecond)
	hc.check() // a single check cycle

	if c.Breaker() != BreakerHalfOpen {
		t.Fatalf("expected HALF_OPEN after cooldown expiry, got %s", c.Breaker())
	}
}

// TestHealthChecker_OpenStaysOpenBeforeCooldown: an OPEN breaker whose cooldown
// has not yet elapsed is left OPEN.
func TestHealthChecker_OpenStaysOpenBeforeCooldown(t *testing.T) {
	sel := NewSelector()
	c := NewConnection("c1", "anthropic", "test", 0, "apikey")
	sel.Register(c)

	c.SetFacetsForTest(BreakerOpen, AuthValid, LifecycleIdle)
	c.mu.Lock()
	c.cooldownUntil = time.Now().Add(10 * time.Minute)
	c.mu.Unlock()

	hc := NewHealthChecker(sel, 100*time.Millisecond)
	hc.check()

	if c.Breaker() != BreakerOpen {
		t.Fatalf("expected OPEN to remain before cooldown elapses, got %s", c.Breaker())
	}
}

// TestHealthChecker_HalfOpenLeftUntouched: the checker acts only on OPEN
// breakers — an already-HALF_OPEN connection (a trial in progress) is left
// alone, so a slow trial is not disturbed (AC7).
func TestHealthChecker_HalfOpenLeftUntouched(t *testing.T) {
	sel := NewSelector()
	c := NewConnection("c1", "anthropic", "test", 0, "apikey")
	sel.Register(c)

	c.SetFacetsForTest(BreakerHalfOpen, AuthValid, LifecycleIdle)
	c.mu.Lock()
	c.cooldownUntil = time.Now().Add(-1 * time.Second) // elapsed — irrelevant once HALF_OPEN
	c.mu.Unlock()

	hc := NewHealthChecker(sel, 100*time.Millisecond)
	hc.check()

	if c.Breaker() != BreakerHalfOpen {
		t.Fatalf("expected HALF_OPEN to remain untouched, got %s", c.Breaker())
	}
}

// TestHealthChecker_ModelLockCleanup: expired model-scoped locks are
// garbage-collected; active ones remain.
func TestHealthChecker_ModelLockCleanup(t *testing.T) {
	sel := NewSelector()
	c := NewConnection("c1", "anthropic", "test", 0, "apikey")
	sel.Register(c)

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
	time.Sleep(120 * time.Millisecond) // let at least one tick run
	hc.Stop()
	// If Stop() hangs, the test times out — that IS the test.
}
