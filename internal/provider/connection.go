package provider

import (
	"fmt"
	"sync"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/providers"
)

// modelDenylistTTL is how long a connection refuses a model after the
// upstream returned a model-level rejection (distinct from a rate limit).
const modelDenylistTTL = 1 * time.Hour

// Connection represents a single configured connection to an upstream provider.
// All exported methods are safe for concurrent use.
//
// Lock-order rule (auto-review M6): c.mu is the only mutex on Connection.
// Methods that acquire c.mu MUST NOT call into stores or refresh code under
// the lock — release c.mu before any I/O and reacquire afterward to publish
// results. A custom go vet analyzer (Task 1.9) enforces this.
type Connection struct {
	ID       string
	Provider string
	Name     string
	Priority int
	AuthType string

	mu    sync.RWMutex
	state State

	// M2 three-facet connection-health model (cycle 20260520-m2-circuit-breaker,
	// ADR-1). These land alongside the old `state` field during the strangler
	// migration; `state` and the old State enum are removed in plan T6/T14.
	breaker          BreakerState
	auth             AuthState
	lifecycle        LifecycleState
	failureKind      FailureKind
	halfOpenInFlight bool

	lastUsedAt      time.Time
	consecutiveUses int
	modelLocks      map[string]time.Time // model → rate-limit expiry
	cooldownUntil   time.Time
	backoffLevel    int
	lastError       error

	// Subscription-auth additions, ALL guarded by c.mu.
	cred          *auth.Credential     // decrypted token cache; nil until loaded
	modelDenylist map[string]time.Time // model → blocked-until after a model-rejection 403
}

// NewConnection creates a connection in the Idle state.
func NewConnection(id, provider, name string, priority int, authType string) *Connection {
	return &Connection{
		ID:            id,
		Provider:      provider,
		Name:          name,
		Priority:      priority,
		AuthType:      authType,
		state:         StateIdle,
		breaker:       BreakerClosed,
		auth:          AuthValid,
		lifecycle:     LifecycleIdle,
		modelLocks:    make(map[string]time.Time),
		modelDenylist: make(map[string]time.Time),
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

// SetLastError records an error message on the connection WITHOUT changing
// state. Used by paths that want to surface an actionable message to the
// dashboard alongside (and BEFORE) a state-transitioning Mark* call —
// e.g., tier-error 401 detection in routes_v1.go::markConnectionResult
// where the friendly text "ChatGPT subscription doesn't include API access"
// is set before MarkAuthExpired transitions the state. Mark* methods that
// already set lastError will overwrite this, which is the right semantics
// (later transitions know more than earlier ones).
func (c *Connection) SetLastError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastError = err
}

// transitionLocked moves the connection to a new state if the transition is valid.
// CALLER MUST HOLD c.mu (write lock). The trailing "Locked" suffix is the lock-
// discipline convention used elsewhere in the codebase. Errors are
// errors.Is(ErrTransitionRejected) so callers can distinguish a benign
// state-machine race from a hard failure.
func (c *Connection) transitionLocked(to State) error {
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

	if err := c.transitionLocked(StateActive); err != nil {
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
	if err := c.transitionLocked(StateRateLimited); err != nil {
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
	if err := c.transitionLocked(StateCooldown); err != nil {
		return fmt.Errorf("MarkRateLimited (cooldown): %w", err)
	}

	return nil
}

// MarkAuthExpired transitions from Active to AuthExpired.
func (c *Connection) MarkAuthExpired() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transitionLocked(StateAuthExpired); err != nil {
		return fmt.Errorf("MarkAuthExpired: %w", err)
	}
	return nil
}

// MarkRefreshing transitions from AuthExpired to Refreshing.
func (c *Connection) MarkRefreshing() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transitionLocked(StateRefreshing); err != nil {
		return fmt.Errorf("MarkRefreshing: %w", err)
	}
	return nil
}

// MarkRefreshSuccess transitions from Refreshing to Active.
func (c *Connection) MarkRefreshSuccess() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transitionLocked(StateActive); err != nil {
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
	if terr := c.transitionLocked(StateErrored); terr != nil {
		return fmt.Errorf("MarkRefreshFailure: %w", terr)
	}
	return nil
}

// MarkErrored transitions from Active to Errored.
func (c *Connection) MarkErrored(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastError = err
	if terr := c.transitionLocked(StateErrored); terr != nil {
		return fmt.Errorf("MarkErrored: %w", terr)
	}
	return nil
}

// MarkSuccess transitions from Active back to Idle and resets backoff.
func (c *Connection) MarkSuccess() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transitionLocked(StateIdle); err != nil {
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

	if err := c.transitionLocked(StateIdle); err != nil {
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

	if err := c.transitionLocked(StateDisabled); err != nil {
		return fmt.Errorf("Disable: %w", err)
	}
	return nil
}

// Enable transitions from Disabled back to Idle.
func (c *Connection) Enable() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.transitionLocked(StateIdle); err != nil {
		return fmt.Errorf("Enable: %w", err)
	}
	c.backoffLevel = 0
	c.cooldownUntil = time.Time{}
	c.lastError = nil
	return nil
}

// ── Subscription-auth additions ──

// cachedCredential returns the in-memory cached credential under RLock,
// or nil if none is cached. AcquireCredential (M2) uses this on its fast
// path before considering a refresh.
func (c *Connection) cachedCredential() *auth.Credential {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cred
}

// HasCredential reports whether a credential is currently cached. Useful for
// the dashboard ("subscription connected") indicator and for tests outside
// this package that need to assert post-Invalidate state without touching
// the raw token.
func (c *Connection) HasCredential() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cred != nil
}

