package refresh

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/provider"
)

// Default operational constants. Tests can override.
const (
	defaultLoopInterval = 30 * time.Second
	defaultRateLimit    = 200 * time.Millisecond // 5 refreshes/sec, generous to providers
	disableThreshold    = 3                      // consecutive refresh failures before auto-disable
	preemptiveBuffer    = 5 * time.Minute        // refresh when token expires within this window
)

// CredentialStore is the narrow view of auth.AuthStore the loop needs.
// Defined here (rather than depending on the concrete *auth.AuthStore)
// so tests can supply a fake without spinning up a real DB.
type CredentialStore interface {
	GetCredential(connID string) (*auth.Credential, error)
	PutCredential(connID string, cred *auth.Credential) error
	BumpRefreshFailure(connID string) (int, error)
	ResetRefreshFailures(connID string) error
}

// RefreshFunc lets tests stub the refresh dispatcher without invoking
// real HTTP. Production wiring passes refresh.Refresh.
type RefreshFunc func(ctx context.Context, cred *auth.Credential) (*auth.Credential, error)

// ConnSnapshot returns the current set of connections to consider. The
// loop calls it once per tick to get a fresh view (the underlying
// selector is mutated by other goroutines).
type ConnSnapshot func() []*provider.Connection

// Loop is the background sweeper that proactively refreshes
// subscription-auth tokens before they expire, and drives refresh
// attempts for connections sitting in the AuthExpired state.
//
// It is NOT the request-path refresh — that lives in (the future)
// Connection.AcquireCredential. The Loop is the safety net that
// catches idle connections so they're hot when a request arrives.
type Loop struct {
	store        CredentialStore
	refresher    RefreshFunc
	snapshot     ConnSnapshot
	interval     time.Duration
	rateLimit    time.Duration
	disableAfter int
}

// NewLoop wires the loop with production defaults: 30s interval,
// 200ms rate limit between refreshes, disable-after-3-failures
// threshold. Override these fields directly for tests.
func NewLoop(store CredentialStore, refresher RefreshFunc, snapshot ConnSnapshot) *Loop {
	return &Loop{
		store:        store,
		refresher:    refresher,
		snapshot:     snapshot,
		interval:     defaultLoopInterval,
		rateLimit:    defaultRateLimit,
		disableAfter: disableThreshold,
	}
}

// Run blocks until ctx is cancelled, ticking once per interval. Each tick
// snapshots the current set of subscription connections and refreshes the
// ones that are AuthExpired or nearing expiry.
//
// Errors from individual refreshes are logged but do NOT stop the loop —
// one bad connection shouldn't poison the others.
func (l *Loop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	// Tick once immediately so a connection that came back as AuthExpired
	// right before sage-router started gets attention before the first
	// 30-second interval elapses.
	l.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

// tick is one sweep over the current connection set.
func (l *Loop) tick(ctx context.Context) {
	conns := l.snapshot()
	for _, c := range conns {
		if ctx.Err() != nil {
			return
		}
		if c.AuthType != auth.AuthTypeSubscription {
			continue
		}
		l.refreshOne(ctx, c)
		// Rate-limit between connections, but don't block on the rate
		// limit if the context is cancelling.
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.rateLimit):
		}
	}
}

