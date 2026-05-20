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
// results. The lockorder_test.go AST-walking unit test enforces this.
type Connection struct {
	ID       string
	Provider string
	Name     string
	Priority int
	AuthType string

	mu sync.RWMutex

	// Three-facet connection-health model (cycle 20260520-m2-circuit-breaker,
	// ADR-1) — the single source of truth. Breaker is transient health, Auth
	// is credential validity, Lifecycle is the request lifecycle. They replaced
	// the old 8-value State enum, which conflated the three concerns.
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
		breaker:       BreakerClosed,
		auth:          AuthValid,
		lifecycle:     LifecycleIdle,
		modelLocks:    make(map[string]time.Time),
		modelDenylist: make(map[string]time.Time),
	}
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

// MarkUsed transitions the lifecycle facet Idle→Active and updates the usage
// counters. It rejects (errors.Is ErrTransitionRejected) if the connection is
// not Idle — the same race-guard the old enum gave callers.
func (c *Connection) MarkUsed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !CanTransitionLifecycle(c.lifecycle, LifecycleActive) {
		return rejectedTransition("MarkUsed", string(c.lifecycle), string(LifecycleActive))
	}
	c.lifecycle = LifecycleActive
	c.lastUsedAt = time.Now()
	c.consecutiveUses++
	return nil
}

// MarkAuthExpired moves the auth facet to AuthExpired (credential invalid) and
// returns an Active connection to Idle. Auth is orthogonal to the breaker — a
// connection can be AuthExpired with a healthy (CLOSED) breaker.
func (c *Connection) MarkAuthExpired() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !CanTransitionAuth(c.auth, AuthExpired) {
		return rejectedTransition("MarkAuthExpired", string(c.auth), string(AuthExpired))
	}
	c.auth = AuthExpired
	if c.lifecycle == LifecycleActive {
		c.lifecycle = LifecycleIdle
	}
	return nil
}

// MarkRefreshing moves the auth facet AuthExpired→AuthRefreshing. It rejects
// unless the credential is currently AuthExpired (the old precondition that
// connection_subscription_test.go pins).
func (c *Connection) MarkRefreshing() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.auth != AuthExpired {
		return rejectedTransition("MarkRefreshing", string(c.auth), string(AuthRefreshing))
	}
	c.auth = AuthRefreshing
	return nil
}

// MarkRefreshSuccess moves the auth facet to AuthValid after a successful
// credential refresh and resets backoff.
func (c *Connection) MarkRefreshSuccess() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !CanTransitionAuth(c.auth, AuthValid) {
		return rejectedTransition("MarkRefreshSuccess", string(c.auth), string(AuthValid))
	}
	c.auth = AuthValid
	c.backoffLevel = 0
	c.lastError = nil
	return nil
}

// MarkRefreshFailure records a failed credential refresh: the auth facet
// returns to AuthExpired — a credential problem, not a transient breaker fault
// (spec §1.2) — and the refresh loop re-drives it on a later tick.
func (c *Connection) MarkRefreshFailure(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastError = err
	if !CanTransitionAuth(c.auth, AuthExpired) {
		return rejectedTransition("MarkRefreshFailure", string(c.auth), string(AuthExpired))
	}
	c.auth = AuthExpired
	return nil
}

// MarkSuccess returns an Active connection to Idle after a successful request:
// it closes a HALF_OPEN breaker (the trial succeeded), resets backoff, and
// garbage-collects expired model locks. It rejects if the connection is not
// Active.
func (c *Connection) MarkSuccess() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !CanTransitionLifecycle(c.lifecycle, LifecycleIdle) {
		return rejectedTransition("MarkSuccess", string(c.lifecycle), string(LifecycleIdle))
	}
	c.lifecycle = LifecycleIdle
	if c.breaker == BreakerHalfOpen {
		c.breaker = BreakerClosed
		c.failureKind = ""
	}
	c.halfOpenInFlight = false
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

// ResetCooldown forces the breaker closed and clears the cooldown timers — the
// operator's manual-retry path. It always succeeds.
func (c *Connection) ResetCooldown() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.breaker = BreakerClosed
	c.failureKind = ""
	c.cooldownUntil = time.Time{}
	c.backoffLevel = 0
	c.lastError = nil
	c.halfOpenInFlight = false
	return nil
}

// Disable moves the lifecycle facet to Disabled (an operator action).
func (c *Connection) Disable() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !CanTransitionLifecycle(c.lifecycle, LifecycleDisabled) {
		return rejectedTransition("Disable", string(c.lifecycle), string(LifecycleDisabled))
	}
	c.lifecycle = LifecycleDisabled
	return nil
}

// Enable re-enables a Disabled connection: the lifecycle returns to Idle and
// the breaker is reset to a clean CLOSED state (the operator override clears
// transient health). The auth facet is left as-is — a credential problem is
// real, not operator-controlled.
func (c *Connection) Enable() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !CanTransitionLifecycle(c.lifecycle, LifecycleIdle) {
		return rejectedTransition("Enable", string(c.lifecycle), string(LifecycleIdle))
	}
	c.lifecycle = LifecycleIdle
	c.breaker = BreakerClosed
	c.failureKind = ""
	c.cooldownUntil = time.Time{}
	c.backoffLevel = 0
	c.lastError = nil
	c.halfOpenInFlight = false
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
// Filter semantics match selectableLocked at the call site: a lock is
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
// The connection-health surface (cycle 20260520-m2-circuit-breaker). The old
// State enum and its State()/MarkRateLimited/MarkErrored/IsAvailable surface
// were removed in plan T14, once every caller had migrated to these facets.

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
// for the given model. The rule: the breaker is CLOSED or HALF_OPEN, the
// credential is valid, the connection is idle (not in-flight and not
// operator-disabled), and — for a non-empty model — there is no live
// model-scoped rate-limit lock, no live post-403 denylist entry, and the
// subscription tier permits the model. A blank model skips the per-model
// checks. (ADR-1 §Selectability writes the lifecycle clause as "!= Disabled",
// but the precise rule is "== Idle" — a connection is at most one in-flight
// request, so an Active connection is not selectable.)
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
	if c.lifecycle != LifecycleIdle {
		// Not selectable while a request is in flight (Active) or while the
		// connection is operator-disabled.
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

// HalfOpenInFlightForTest reports whether the HALF_OPEN single-trial slot is
// currently claimed. Test-only — production code never inspects the slot
// directly; it is managed by TryClaimHalfOpenTrial / ReleaseHalfOpenTrial. The
// "ForTest" suffix is deliberately ugly so reviewers notice if it leaks into a
// non-test path (cf. SetFacetsForTest).
func (c *Connection) HalfOpenInFlightForTest() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.halfOpenInFlight
}

// rejectedTransition builds an error for an illegal facet transition. It wraps
// ErrTransitionRejected so callers can errors.Is it — the same contract the
// old State-enum transitions use.
func rejectedTransition(op, from, to string) error {
	return fmt.Errorf("%s: transition %s→%s rejected: %w", op, from, to, ErrTransitionRejected)
}
