package provider

import (
	"errors"
	"testing"
)

// helper creates a fresh idle connection for tests.
func newTestConn(t *testing.T) *Connection {
	t.Helper()
	return NewConnection("test-id", "openai", "test-conn", 1, "api_key")
}

func assertState(t *testing.T, c *Connection, want State) {
	t.Helper()
	if got := c.State(); got != want {
		t.Fatalf("expected state %s, got %s", want, got)
	}
}

func TestNewConnectionStartsIdle(t *testing.T) {
	c := newTestConn(t)
	assertState(t, c, StateIdle)
}

func TestSuccessPath_IdleActiveIdle(t *testing.T) {
	c := newTestConn(t)

	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	assertState(t, c, StateActive)

	if err := c.MarkSuccess(); err != nil {
		t.Fatalf("MarkSuccess: %v", err)
	}
	assertState(t, c, StateIdle)
}

func TestRateLimitPath_IdleActiveCooldown(t *testing.T) {
	c := newTestConn(t)

	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	assertState(t, c, StateActive)

	if err := c.MarkRateLimited("gpt-4", 1); err != nil {
		t.Fatalf("MarkRateLimited: %v", err)
	}
	assertState(t, c, StateCooldown)
}

func TestErrorPath_IdleActiveErrored(t *testing.T) {
	c := newTestConn(t)

	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	assertState(t, c, StateActive)

	if err := c.MarkErrored(errors.New("upstream timeout")); err != nil {
		t.Fatalf("MarkErrored: %v", err)
	}
	assertState(t, c, StateErrored)
}

func TestAuthExpiredPath(t *testing.T) {
	c := newTestConn(t)

	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	assertState(t, c, StateActive)

	if err := c.MarkAuthExpired(); err != nil {
		t.Fatalf("MarkAuthExpired: %v", err)
	}
	assertState(t, c, StateAuthExpired)
}

func TestCooldownToIdle(t *testing.T) {
	c := newTestConn(t)

	// Drive to Cooldown: Idle -> Active -> RateLimited -> Cooldown
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := c.MarkRateLimited("gpt-4", 0); err != nil {
		t.Fatalf("MarkRateLimited: %v", err)
	}
	assertState(t, c, StateCooldown)

	if err := c.ResetCooldown(); err != nil {
		t.Fatalf("ResetCooldown: %v", err)
	}
	assertState(t, c, StateIdle)
}

func TestErroredToIdle(t *testing.T) {
	c := newTestConn(t)

	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := c.MarkErrored(errors.New("oops")); err != nil {
		t.Fatalf("MarkErrored: %v", err)
	}
	assertState(t, c, StateErrored)

	if err := c.ResetCooldown(); err != nil {
		t.Fatalf("ResetCooldown: %v", err)
	}
	assertState(t, c, StateIdle)
}

func TestDisableAndEnable(t *testing.T) {
	// Test Disable from various states.
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
			name: "from Cooldown",
			setup: func(t *testing.T) *Connection {
				c := newTestConn(t)
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("setup MarkUsed: %v", err)
				}
				if err := c.MarkRateLimited("gpt-4", 0); err != nil {
					t.Fatalf("setup MarkRateLimited: %v", err)
				}
				return c
			},
		},
		{
			name: "from Errored",
			setup: func(t *testing.T) *Connection {
				c := newTestConn(t)
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("setup MarkUsed: %v", err)
				}
				if err := c.MarkErrored(errors.New("fail")); err != nil {
					t.Fatalf("setup MarkErrored: %v", err)
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
			assertState(t, c, StateDisabled)

			if err := c.Enable(); err != nil {
				t.Fatalf("Enable: %v", err)
			}
			assertState(t, c, StateIdle)
		})
	}
}

func TestInvalidTransition_MarkSuccessFromIdle(t *testing.T) {
	c := newTestConn(t)
	assertState(t, c, StateIdle)

	err := c.MarkSuccess()
	if err == nil {
		t.Fatal("MarkSuccess from Idle: expected error, got nil")
	}
}

func TestInvalidTransition_MarkRateLimitedFromIdle(t *testing.T) {
	c := newTestConn(t)
	assertState(t, c, StateIdle)

	err := c.MarkRateLimited("gpt-4", 1)
	if err == nil {
		t.Fatal("MarkRateLimited from Idle: expected error, got nil")
	}
}

