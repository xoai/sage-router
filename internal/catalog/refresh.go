package catalog

import (
	"context"
	"log/slog"
	"time"
)

// CredentialsLookup is the caller-side function the DiscoveryRunner
// uses to obtain a ListerCredentials for a given provider when it
// doesn't have a specific connection (e.g., on the 24h ticker).
//
// Returns (creds, true) when an active connection exists for the
// provider AND credentials could be assembled; (zero, false) when
// the provider should be skipped (e.g., no connections registered
// yet, or all are disabled). The runner does not infer "no creds"
// as an error — that condition is normal during operation.
//
// Wired by main.go (M2.5 production wiring) from the connection
// table + config.KnownProviders.
type CredentialsLookup func(provider string) (creds ListerCredentials, ok bool)

const (
	defaultRefreshInterval = 24 * time.Hour
	defaultSettleDelay     = 5 * time.Minute
)

// DiscoverAll runs DiscoverProvider for every provider with
// DiscoveryEnabled=true that's not currently in backoff. Used by the
// 24h ticker. Returns one DiscoveryResult per provider actually
// dispatched (skipped providers don't appear).
//
// Per-provider failures don't block the others — every result lands
// in the returned slice regardless. Callers may log / surface them.
func (d *DiscoveryRunner) DiscoverAll(ctx context.Context, credsFor CredentialsLookup) []DiscoveryResult {
	metas, err := d.Store.ListProviderMetas(ctx)
	if err != nil {
		slog.Warn("discovery: list provider metas failed", "err", err)
		return nil
	}

	now := d.Clock()
	var results []DiscoveryResult
	for _, m := range metas {
		if !d.ShouldRunForProvider(m, now) {
			continue
		}
		creds, ok := credsFor(m.Provider)
		if !ok {
			// No connection / no usable creds for this provider right
			// now. Not an error — operators may not have added a
			// connection for every provider in the seed.
			continue
		}
		results = append(results, d.DiscoverProvider(ctx, m.Provider, creds))
	}
	return results
}

// StartBackgroundRefresh spawns a goroutine that runs DiscoverAll on
// a 24h ticker. The first run is delayed by `defaultSettleDelay`
// (5 minutes) so the server has a chance to finish bootstrapping and
// register all its connections before the first discovery sweep
// hits the providers.
//
// Returns immediately. The goroutine exits when `ctx` is cancelled
// (shutdown), draining any in-flight tick.
//
// Production wiring (cmd/sage-router/main.go in M2.13's close):
//
//	catalog.StartBackgroundRefresh(serverCtx, w.Discovery, credsLookupFunc)
//
// In tests, drive the loop directly via runBackgroundRefreshLoop with
// an external tick channel for deterministic timing.
func StartBackgroundRefresh(ctx context.Context, runner *DiscoveryRunner, credsFor CredentialsLookup) {
	go func() {
		// Settle delay before the first tick.
		select {
		case <-time.After(defaultSettleDelay):
		case <-ctx.Done():
			return
		}

		ticker := time.NewTicker(defaultRefreshInterval)
		defer ticker.Stop()

		// Run the first cycle immediately after the settle delay.
		runner.DiscoverAll(ctx, credsFor)

		runBackgroundRefreshLoop(ctx, runner, credsFor, ticker.C)
	}()
}

// runBackgroundRefreshLoop is the per-tick body of the background
// refresh goroutine, separated so tests can drive it deterministically
// via an external chan. Each tick fires DiscoverAll. The loop exits
// on ctx.Done().
func runBackgroundRefreshLoop(
	ctx context.Context,
	runner *DiscoveryRunner,
	credsFor CredentialsLookup,
	ticks <-chan time.Time,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			runner.DiscoverAll(ctx, credsFor)
		}
	}
}