// SetCredentialForTest seeds the in-memory cache. Test-only. Production code
// populates this via AcquireCredential (M2). The "ForTest" suffix is
// deliberately ugly so reviewers notice if it leaks into a non-test path.
func (c *Connection) SetCredentialForTest(cred *auth.Credential) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cred = cred
}

// InvalidateCredential drops the in-memory token cache. Called on upstream
// 401/403 so the next request triggers a fresh load + refresh.
func (c *Connection) InvalidateCredential() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cred = nil
}

// CanServeModel reports whether this connection can serve the given model
// right now. Two layered checks:
//
//  1. Per-connection runtime denylist (1h TTL after a model-level 403).
//     This is the "self-healing" half — the connection learns to avoid
//     models the provider has dropped from this subscription tier.
//
//  2. Static subscription allowlist from the provider registry, but ONLY
//     when AuthType is "subscription". API-key connections aren't
//     constrained by a subscription tier so the allowlist doesn't apply.
//
// An empty model passes through unconditionally — selection paths that
// don't know the model yet shouldn't be punished.
func (c *Connection) CanServeModel(model string) bool {
	if model == "" {
		return true
	}
	c.mu.RLock()
	until, denied := c.modelDenylist[model]
	c.mu.RUnlock()
	if denied && time.Now().Before(until) {
		return false
	}
	if c.AuthType == auth.AuthTypeSubscription {
		if !providers.SubscriptionAllowed(c.Provider, model) {
			return false
		}
	}
	return true
}

// RecordModelRejection adds model to the per-connection denylist for the
// standard TTL (1h). Called when the upstream returns a model-level 403.
// Distinct from the rate-limit-driven modelLocks.
func (c *Connection) RecordModelRejection(model string) {
	c.addModelDenylistFor(model, modelDenylistTTL)
}

// addModelDenylistFor is the inner helper that lets tests inject a custom TTL.
func (c *Connection) addModelDenylistFor(model string, ttl time.Duration) {
	if model == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modelDenylist[model] = time.Now().Add(ttl)
}

// ModelDenylistSnapshot returns an independent copy of the active per-model
// denylist entries — those whose expiry has not yet passed. Mutating the
// returned map does NOT affect the connection. Used by the dashboard's
// /api/connections projection to surface in-memory filter state that would
// otherwise be invisible (the connection appears "green" in DB state while
// silently refusing specific models). Cycle 20260516-connection-runtime-state.
//
// Filter semantics match CanServeModel at the call site: an entry is "active"
// when now.Before(expiry). Returns an empty (non-nil) map when no entries
// are active, for consistent JSON wire shape.
func (c *Connection) ModelDenylistSnapshot() map[string]time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now()
	out := make(map[string]time.Time, len(c.modelDenylist))
	for model, expiry := range c.modelDenylist {
		if now.Before(expiry) {
			out[model] = expiry
		}
	}
	return out
}

// ModelLocksSnapshot returns an independent copy of the active per-model
// rate-limit locks — those whose expiry has not yet passed. Mutating the
// returned map does NOT affect the connection.
//
// Filter semantics match isAvailableLocked at the call site: a lock is
// "active" when now.Before(expiry). Returns an empty (non-nil) map when no
// locks are active. Cycle 20260516-connection-runtime-state.
func (c *Connection) ModelLocksSnapshot() map[string]time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now()
	out := make(map[string]time.Time, len(c.modelLocks))
	for model, expiry := range c.modelLocks {
		if now.Before(expiry) {
			out[model] = expiry
		}
	}
	return out
}

// ── M2 three-facet model: breaker / auth / lifecycle methods ──
//
// New code (cycle 20260520-m2-circuit-breaker, plan T5) alongside the old
// State enum. T6 rewrites the old State()/Mark*/IsAvailable surface as shims
// over these facets; T14 deletes the old enum.

// Breaker returns the transient-health facet (thread-safe).
func (c *Connection) Breaker() BreakerState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.breaker
}

// Auth returns the credential-validity facet (thread-safe).
func (c *Connection) Auth() AuthState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.auth
}

// Lifecycle returns the request-lifecycle facet (thread-safe).
func (c *Connection) Lifecycle() LifecycleState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lifecycle
}

// FailureKind returns the kind of the failure that last opened the breaker.
func (c *Connection) FailureKind() FailureKind {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.failureKind
}