func TestBackoffLevelResetsOnSuccess(t *testing.T) {
	c := newTestConn(t)

	// Drive to Cooldown with backoff 3.
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := c.MarkRateLimited("gpt-4", 3); err != nil {
		t.Fatalf("MarkRateLimited: %v", err)
	}
	if c.BackoffLevel() != 3 {
		t.Fatalf("expected backoff 3, got %d", c.BackoffLevel())
	}

	// Reset and go through a success cycle.
	if err := c.ResetCooldown(); err != nil {
		t.Fatalf("ResetCooldown: %v", err)
	}
	if c.BackoffLevel() != 0 {
		t.Fatalf("expected backoff 0 after ResetCooldown, got %d", c.BackoffLevel())
	}

	// Full success path also resets.
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

// TestConnectionStateTransitions is a table-driven test verifying the four
// main state-transition paths through the connection lifecycle.
func TestConnectionStateTransitions(t *testing.T) {
	tests := []struct {
		name   string
		steps  func(t *testing.T, c *Connection)
		expect State
	}{
		{
			name: "success path: Idle -> Active -> Idle",
			steps: func(t *testing.T, c *Connection) {
				t.Helper()
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("MarkUsed: %v", err)
				}
				assertState(t, c, StateActive)
				if err := c.MarkSuccess(); err != nil {
					t.Fatalf("MarkSuccess: %v", err)
				}
			},
			expect: StateIdle,
		},
		{
			name: "429 path: Idle -> Active -> RateLimited -> Cooldown",
			steps: func(t *testing.T, c *Connection) {
				t.Helper()
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("MarkUsed: %v", err)
				}
				assertState(t, c, StateActive)
				if err := c.MarkRateLimited("gpt-4", 1); err != nil {
					t.Fatalf("MarkRateLimited: %v", err)
				}
			},
			expect: StateCooldown,
		},
		{
			name: "5xx path: Idle -> Active -> Errored",
			steps: func(t *testing.T, c *Connection) {
				t.Helper()
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("MarkUsed: %v", err)
				}
				assertState(t, c, StateActive)
				if err := c.MarkErrored(errors.New("internal server error")); err != nil {
					t.Fatalf("MarkErrored: %v", err)
				}
			},
			expect: StateErrored,
		},
		{
			name: "401 path: Idle -> Active -> AuthExpired",
			steps: func(t *testing.T, c *Connection) {
				t.Helper()
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("MarkUsed: %v", err)
				}
				assertState(t, c, StateActive)
				if err := c.MarkAuthExpired(); err != nil {
					t.Fatalf("MarkAuthExpired: %v", err)
				}
			},
			expect: StateAuthExpired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewConnection("td-id", "openai", "td-conn", 1, "api_key")
			assertState(t, c, StateIdle)
			tt.steps(t, c)
			assertState(t, c, tt.expect)
		})
	}
}

// TestConnectionIsAvailable verifies availability checks based on state.
func TestConnectionIsAvailable(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T) *Connection
		model     string
		available bool
	}{
		{
			name: "Idle connection is available",
			setup: func(t *testing.T) *Connection {
				return NewConnection("a1", "openai", "c", 1, "api_key")
			},
			model:     "gpt-4",
			available: true,
		},
		{
			name: "Active connection is not available",
			setup: func(t *testing.T) *Connection {
				c := NewConnection("a2", "openai", "c", 1, "api_key")
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("setup MarkUsed: %v", err)
				}
				return c
			},
			model:     "gpt-4",
			available: false,
		},
		{
			name: "Errored connection is not available",
			setup: func(t *testing.T) *Connection {
				c := NewConnection("a3", "openai", "c", 1, "api_key")
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("setup MarkUsed: %v", err)
				}
				if err := c.MarkErrored(errors.New("fail")); err != nil {
					t.Fatalf("setup MarkErrored: %v", err)
				}
				return c
			},
			model:     "",
			available: false,
		},
		{
			name: "AuthExpired connection is not available",
			setup: func(t *testing.T) *Connection {
				c := NewConnection("a4", "openai", "c", 1, "api_key")
				if err := c.MarkUsed(); err != nil {
					t.Fatalf("setup MarkUsed: %v", err)
				}
				if err := c.MarkAuthExpired(); err != nil {
					t.Fatalf("setup MarkAuthExpired: %v", err)
				}
				return c
			},
			model:     "",
			available: false,
		},
		{
			name: "Disabled connection is not available",
			setup: func(t *testing.T) *Connection {
				c := NewConnection("a5", "openai", "c", 1, "api_key")
				if err := c.Disable(); err != nil {
					t.Fatalf("setup Disable: %v", err)
				}
				return c
			},
			model:     "",
			available: false,
		},
		{
			name: "Idle connection with blank model is available",
			setup: func(t *testing.T) *Connection {
				return NewConnection("a6", "openai", "c", 1, "api_key")
			},
			model:     "",
			available: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.setup(t)
			got := c.IsAvailable(tt.model)
			if got != tt.available {
				t.Errorf("IsAvailable(%q) = %v, want %v (state=%s)", tt.model, got, tt.available, c.State())
			}
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
