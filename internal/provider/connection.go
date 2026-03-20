package provider

import (
	"fmt"
	"sync"
	"time"
)

// Connection represents a single configured connection to an upstream provider.
// All exported methods are safe for concurrent use.
type Connection struct {
	ID       string
	Provider string
	Name     string
	Priority int
	AuthType string

	mu              sync.RWMutex
	state           State
	lastUsedAt      time.Time
	consecutiveUses int
	modelLocks      map[string]time.Time // model → rate-limit expiry
	cooldownUntil   time.Time
	backoffLevel    int
	lastError       error
}

// NewConnection creates a connection in the Idle state.
func NewConnection(id, provider, name string, priority int, authType string) *Connection {
	return &Connection{
		ID:         id,
		Provider:   provider,
		Name:       name,
		Priority:   priority,
		AuthType:   authType,
		state:      StateIdle,
		modelLocks: make(map[string]time.Time),
	}
}

// State returns the current state (thread-safe).
func (c *Connection) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// LastUsedAt returns when the connection was last used.
func (c *Connection) LastUsedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastUsedAt
}

// ConsecutiveUses returns the number of consecutive uses without rotation.
func (c *Connection) ConsecutiveUses() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.consecutiveUses
}

// BackoffLevel returns the current backoff level.
func (c *Connection) BackoffLevel() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.backoffLevel
}

// CooldownUntil returns the time at which the cooldown expires.
func (c *Connection) CooldownUntil() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cooldownUntil
}

// LastError returns the most recent error that caused a state transition.
func (c *Connection) LastError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastError
}

// transition moves the connection to a new state if the transition is valid.
// Caller must hold c.mu (write lock).
func (c *Connection) transition(to State) error {
	if !CanTransition(c.state, to) {
		return &ErrInvalidTransition{From: c.state, To: to}
	}
	c.state = to
	return nil
}

// IsAvailable reports whether this connection can serve a request for the
// given model right now. A blank model matches any.
func (c *Connection) IsAvailable(model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isAvailableLocked(model)
}

// isAvailableLocked is the lock-free inner check. Caller must hold at least RLock.
func (c *Connection) isAvailableLocked(model string) bool {
	now := time.Now()

	switch c.state {
	case StateIdle:
		// Idle is always available, but check model lock.
	case StateCooldown:
		// Available only if the cooldown has expired.
		if now.Before(c.cooldownUntil) {
			return false
		}
	default:
		// Active, RateLimited, AuthExpired, Refreshing, Errored, Disabled — not available.
		return false
	}

	// Check model-scoped rate-limit lock.
	if model != "" {
		if expiry, locked := c.modelLocks[model]; locked && now.Before(expiry) {
			return false
		}
	}

	return true
}

// MarkUsed transitions from Idle to Active and updates usage counters.
func (c *Connection) MarkUsed() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transition(StateActive); err != nil {
		return fmt.Errorf("MarkUsed: %w", err)
	}
	c.lastUsedAt = time.Now()
	c.consecutiveUses++
	return nil
}

// MarkRateLimited transitions Active → RateLimited → Cooldown, sets a
// model-scoped lock, and applies exponential backoff.
func (c *Connection) MarkRateLimited(model string, backoffLevel int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Active → RateLimited
	if err := c.transition(StateRateLimited); err != nil {
		return fmt.Errorf("MarkRateLimited: %w", err)
	}

	// Clamp backoff.
	if backoffLevel < 0 {
		backoffLevel = 0
	}
	if backoffLevel > MaxBackoffLevel {
		backoffLevel = MaxBackoffLevel
	}
	c.backoffLevel = backoffLevel

	cooldown := CalculateCooldown(c.backoffLevel)
	c.cooldownUntil = time.Now().Add(cooldown)

	// Set model-scoped lock if a model was specified.
	if model != "" {
		c.modelLocks[model] = c.cooldownUntil
	}

	// RateLimited → Cooldown (immediate)
	if err := c.transition(StateCooldown); err != nil {
		return fmt.Errorf("MarkRateLimited (cooldown): %w", err)
	}

	return nil
}

// MarkAuthExpired transitions from Active to AuthExpired.
func (c *Connection) MarkAuthExpired() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transition(StateAuthExpired); err != nil {
		return fmt.Errorf("MarkAuthExpired: %w", err)
	}
	return nil
}

// MarkRefreshing transitions from AuthExpired to Refreshing.
func (c *Connection) MarkRefreshing() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transition(StateRefreshing); err != nil {
		return fmt.Errorf("MarkRefreshing: %w", err)
	}
	return nil
}

// MarkRefreshSuccess transitions from Refreshing to Active.
func (c *Connection) MarkRefreshSuccess() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transition(StateActive); err != nil {
		return fmt.Errorf("MarkRefreshSuccess: %w", err)
	}
	c.backoffLevel = 0
	c.lastError = nil
	return nil
}

// MarkRefreshFailure transitions from Refreshing to Errored.
func (c *Connection) MarkRefreshFailure(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastError = err
	if terr := c.transition(StateErrored); terr != nil {
		return fmt.Errorf("MarkRefreshFailure: %w", terr)
	}
	return nil
}

// MarkErrored transitions from Active to Errored.
func (c *Connection) MarkErrored(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastError = err
	if terr := c.transition(StateErrored); terr != nil {
		return fmt.Errorf("MarkErrored: %w", terr)
	}
	return nil
}

// MarkSuccess transitions from Active back to Idle and resets backoff.
func (c *Connection) MarkSuccess() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transition(StateIdle); err != nil {
		return fmt.Errorf("MarkSuccess: %w", err)
	}
	c.backoffLevel = 0
	c.lastError = nil
	c.consecutiveUses = 0

	// Garbage-collect expired model locks.
	now := time.Now()
	for m, exp := range c.modelLocks {
		if now.After(exp) {
			delete(c.modelLocks, m)
		}
	}

	return nil
}

// ResetCooldown transitions from Cooldown (or Errored) back to Idle, clearing
// backoff and cooldown timers. Used for manual retry.
func (c *Connection) ResetCooldown() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transition(StateIdle); err != nil {
		return fmt.Errorf("ResetCooldown: %w", err)
	}
	c.backoffLevel = 0
	c.cooldownUntil = time.Time{}
	c.lastError = nil
	return nil
}

// Disable transitions any state to Disabled.
func (c *Connection) Disable() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transition(StateDisabled); err != nil {
		return fmt.Errorf("Disable: %w", err)
	}
	return nil
}

// Enable transitions from Disabled back to Idle.
func (c *Connection) Enable() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transition(StateIdle); err != nil {
		return fmt.Errorf("Enable: %w", err)
	}
	c.backoffLevel = 0
	c.cooldownUntil = time.Time{}
	c.lastError = nil
	return nil
}