// refreshOne handles a single connection. The decision tree:
//
//   - In AuthExpired → drive the full state machine
//     (AuthExpired → Refreshing → {AuthValid|AuthExpired}).
//   - In Idle/Active with cred expiring within the buffer → refresh
//     transparently, no state-machine transition. (Skipping for
//     non-Idle/Active states avoids racing with in-flight requests.)
//   - Anything else → skip.
//
// On ErrRefreshTokenRevoked: bump the persistent counter; at threshold,
// transition to Disabled so the selector ignores the connection until
// the user re-authenticates.
// On transient errors: bump the counter (so flapping providers eventually
// hit the threshold) but don't disable on a single bad attempt.
func (l *Loop) refreshOne(ctx context.Context, c *provider.Connection) {
	cred, err := l.store.GetCredential(c.ID)
	if err != nil {
		slog.Warn("refresh loop: GetCredential failed",
			"conn_id", c.ID, "err", err)
		return
	}
	if cred == nil {
		// No subscription credential to refresh (e.g., an apikey row that
		// somehow has auth_type=subscription — would be a misconfig).
		return
	}
	cred.Provider = c.Provider
	cred.ConnectionID = c.ID

	authState := c.Auth()
	inAuthExpired := authState == provider.AuthExpired

	if !inAuthExpired {
		// Only proactively refresh a healthy Idle/Active connection whose
		// credential is expiring soon. Skip a connection mid-refresh
		// (Auth=Refreshing), one with a tripped breaker (a transient
		// cooldown — not an auth problem), and an operator-Disabled one —
		// none of those want a transparent token swap. (AuthExpired is
		// handled by the branch below.)
		if authState != provider.AuthValid ||
			c.Breaker() != provider.BreakerClosed ||
			c.Lifecycle() == provider.LifecycleDisabled {
			return
		}
		if !cred.ExpiresWithin(preemptiveBuffer) {
			return
		}
	}

	// Drive the state machine for the AuthExpired case. If another
	// goroutine already transitioned us (e.g., a concurrent in-process
	// refresh from the request path that we're not wiring yet), MarkRefreshing
	// returns ErrTransitionRejected — skip this connection on this tick.
	if inAuthExpired {
		if err := c.MarkRefreshing(); err != nil {
			if errors.Is(err, provider.ErrTransitionRejected) {
				return
			}
			slog.Warn("refresh loop: MarkRefreshing failed",
				"conn_id", c.ID, "err", err)
			return
		}
	}

	fresh, err := l.refresher(ctx, cred)
	if err != nil {
		n, bumpErr := l.store.BumpRefreshFailure(c.ID)
		if bumpErr != nil {
			slog.Warn("refresh loop: BumpRefreshFailure failed",
				"conn_id", c.ID, "err", bumpErr)
		}

		if inAuthExpired {
			// Drive Refreshing → AuthExpired (the facet model — NOT Errored;
			// staying AuthExpired is what lets the next tick re-drive this
			// connection). Failure to mark is logged but we still attempt the
			// disable path.
			if merr := c.MarkRefreshFailure(err); merr != nil {
				slog.Warn("refresh loop: MarkRefreshFailure rejected",
					"conn_id", c.ID, "err", merr)
			}
		}

		revoked := errors.Is(err, ErrRefreshTokenRevoked)
		shouldDisable := revoked || (bumpErr == nil && n >= l.disableAfter)
		if shouldDisable {
			if derr := c.Disable(); derr != nil {
				if !errors.Is(derr, provider.ErrTransitionRejected) {
					slog.Warn("refresh loop: Disable rejected",
						"conn_id", c.ID, "err", derr)
				}
			} else {
				slog.Warn("subscription connection disabled — re-authenticate required",
					"conn_id", c.ID,
					"provider", c.Provider,
					"refresh_failures", n,
					"revoked", revoked,
				)
			}
		} else {
			slog.Info("refresh loop: transient failure",
				"conn_id", c.ID, "provider", c.Provider,
				"refresh_failures", n,
				"err", err,
			)
		}
		return
	}

	// Success path.
	fresh.ConnectionID = c.ID
	if perr := l.store.PutCredential(c.ID, fresh); perr != nil {
		// The in-memory token is valid (we just got it) but the DB
		// write failed. We CAN still use the new token in-process,
		// but the next process restart will pay one extra refresh
		// against the stale DB row. Log and continue.
		slog.Warn("refresh loop: PutCredential failed; cache leads DB",
			"conn_id", c.ID, "err", perr)
	}
	if rerr := l.store.ResetRefreshFailures(c.ID); rerr != nil {
		slog.Warn("refresh loop: ResetRefreshFailures failed",
			"conn_id", c.ID, "err", rerr)
	}

	// Drop the in-memory cache so the next request loads the fresh
	// credential from the (just-updated) DB.
	c.InvalidateCredential()

	if inAuthExpired {
		if err := c.MarkRefreshSuccess(); err != nil {
			slog.Warn("refresh loop: MarkRefreshSuccess rejected",
				"conn_id", c.ID, "err", err)
		}
	}
}
