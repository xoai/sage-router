package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// DiscoveryResult captures the outcome of one DiscoverProvider call.
// Returned (not just logged) so callers — the on-connection-create
// hook (M2.4), the background ticker (M2.5), the on-404 trigger
// (M2.7) — can branch on success/empty/error.
type DiscoveryResult struct {
	Provider string
	Count    int   // models discovered + upserted
	Err      error // any error from the lister or the store
	Empty    bool  // true when the lister returned zero models
	// Skipped is true when a gate short-circuited the call before the
	// lister ran: persistent backoff active (ShouldRunForProvider),
	// the 5-min on-404 debounce window not elapsed, discovery_enabled
	// false, or ProviderMeta missing. Distinct from Empty (lister
	// returned []) — Skipped means no upstream call happened.
	Skipped bool
}

// DiscoveryRunner orchestrates per-provider model discovery. It
// dispatches to the right ModelLister, stamps SourceDiscovery on
// the returned models, persists them through the catalog Store, and
// updates catalog_provider_meta to reflect the run's outcome.
//
// Empty-list safety (AC14b): when a lister returns `(nil, nil)` —
// success with zero models — DiscoveryRunner does NOT delete rows
// for that provider. The existing seed/discovery/openrouter rows
// remain; only catalog_provider_meta.last_discovery_error is updated
// to "empty model list" to surface the situation to the operator.
//
// Failure safety (AC14): when a lister errors, no rows are touched.
// The error is persisted to last_discovery_error and returned.
//
// Unsupported providers (e.g., github-copilot returning
// ErrDiscoveryUnsupported) are treated as a no-op: the result
// reports zero count, no error, and ProviderMeta is left alone.
// This prevents the 24h ticker from flooding last_discovery_error
// with a permanent state.
//
// Construction invariant: callers MUST use NewDiscoveryRunner.
// A bare struct literal (`&DiscoveryRunner{Store: s, Listers: ls}`)
// leaves Clock nil and panics at the first `d.Clock()` call inside
// TryDiscoverOnNotFound or DiscoverProvider. recentlyDiscovered is
// lazy-initialized at write time so the map field tolerates the
// zero value, but Clock has no lazy default — the constructor is
// the only safe path. Defense-in-depth nil-checks for Clock are
// intentionally NOT added here; the panic surfaces the wiring bug
// loudly at first use, which is the better failure mode than
// silently substituting time.Now and hiding the misconstruction.
type DiscoveryRunner struct {
	Store   Store
	Listers map[string]ModelLister

	// Timeout is the per-DiscoverProvider call budget. Wraps the
	// caller's ctx in a child with this deadline. Default 10s if zero.
	Timeout time.Duration

	// Clock is injectable for tests that need deterministic timestamps
	// (M2.6's exponential backoff in particular). Defaults to time.Now.
	Clock func() time.Time

	// recentlyDiscovered tracks per-provider timestamps of the most
	// recent on-404 trigger (M2.7). Used by TryDiscoverOnNotFound to
	// suppress repeated lister calls within onNotFoundDebounce.
	//
	// Lock-order discipline (mirrors ADR-1 §Lock-order discipline for
	// Registry.mu):
	//   - mu guards recentlyDiscovered only.
	//   - NEVER hold mu while calling Store, ShouldRunForProvider, or
	//     any other method that may perform I/O. Acquire, mutate or
	//     read the map, release; then make the Store call.
	//   - TryDiscoverOnNotFound is the canonical pattern: Lock,
	//     check/update the debounce map, Unlock, then call into the
	//     Store and DiscoverProvider. The debounce stamp at the tail
	//     of the function takes mu a second time after the Store
	//     calls return.
	// Re-entrant Store calls under mu would deadlock against an
	// Invalidate that takes a write lock during a debounce read.
	mu                 sync.Mutex
	recentlyDiscovered map[string]time.Time
}

const (
	defaultDiscoveryTimeout = 10 * time.Second

	// onNotFoundDebounce caps how often the on-404 trigger may invoke
	// a lister for the same provider. Persistent backoff (BackoffStep
	// → NextDiscoveryAfter) handles longer cool-down; this map keeps
	// a stream of 404s from re-listing the same /v1/models every
	// request when the backoff window is wide open.
	onNotFoundDebounce = 5 * time.Minute
)

// backoffSchedule maps the persisted BackoffStep counter (1..5) to a
// wait duration. The 5th step caps at 24h — additional consecutive
// failures stay at 24h indefinitely. Replaces the r1 "decode interval
// from NextDiscoveryAfter.Sub(LastDiscoveredAt)" approach which had
// an off-by-one (see ADR-2 §Exponential backoff for the r2 fix).
//
// Curve change at runtime is intentional: editing this slice
// re-interprets existing backoff_step values on the next recordResult
// call (no data migration needed) — see ADR-2 §Curve-change semantics.
var backoffSchedule = []time.Duration{
	1 * time.Hour,
	2 * time.Hour,
	4 * time.Hour,
	8 * time.Hour,
	24 * time.Hour, // cap
}

