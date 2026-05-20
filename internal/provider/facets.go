package provider

import "errors"

// The three-facet connection-health model (ADR-1, cycle
// 20260520-routing-core-hardening M2). The single 8-value provider.State enum
// conflated three orthogonal concerns; this file introduces them as separate
// facets — Breaker (transient health), Auth (credential validity), and
// Lifecycle (request lifecycle) — each with its own small transition table.
// The old State enum and state.go were deleted in plan T14, once every caller
// had migrated to the facets.

// ErrTransitionRejected is the sentinel returned when a facet transition is
// not allowed. Callers use errors.Is to distinguish a benign transition race
// (e.g. next-connection fallthrough) from a hard failure that should bubble
// up to the user. Every facet transition method wraps it via rejectedTransition.
var ErrTransitionRejected = errors.New("state transition rejected")

// BreakerState is the transient-health facet — a circuit breaker. It is
// in-memory only: never persisted, a connection loads CLOSED on restart.
type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"    // healthy, selectable
	BreakerOpen     BreakerState = "open"      // a transient failure occurred; unavailable until cooldown
	BreakerHalfOpen BreakerState = "half_open" // cooldown expired; selectable, one trial at a time
)

// AuthState is the credential-validity facet — orthogonal to the breaker. A
// connection with a fresh credential that is also rate-limited is
// {Auth: AuthValid, Breaker: BreakerOpen} — a state the old enum could not
// express.
type AuthState string

const (
	AuthValid      AuthState = "valid"      // credential usable
	AuthExpired    AuthState = "expired"    // credential expired; needs refresh
	AuthRefreshing AuthState = "refreshing" // refresh in progress
)

// LifecycleState is the request-lifecycle facet.
type LifecycleState string

const (
	LifecycleIdle     LifecycleState = "idle"     // not in flight
	LifecycleActive   LifecycleState = "active"   // request in flight
	LifecycleDisabled LifecycleState = "disabled" // operator-disabled
)

// FailureKind classifies why a breaker opened. It drives the cooldown duration
// (cooldown.go: CooldownFor) and the 429-orthogonality rule
// (CountsTowardProviderHealth).
type FailureKind string

const (
	FailureRateLimit FailureKind = "rate_limit"      // upstream 429
	FailureQuota     FailureKind = "quota_exhausted" // subscription credits / quota window
	FailureTransient FailureKind = "transient"       // 5xx, timeout, transport error
	FailureErrored   FailureKind = "errored"         // other upstream failure
)

// CountsTowardProviderHealth reports whether a failure of this kind may
// contribute to a provider-level health aggregate. Rate-limit and
// quota-exhaustion are connection-scoped — counting them provider-wide would
// trip the breaker for every account on a provider when one account is merely
// throttled (the 429-orthogonality rule — ADR-1, OmniRoute Issue #1846). M2
// ships no aggregate consumer; this predicate exists so a future one is
// correct by construction.
func (k FailureKind) CountsTowardProviderHealth() bool {
	switch k {
	case FailureTransient, FailureErrored:
		return true
	default: // FailureRateLimit, FailureQuota
		return false
	}
}

// validBreakerTransitions encodes the breaker's legal moves. OPEN→HALF_OPEN is
// timer-driven (HealthChecker); HALF_OPEN→CLOSED/OPEN is the trial outcome.
var validBreakerTransitions = map[BreakerState]map[BreakerState]bool{
	BreakerClosed:   {BreakerOpen: true},
	BreakerOpen:     {BreakerHalfOpen: true},
	BreakerHalfOpen: {BreakerClosed: true, BreakerOpen: true},
}

// validAuthTransitions encodes the auth facet's legal moves. AuthExpired→
// AuthValid covers auto-detect connections recovering from fresh filesystem
// credentials without an explicit refresh round-trip.
var validAuthTransitions = map[AuthState]map[AuthState]bool{
	AuthValid:      {AuthExpired: true, AuthRefreshing: true},
	AuthExpired:    {AuthRefreshing: true, AuthValid: true},
	AuthRefreshing: {AuthValid: true, AuthExpired: true},
}

// validLifecycleTransitions encodes the lifecycle facet's legal moves.
var validLifecycleTransitions = map[LifecycleState]map[LifecycleState]bool{
	LifecycleIdle:     {LifecycleActive: true, LifecycleDisabled: true},
	LifecycleActive:   {LifecycleIdle: true, LifecycleDisabled: true},
	LifecycleDisabled: {LifecycleIdle: true},
}

// CanTransitionBreaker reports whether a breaker move is legal. Self-loops are
// not transitions and return false.
func CanTransitionBreaker(from, to BreakerState) bool {
	return validBreakerTransitions[from][to]
}

// CanTransitionAuth reports whether an auth-facet move is legal.
func CanTransitionAuth(from, to AuthState) bool {
	return validAuthTransitions[from][to]
}

// CanTransitionLifecycle reports whether a lifecycle-facet move is legal.
func CanTransitionLifecycle(from, to LifecycleState) bool {
	return validLifecycleTransitions[from][to]
}
