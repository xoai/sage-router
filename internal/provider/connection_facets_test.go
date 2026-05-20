package provider

import (
	"errors"
	"testing"
	"time"
)

// T5 (cycle 20260520-m2-circuit-breaker) — the new facet method surface on
// Connection. These land alongside the old State enum / Mark* methods; T6
// rewrites the old surface as shims and T14 deletes it.

func TestConnection_FacetDefaults(t *testing.T) {
	c := NewConnection("c1", "openai", "c1", 0, "apikey")
	if c.Breaker() != BreakerClosed {
		t.Errorf("Breaker() = %q, want closed", c.Breaker())
	}
	if c.Auth() != AuthValid {
		t.Errorf("Auth() = %q, want valid", c.Auth())
	}
	if c.Lifecycle() != LifecycleIdle {
		t.Errorf("Lifecycle() = %q, want idle", c.Lifecycle())
	}
}

// TestConnection_Selectable pins the §1.4 selectability rule across facet
// combinations — including the orthogonal cases the old 8-state enum could not
// express (a valid credential AND an open breaker; an expired credential AND a
// closed breaker).
func TestConnection_Selectable(t *testing.T) {
	tests := []struct {
		name      string
		breaker   BreakerState
		auth      AuthState
		lifecycle LifecycleState
		want      bool
	}{
		{"closed+valid+idle is selectable", BreakerClosed, AuthValid, LifecycleIdle, true},
		{"closed+valid+active is NOT selectable (in-flight)", BreakerClosed, AuthValid, LifecycleActive, false},
		{"half_open+valid+idle is selectable", BreakerHalfOpen, AuthValid, LifecycleIdle, true},
		{"open breaker not selectable (auth still valid — orthogonal)", BreakerOpen, AuthValid, LifecycleIdle, false},
		{"expired auth not selectable (breaker still closed — orthogonal)", BreakerClosed, AuthExpired, LifecycleIdle, false},
		{"refreshing auth not selectable", BreakerClosed, AuthRefreshing, LifecycleIdle, false},
		{"disabled lifecycle not selectable", BreakerClosed, AuthValid, LifecycleDisabled, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewConnection("c1", "openai", "c1", 0, "apikey")
			c.SetFacetsForTest(tt.breaker, tt.auth, tt.lifecycle)
			if got := c.Selectable(""); got != tt.want {
				t.Errorf("Selectable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestConnection_SelectableModelLock verifies the model-scoped lock makes a
// connection unselectable for that model only, not the whole connection.
func TestConnection_SelectableModelLock(t *testing.T) {
	c := NewConnection("c1", "openai", "c1", 0, "apikey")
	if err := c.OpenBreaker(FailureRateLimit, time.Hour, "gpt-4"); err != nil {
		t.Fatalf("OpenBreaker: %v", err)
	}
	// OpenBreaker also set the breaker OPEN; reset just the breaker so the
	// model-lock check is what is under test.
	c.SetFacetsForTest(BreakerClosed, AuthValid, LifecycleIdle)
	if c.Selectable("gpt-4") {
		t.Errorf("Selectable(gpt-4) = true, want false (model is rate-limit locked)")
	}
	if !c.Selectable("gpt-3.5") {
		t.Errorf("Selectable(gpt-3.5) = false, want true (a different, unlocked model)")
	}
}

// TestConnection_OpenBreaker pins OpenBreaker: it opens the breaker, records
// the failure kind, escalates backoff, sets cooldownUntil per CooldownFor, and
// clears an Active lifecycle.
func TestConnection_OpenBreaker(t *testing.T) {
	t.Run("rate_limit honors Retry-After", func(t *testing.T) {
		c := NewConnection("c1", "openai", "c1", 0, "apikey")
		before := time.Now()
		if err := c.OpenBreaker(FailureRateLimit, 90*time.Second, "gpt-4"); err != nil {
			t.Fatalf("OpenBreaker: %v", err)
		}
		if c.Breaker() != BreakerOpen {
			t.Errorf("Breaker() = %q, want open", c.Breaker())
		}
		if c.FailureKind() != FailureRateLimit {
			t.Errorf("FailureKind() = %q, want rate_limit", c.FailureKind())
		}
		if d := c.CooldownUntil().Sub(before); d < 89*time.Second || d > 91*time.Second {
			t.Errorf("cooldown = %v, want ~90s (±1s)", d)
		}
	})

	t.Run("transient uses the computed cooldown", func(t *testing.T) {
		c := NewConnection("c2", "openai", "c2", 0, "apikey")
		before := time.Now()
		if err := c.OpenBreaker(FailureTransient, 0, ""); err != nil {
			t.Fatalf("OpenBreaker: %v", err)
		}
		// backoffLevel escalates 0→1; CooldownFor(transient,1,0) = 2s.
		if d := c.CooldownUntil().Sub(before); d < 1*time.Second || d > 3*time.Second {
			t.Errorf("cooldown = %v, want ~2s (±1s)", d)
		}
	})

	t.Run("clears an Active lifecycle", func(t *testing.T) {
		c := NewConnection("c3", "openai", "c3", 0, "apikey")
		c.SetFacetsForTest(BreakerClosed, AuthValid, LifecycleActive)
		if err := c.OpenBreaker(FailureTransient, 0, ""); err != nil {
			t.Fatalf("OpenBreaker: %v", err)
		}
		if c.Lifecycle() != LifecycleIdle {
			t.Errorf("Lifecycle() = %q, want idle (the failed request ended)", c.Lifecycle())
		}
	})

	t.Run("rejects re-opening an already-open breaker", func(t *testing.T) {
		c := NewConnection("c4", "openai", "c4", 0, "apikey")
		if err := c.OpenBreaker(FailureTransient, 0, ""); err != nil {
			t.Fatalf("first OpenBreaker: %v", err)
		}
		if err := c.OpenBreaker(FailureTransient, 0, ""); !errors.Is(err, ErrTransitionRejected) {
			t.Errorf("re-OpenBreaker error = %v, want ErrTransitionRejected", err)
		}
	})
}

// TestConnection_ToHalfOpen pins the timer-driven OPEN→HALF_OPEN move.
func TestConnection_ToHalfOpen(t *testing.T) {
	c := NewConnection("c1", "openai", "c1", 0, "apikey")
	if err := c.ToHalfOpen(); !errors.Is(err, ErrTransitionRejected) {
		t.Errorf("ToHalfOpen from closed: err = %v, want ErrTransitionRejected", err)
	}
	if err := c.OpenBreaker(FailureTransient, 0, ""); err != nil {
		t.Fatalf("OpenBreaker: %v", err)
	}
	if err := c.ToHalfOpen(); err != nil {
		t.Errorf("ToHalfOpen from open: %v", err)
	}
	if c.Breaker() != BreakerHalfOpen {
		t.Errorf("Breaker() = %q, want half_open", c.Breaker())
	}
}

// TestConnection_HalfOpenTrialGate pins the single-trial gate: a CLOSED
// connection is claimable without consuming a slot; a HALF_OPEN connection
// admits exactly one claim until released; release is idempotent (AC9).
func TestConnection_HalfOpenTrialGate(t *testing.T) {
	c := NewConnection("c1", "openai", "c1", 0, "apikey")

	if !c.TryClaimHalfOpenTrial("") || !c.TryClaimHalfOpenTrial("") {
		t.Errorf("a CLOSED connection must be claimable without consuming a trial slot")
	}

	if err := c.OpenBreaker(FailureTransient, 0, ""); err != nil {
		t.Fatalf("OpenBreaker: %v", err)
	}
	if err := c.ToHalfOpen(); err != nil {
		t.Fatalf("ToHalfOpen: %v", err)
	}

	if !c.TryClaimHalfOpenTrial("") {
		t.Errorf("first HALF_OPEN claim must win")
	}
	if c.TryClaimHalfOpenTrial("") {
		t.Errorf("second HALF_OPEN claim must lose — the trial slot is taken")
	}

	c.ReleaseHalfOpenTrial()
	c.ReleaseHalfOpenTrial() // idempotent — a second release must be a no-op
	if !c.TryClaimHalfOpenTrial("") {
		t.Errorf("after release the HALF_OPEN slot must be claimable again")
	}
}