// NewDiscoveryRunner returns a Runner with sensible defaults
// (10s per-call timeout, real wall clock).
func NewDiscoveryRunner(store Store, listers map[string]ModelLister) *DiscoveryRunner {
	return &DiscoveryRunner{
		Store:              store,
		Listers:            listers,
		Timeout:            defaultDiscoveryTimeout,
		Clock:              time.Now,
		recentlyDiscovered: make(map[string]time.Time),
	}
}

// DiscoverProvider runs the lister for one provider against the given
// credentials, upserts the results, and updates ProviderMeta. The
// result is returned (callers may persist it to a log/metrics surface);
// the runner has already updated the Store before returning.
//
// `provider` MUST match a key in Listers. An unknown provider returns
// a result with Err set — that's a wiring bug worth flagging loudly,
// not a runtime no-op.
func (d *DiscoveryRunner) DiscoverProvider(ctx context.Context, provider string, creds ListerCredentials) DiscoveryResult {
	lister, ok := d.Listers[provider]
	if !ok {
		return DiscoveryResult{
			Provider: provider,
			Err:      fmt.Errorf("catalog: no lister registered for provider %q", provider),
		}
	}

	timeout := d.Timeout
	if timeout == 0 {
		timeout = defaultDiscoveryTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	models, err := lister.ListModels(callCtx, creds)

	// Unsupported provider: treat as a contract no-op, not a failure.
	// Documented in tests as the "permanent state" exclusion.
	if errors.Is(err, ErrDiscoveryUnsupported) {
		return DiscoveryResult{Provider: provider}
	}

	if err != nil {
		// Persist the error to ProviderMeta. Existing rows are NOT
		// touched (AC14). Best-effort: a store-write error here is
		// logged but not chained — the upstream error is what the
		// caller actually cares about.
		d.recordError(ctx, provider, err.Error())
		return DiscoveryResult{Provider: provider, Err: err}
	}

	// Empty-list safety (AC14b): success-with-zero-models. Update
	// ProviderMeta to mark the run AND record the diagnostic note.
	// DO NOT delete rows.
	if len(models) == 0 {
		d.recordSuccess(ctx, provider, "empty model list")
		return DiscoveryResult{Provider: provider, Empty: true}
	}

	// Happy path: stamp SourceDiscovery and upsert.
	var count int
	for _, m := range models {
		// The lister doesn't know about source-precedence — that's
		// the runner's contract. SourceDiscovery is the default for
		// rows coming from a /v1/models endpoint.
		m.Source = SourceDiscovery
		if err := d.Store.UpsertModel(ctx, m); err != nil {
			// Persist what we have so far AND record the error. The
			// caller sees a partial result.
			d.recordError(ctx, provider, fmt.Sprintf("upsert %s/%s: %v", m.Provider, m.ModelID, err))
			return DiscoveryResult{Provider: provider, Count: count, Err: err}
		}
		count++
	}

	d.recordSuccess(ctx, provider, "")
	return DiscoveryResult{Provider: provider, Count: count}
}

// recordSuccess updates ProviderMeta after a successful run. Note
// is "" on the canonical happy path; "empty model list" on AC14b's
// empty-success path.
//
// AC14c: a successful run resets BackoffStep to 0 and clears
// NextDiscoveryAfter — backoff state survives only failures.
// Preserves DiscoveryEnabled / SubscriptionDiscoverable — those are
// seed/operator state.
func (d *DiscoveryRunner) recordSuccess(ctx context.Context, provider, note string) {
	now := d.Clock().UTC()
	cur, err := d.Store.GetProviderMeta(ctx, provider)
	if err != nil {
		slog.Warn("discovery: read ProviderMeta failed", "provider", provider, "err", err)
		return
	}
	if cur == nil {
		// No seeded row — write a minimal one so the operator sees
		// "yes, discovery did run for this provider."
		cur = &ProviderMeta{Provider: provider, DiscoveryEnabled: true}
	}
	cur.LastDiscoveredAt = now
	cur.LastDiscoveryError = note
	cur.BackoffStep = 0
	cur.NextDiscoveryAfter = time.Time{}

	if err := d.Store.SetProviderMeta(ctx, provider, *cur); err != nil {
		slog.Warn("discovery: write ProviderMeta failed", "provider", provider, "err", err)
	}
}

// recordError updates ProviderMeta after a failed run. last_discovered_at
// IS still updated — "we tried and it failed at <time>" is the useful
// operational signal.
//
// AC14c: advances BackoffStep by one (capped at len(backoffSchedule))
// and sets NextDiscoveryAfter = now + backoffSchedule[step-1]. The
// ticker (M2.5) consults ShouldRunForProvider to skip during backoff;
// the on-create hook (M2.4) bypasses this gate explicitly.
func (d *DiscoveryRunner) recordError(ctx context.Context, provider, msg string) {
	now := d.Clock().UTC()
	cur, err := d.Store.GetProviderMeta(ctx, provider)
	if err != nil {
		slog.Warn("discovery: read ProviderMeta failed", "provider", provider, "err", err)
		return
	}
	if cur == nil {
		cur = &ProviderMeta{Provider: provider, DiscoveryEnabled: true}
	}
	cur.LastDiscoveredAt = now
	cur.LastDiscoveryError = msg

	// AC14c — advance backoff step, capped at len(backoffSchedule).
	if cur.BackoffStep < len(backoffSchedule) {
		cur.BackoffStep++
	}
	// step is 1-indexed into the schedule; step=1 → 1h, step=5 → 24h cap.
	cur.NextDiscoveryAfter = now.Add(backoffSchedule[cur.BackoffStep-1])

	if err := d.Store.SetProviderMeta(ctx, provider, *cur); err != nil {
		slog.Warn("discovery: write ProviderMeta failed", "provider", provider, "err", err)
	}
}

// TryDiscoverOnNotFound is the M2.7 entrypoint for "upstream returned
// model-not-found, try to refresh the catalog before giving up." It
// is the on-404 sibling of DiscoverProvider with two extra gates:
//
//  1. Persistent backoff: ShouldRunForProvider is consulted against
//     the live ProviderMeta. A provider whose 24h ticker is in
//     cool-down (NextDiscoveryAfter > now) is skipped — repeated
//     404s do not override the backoff state machine.
//
//  2. 5-minute in-memory debounce: even with backoff open, the same
//     provider is not re-listed within onNotFoundDebounce. This
//     prevents a stream of failed requests from hammering /v1/models.
//
// Returns DiscoveryResult.Skipped=true when either gate blocks. The
// debounce timestamp is recorded for every gate-passing call regardless
// of outcome (success or failure), so a broken upstream doesn't burn
// the whole 5-minute budget on a single bad request.
//
// Callers should invoke fire-and-forget — the underlying DiscoverProvider
// can take up to Timeout (default 10s) under bad network conditions and
// must not block the 404 response to the client.
func (d *DiscoveryRunner) TryDiscoverOnNotFound(ctx context.Context, provider string, creds ListerCredentials) DiscoveryResult {
	now := d.Clock()

	// Debounce gate. Held briefly — no Store calls under d.mu.
	d.mu.Lock()
	if last, ok := d.recentlyDiscovered[provider]; ok && now.Sub(last) < onNotFoundDebounce {
		d.mu.Unlock()
		return DiscoveryResult{Provider: provider, Skipped: true}
	}
	d.mu.Unlock()

	// Persistent backoff gate. Reads ProviderMeta straight from the
	// store — Registry caching doesn't apply to provider_meta state.
	meta, err := d.Store.GetProviderMeta(ctx, provider)
	if err != nil {
		return DiscoveryResult{Provider: provider, Err: fmt.Errorf("on-404: read ProviderMeta: %w", err)}
	}
	// Missing meta row → fail-closed skip. Bootstrap seeds meta rows
	// for every KnownProvider, so a nil result indicates a wiring
	// gap, not a runtime expected condition. Surface the wiring gap
	// via slog.Warn — silent skips obscure the fact that on-404
	// discovery is not running at all for this provider; operators
	// would otherwise discover it only by absence-of-discovery, which
	// reads as "everything is fine" until a separate signal contradicts.
	if meta == nil {
		slog.Warn("on-404 discovery skipped: no ProviderMeta row",
			"provider", provider,
			"hint", "bootstrap seeds meta rows for every KnownProvider; nil indicates wiring gap or post-bootstrap deletion")
		return DiscoveryResult{Provider: provider, Skipped: true}
	}
	if !d.ShouldRunForProvider(*meta, now) {
		return DiscoveryResult{Provider: provider, Skipped: true}
	}

	res := d.DiscoverProvider(ctx, provider, creds)

	// Stamp the debounce map. Failures count — backoff-state covers
	// longer windows; the 5-min debounce protects against the same
	// failed request triggering a fresh lister call N requests later.
	d.mu.Lock()
	if d.recentlyDiscovered == nil {
		d.recentlyDiscovered = make(map[string]time.Time)
	}
	d.recentlyDiscovered[provider] = now
	d.mu.Unlock()

	return res
}

// ShouldRunForProvider reports whether the scheduled discovery
// machinery (the 24h ticker in M2.5, the on-404 trigger in M2.7)
// should invoke DiscoverProvider right now.
//
// Returns false when:
//   - the provider has DiscoveryEnabled=false (operator/seed opt-out)
//   - the provider's NextDiscoveryAfter is in the future (backoff active)
//
// The on-create discovery hook (M2.4) does NOT consult this method
// — adding a connection is an explicit user action that overrides
// backoff once (per ADR-2 §"On-connection-create discovery bypasses
// backoff once").
func (d *DiscoveryRunner) ShouldRunForProvider(meta ProviderMeta, now time.Time) bool {
	if !meta.DiscoveryEnabled {
		return false
	}
	if !meta.NextDiscoveryAfter.IsZero() && now.Before(meta.NextDiscoveryAfter) {
		return false
	}
	return true
}