// Selectable reports whether the Selector may hand this connection a request
// for the given model. The rule (ADR-1 §Selectability): the breaker is CLOSED
// or HALF_OPEN, the credential is valid, the connection is not operator-
// disabled, and — for a non-empty model — there is no live model-scoped
// rate-limit lock, no live post-403 denylist entry, and the subscription tier
// permits the model. A blank model skips the per-model checks.
func (c *Connection) Selectable(model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.selectableLocked(model)
}

// selectableLocked is the lock-free selectability check. The caller must hold
// c.mu (read or write).
func (c *Connection) selectableLocked(model string) bool {
	if c.breaker != BreakerClosed && c.breaker != BreakerHalfOpen {
		return false
	}
	if c.auth != AuthValid {
		return false
	}
	if c.lifecycle == LifecycleDisabled {
		return false
	}
	if model == "" {
		return true
	}
	now := time.Now()
	if exp, locked := c.modelLocks[model]; locked && now.Before(exp) {
		return false
	}
	if until, denied := c.modelDenylist[model]; denied && now.Before(until) {
		return false
	}
	// Subscription-tier allowlist. AuthType is immutable and SubscriptionAllowed
	// is a pure registry lookup, so this is safe under the held lock.
	if c.AuthType == auth.AuthTypeSubscription && !providers.SubscriptionAllowed(c.Provider, model) {
		return false
	}
	return true
}

// TryClaimHalfOpenTrial reports whether this connection is selectable for the
// model and, when its breaker is HALF_OPEN, atomically claims the single trial
// slot (compare-and-set halfOpenInFlight false→true). A CLOSED connection is
// selectable without consuming a slot. A HALF_OPEN connection is selectable
// only if the claim wins — the losing caller falls through to the next
// candidate. The claimer MUST pair this with ReleaseHalfOpenTrial on every
// terminal path of the request (spec §4).
func (c *Connection) TryClaimHalfOpenTrial(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.selectableLocked(model) {
		return false
	}
	if c.breaker == BreakerHalfOpen {
		if c.halfOpenInFlight {
			return false // the single trial slot is already taken
		}
		c.halfOpenInFlight = true
	}
	return true
}

// ReleaseHalfOpenTrial frees the HALF_OPEN single-trial slot. It is idempotent
// — releasing an unclaimed slot is a no-op — so a caller may defer it
// unconditionally after any Select, covering every terminal path (success,
// trial failure, executor error, context cancel, panic unwind).
func (c *Connection) ReleaseHalfOpenTrial() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.halfOpenInFlight = false
}

// OpenBreaker opens the circuit breaker after a failed request: it records the
// failure kind, escalates the backoff level, sets cooldownUntil from
// CooldownFor (honoring retryAfter for rate-limit/quota), sets a model-scoped
// lock when model is non-empty, and returns an Active connection to Idle. A
// failed HALF_OPEN trial is OpenBreaker observing a HALF_OPEN breaker.
// Re-opening an already-OPEN breaker is rejected (errors.Is ErrTransitionRejected).
func (c *Connection) OpenBreaker(kind FailureKind, retryAfter time.Duration, model string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !CanTransitionBreaker(c.breaker, BreakerOpen) {
		return rejectedTransition("OpenBreaker", string(c.breaker), string(BreakerOpen))
	}
	c.breaker = BreakerOpen
	c.failureKind = kind
	if c.backoffLevel < MaxBackoffLevel {
		c.backoffLevel++
	}
	c.cooldownUntil = time.Now().Add(CooldownFor(kind, c.backoffLevel, retryAfter))
	if model != "" {
		c.modelLocks[model] = c.cooldownUntil
	}
	if c.lifecycle == LifecycleActive {
		c.lifecycle = LifecycleIdle
	}
	c.halfOpenInFlight = false
	return nil
}

// ToHalfOpen moves an OPEN breaker to HALF_OPEN — the timer-driven recovery
// step the HealthChecker performs once cooldownUntil has elapsed. It resets
// the trial slot so the next Selector pick can claim it.
func (c *Connection) ToHalfOpen() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !CanTransitionBreaker(c.breaker, BreakerHalfOpen) {
		return rejectedTransition("ToHalfOpen", string(c.breaker), string(BreakerHalfOpen))
	}
	c.breaker = BreakerHalfOpen
	c.halfOpenInFlight = false
	return nil
}

// SetFacetsForTest seeds the three facets directly, bypassing the transition
// rules. Test-only — production code transitions facets via OpenBreaker /
// ToHalfOpen / the Mark* methods. The "ForTest" suffix is deliberately ugly so
// reviewers notice if it leaks into a non-test path (cf. SetCredentialForTest).
func (c *Connection) SetFacetsForTest(breaker BreakerState, authState AuthState, lifecycle LifecycleState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.breaker = breaker
	c.auth = authState
	c.lifecycle = lifecycle
}

// rejectedTransition builds an error for an illegal facet transition. It wraps
// ErrTransitionRejected so callers can errors.Is it — the same contract the
// old State-enum transitions use.
func rejectedTransition(op, from, to string) error {
	return fmt.Errorf("%s: transition %s→%s rejected: %w", op, from, to, ErrTransitionRejected)
}
