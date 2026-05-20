package provider

import "testing"

// TestCanTransitionBreaker pins the breaker facet's legal moves: CLOSED→OPEN,
// OPEN→HALF_OPEN (timer-driven), HALF_OPEN→CLOSED (trial ok), HALF_OPEN→OPEN
// (trial failed). Everything else — including self-loops — is rejected.
func TestCanTransitionBreaker(t *testing.T) {
	legal := []struct{ from, to BreakerState }{
		{BreakerClosed, BreakerOpen},
		{BreakerOpen, BreakerHalfOpen},
		{BreakerHalfOpen, BreakerClosed},
		{BreakerHalfOpen, BreakerOpen},
	}
	for _, tc := range legal {
		if !CanTransitionBreaker(tc.from, tc.to) {
			t.Errorf("breaker %s→%s: want legal, got rejected", tc.from, tc.to)
		}
	}
	illegal := []struct{ from, to BreakerState }{
		{BreakerClosed, BreakerHalfOpen}, // must pass through OPEN
		{BreakerOpen, BreakerClosed},     // must pass through HALF_OPEN
		{BreakerClosed, BreakerClosed},   // self-loop
	}
	for _, tc := range illegal {
		if CanTransitionBreaker(tc.from, tc.to) {
			t.Errorf("breaker %s→%s: want rejected, got legal", tc.from, tc.to)
		}
	}
}

// TestCanTransitionAuth pins the auth facet. Auth moves independently of the
// breaker: a connection can be AuthValid while its breaker is OPEN.
func TestCanTransitionAuth(t *testing.T) {
	legal := []struct{ from, to AuthState }{
		{AuthValid, AuthExpired},
		{AuthValid, AuthRefreshing},   // preemptive refresh of an expiring credential
		{AuthExpired, AuthRefreshing}, // reactive refresh
		{AuthExpired, AuthValid},      // auto-detect recovery from fresh filesystem creds
		{AuthRefreshing, AuthValid},   // refresh succeeded
		{AuthRefreshing, AuthExpired}, // refresh failed
	}
	for _, tc := range legal {
		if !CanTransitionAuth(tc.from, tc.to) {
			t.Errorf("auth %s→%s: want legal, got rejected", tc.from, tc.to)
		}
	}
	illegal := []struct{ from, to AuthState }{
		{AuthValid, AuthValid},
		{AuthRefreshing, AuthRefreshing},
	}
	for _, tc := range illegal {
		if CanTransitionAuth(tc.from, tc.to) {
			t.Errorf("auth %s→%s: want rejected, got legal", tc.from, tc.to)
		}
	}
}

// TestCanTransitionLifecycle pins the lifecycle facet.
func TestCanTransitionLifecycle(t *testing.T) {
	legal := []struct{ from, to LifecycleState }{
		{LifecycleIdle, LifecycleActive},
		{LifecycleActive, LifecycleIdle},
		{LifecycleIdle, LifecycleDisabled},
		{LifecycleActive, LifecycleDisabled},
		{LifecycleDisabled, LifecycleIdle},
	}
	for _, tc := range legal {
		if !CanTransitionLifecycle(tc.from, tc.to) {
			t.Errorf("lifecycle %s→%s: want legal, got rejected", tc.from, tc.to)
		}
	}
	illegal := []struct{ from, to LifecycleState }{
		{LifecycleIdle, LifecycleIdle},
		{LifecycleDisabled, LifecycleActive}, // must re-enable to Idle first
	}
	for _, tc := range illegal {
		if CanTransitionLifecycle(tc.from, tc.to) {
			t.Errorf("lifecycle %s→%s: want rejected, got legal", tc.from, tc.to)
		}
	}
}

// TestFailureKind_CountsTowardProviderHealth pins the 429-orthogonality rule:
// rate_limit and quota_exhausted are connection-scoped and must NOT feed a
// provider-level health aggregate; transient and errored may.
func TestFailureKind_CountsTowardProviderHealth(t *testing.T) {
	want := map[FailureKind]bool{
		FailureRateLimit: false,
		FailureQuota:     false,
		FailureTransient: true,
		FailureErrored:   true,
	}
	for kind, expect := range want {
		if got := kind.CountsTowardProviderHealth(); got != expect {
			t.Errorf("%s.CountsTowardProviderHealth() = %v, want %v", kind, got, expect)
		}
	}
}
