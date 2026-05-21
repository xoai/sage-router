package provider

import (
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"
)

var (
	// ErrNoConnections is returned when no connections are registered for a provider.
	ErrNoConnections = errors.New("no connections registered for provider")

	// ErrAllUnavailable is returned when every connection is unavailable.
	ErrAllUnavailable = errors.New("all connections unavailable")
)

// SelectStrategy chooses how Select orders the candidate connections before
// the HALF_OPEN claim-walk. The filter chain and the claim-walk are identical
// for every strategy — only the candidate ordering differs (M3 spec §4.3).
type SelectStrategy int

const (
	// SelectDefault is today's ordering: a healthy (CLOSED) breaker before a
	// probing (HALF_OPEN) one, then user priority ascending, then consecutive
	// uses ascending (round-robin spread).
	SelectDefault SelectStrategy = iota
	// SelectP2C is power-of-two-choices over an LRU/recency signal (M3 §5).
	SelectP2C
	// SelectResetAware orders by soonest quota-window reset (M3 §5).
	SelectResetAware
)

// SelectResult carries the outcome of a selection attempt.
type SelectResult struct {
	// Connection is the chosen connection, or nil if none are available.
	Connection *Connection

	// AllRateLimited is true when every non-excluded connection for the
	// provider is unavailable specifically because its breaker is OPEN with a
	// rate-limit failure kind — a retry-after-cooldown situation, as opposed
	// to an errored or auth-expired connection. (The name predates the
	// three-facet model; M2 review MC1 kept it and documented the precise
	// meaning rather than rename a field consumed across the request path.)
	AllRateLimited bool

	// EarliestRetry is the earliest time at which a rate-limited connection
	// becomes available again. Zero value if not applicable.
	EarliestRetry time.Time
}

// Selector manages connections per provider and implements the selection algorithm.
type Selector struct {
	mu    sync.RWMutex
	conns map[string][]*Connection // provider → connections
	byID  map[string]*Connection   // connection ID → connection

	// rng is the SelectP2C sampling source — a local instance (not the
	// math/rand package global) so tests can seed it deterministically.
	// rngMu guards it: *rand.Rand is not safe for concurrent use.
	rngMu sync.Mutex
	rng   *rand.Rand
}

// NewSelector creates an empty Selector.
func NewSelector() *Selector {
	return &Selector{
		conns: make(map[string][]*Connection),
		byID:  make(map[string]*Connection),
		rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// seedRNGForTest reseeds the SelectP2C sampling source so distribution tests
// are deterministic. Test-only.
func (s *Selector) seedRNGForTest(seed int64) {
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	s.rng = rand.New(rand.NewSource(seed))
}

// Register adds a connection to the selector. If a connection with the same ID
// already exists it is replaced.
func (s *Selector) Register(conn *Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove any existing connection with this ID first.
	if existing, ok := s.byID[conn.ID]; ok {
		s.removeLocked(existing.ID)
	}

	s.conns[conn.Provider] = append(s.conns[conn.Provider], conn)
	s.byID[conn.ID] = conn
}

// Remove deletes a connection by ID.
func (s *Selector) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(id)
}

// removeLocked removes a connection without acquiring the lock. Caller must hold write lock.
func (s *Selector) removeLocked(id string) {
	conn, ok := s.byID[id]
	if !ok {
		return
	}
	delete(s.byID, id)

	provider := conn.Provider
	conns := s.conns[provider]
	for i, c := range conns {
		if c.ID == id {
			s.conns[provider] = append(conns[:i], conns[i+1:]...)
			break
		}
	}
	if len(s.conns[provider]) == 0 {
		delete(s.conns, provider)
	}
}

// Select picks the best selectable connection for the given provider and model.
//
// Algorithm:
//  1. Skip connections whose IDs appear in excludeIDs.
//  2. Filter to Selectable connections — breaker CLOSED or HALF_OPEN, auth
//     valid, idle, and the model not rate-limit-locked, not denylisted, and
//     permitted by the subscription tier.
//  3. Order candidates per strategy (orderCandidates). SelectDefault: CLOSED
//     before HALF_OPEN (healthy before probing), then user priority ascending,
//     then consecutive uses ascending (spread). SelectP2C / SelectResetAware
//     apply their own ordering (M3 §5).
//  4. Walk the ordered candidates and return the first whose HALF_OPEN trial
//     slot can be claimed — a CLOSED connection always claims without
//     consuming a slot; a HALF_OPEN connection only if it wins the atomic
//     compare-and-set (ADR-1 §HALF_OPEN gate). The caller MUST pair the
//     returned connection with ReleaseHalfOpenTrial on every terminal path.
//
// If no candidate is available, the returned SelectResult reports whether
// every connection is rate-limit-cooling-down, and the earliest retry time.
func (s *Selector) Select(provider, model string, excludeIDs []string, strategy SelectStrategy) (*SelectResult, error) {
	s.mu.RLock()
	conns, ok := s.conns[provider]
	if !ok || len(conns) == 0 {
		s.mu.RUnlock()
		return nil, ErrNoConnections
	}
	// Snapshot the slice reference under the read lock; each connection has
	// its own lock for the facet reads below.
	snapshot := make([]*Connection, len(conns))
	copy(snapshot, conns)
	s.mu.RUnlock()

	excluded := make(map[string]bool, len(excludeIDs))
	for _, id := range excludeIDs {
		excluded[id] = true
	}

	var candidates []*Connection
	var rateLimitedCount int
	var earliestRetry time.Time
	for _, c := range snapshot {
		if excluded[c.ID] {
			continue
		}
		if c.Selectable(model) {
			candidates = append(candidates, c)
			continue
		}
		// Not selectable — track rate-limit cooldowns for the result metadata
		// (a breaker opened by a 429, as opposed to a transient/errored fault).
		if c.Breaker() == BreakerOpen && c.FailureKind() == FailureRateLimit {
			rateLimitedCount++
			cu := c.CooldownUntil()
			if earliestRetry.IsZero() || (!cu.IsZero() && cu.Before(earliestRetry)) {
				earliestRetry = cu
			}
		}
	}

	if len(candidates) == 0 {
		allRL := rateLimitedCount > 0 && rateLimitedCount == len(snapshot)-len(excluded)
		return &SelectResult{
			Connection:     nil,
			AllRateLimited: allRL,
			EarliestRetry:  earliestRetry,
		}, ErrAllUnavailable
	}

	// Order the candidates per the requested strategy. The claim-walk below
	// then takes the first claimable one.
	s.orderCandidates(candidates, strategy)

	// Claim-the-winner. A CLOSED candidate claims without consuming a slot; a
	// HALF_OPEN candidate claims only if it wins the compare-and-set — a loser
	// is skipped (its single trial is already in flight elsewhere). The
	// re-check inside TryClaimHalfOpenTrial closes the gap between the filter
	// pass above and the claim.
	for _, c := range candidates {
		if c.TryClaimHalfOpenTrial(model) {
			return &SelectResult{Connection: c}, nil
		}
	}

	// Every candidate was a HALF_OPEN connection whose single trial slot is
	// already taken (or it stopped being selectable mid-pick).
	return &SelectResult{Connection: nil}, ErrAllUnavailable
}

// selectionRank orders selectable candidates: a healthy (CLOSED) breaker is
// preferred over a probing (HALF_OPEN) one. Selectable candidates are always
// Idle (Connection.Selectable requires it), so lifecycle does not enter the
// rank.
func selectionRank(c *Connection) int {
	if c.Breaker() == BreakerClosed {
		return 0
	}
	return 1 // HALF_OPEN
}

// orderCandidates arranges candidates in place into the order the requested
// strategy prefers. Select's claim-walk then takes the first claimable one.
//
// SelectResetAware is a placeholder until M3 T6 — it falls through to the
// SelectDefault ordering for now.
func (s *Selector) orderCandidates(candidates []*Connection, strategy SelectStrategy) {
	switch strategy {
	case SelectP2C:
		s.p2cOrder(candidates)
	case SelectResetAware:
		// T6 fills this arm with soonest-reset ordering.
		sort.SliceStable(candidates, defaultLess(candidates))
	default:
		sort.SliceStable(candidates, defaultLess(candidates))
	}
}

// p2cOrder applies the power-of-two-choices ordering (M3 spec §5). It samples
// two distinct candidates uniformly at random and lifts them to the front —
// the less-recently-used of the two (older LastUsedAt) first, the other
// second — with the remaining candidates behind them in SelectDefault order.
//
// The load signal is LastUsedAt: every selectable candidate is Idle, so there
// is no live in-flight load to sample; p2c is honestly power-of-two-choices
// over an LRU/recency proxy, kept for its herd-avoidance property — random-2
// sampling stops every concurrent request stampeding the single coldest
// connection. A never-used connection has a zero LastUsedAt and sorts as
// least-recently-used (correct: maximally cold). With <=1 candidate there is
// nothing to sample, so it degrades to SelectDefault.
func (s *Selector) p2cOrder(candidates []*Connection) {
	// SelectDefault baseline first — this fixes the order of "the rest".
	sort.SliceStable(candidates, defaultLess(candidates))
	n := len(candidates)
	if n <= 1 {
		return
	}

	// Sample two distinct indices uniformly: pick j from the n-1 indices that
	// are not i by drawing in [0,n-1) and skipping past i.
	s.rngMu.Lock()
	i := s.rng.Intn(n)
	j := s.rng.Intn(n - 1)
	s.rngMu.Unlock()
	if j >= i {
		j++
	}

	// Less-recently-used (older LastUsedAt) of the two picks goes first.
	a, b := candidates[i], candidates[j]
	if b.LastUsedAt().Before(a.LastUsedAt()) {
		a, b = b, a
	}

	// Rebuild: a, b, then every other candidate in its existing
	// (SelectDefault) order.
	rest := make([]*Connection, 0, n-2)
	for k, c := range candidates {
		if k != i && k != j {
			rest = append(rest, c)
		}
	}
	candidates[0], candidates[1] = a, b
	copy(candidates[2:], rest)
}

// defaultLess is the SelectDefault candidate comparator: a healthy (CLOSED)
// breaker before a probing (HALF_OPEN) one, then user priority ascending,
// then consecutive uses ascending (round-robin spread). It is the regression
// baseline — this ordering must match the pre-M3 selector exactly.
func defaultLess(candidates []*Connection) func(i, j int) bool {
	return func(i, j int) bool {
		ri, rj := selectionRank(candidates[i]), selectionRank(candidates[j])
		if ri != rj {
			return ri < rj
		}
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		return candidates[i].ConsecutiveUses() < candidates[j].ConsecutiveUses()
	}
}

// AllConnections returns all connections registered for a provider.
// The returned slice is a snapshot; mutations to it do not affect the selector.
func (s *Selector) AllConnections(provider string) []*Connection {
	s.mu.RLock()
	defer s.mu.RUnlock()

	conns := s.conns[provider]
	if len(conns) == 0 {
		return nil
	}
	out := make([]*Connection, len(conns))
	copy(out, conns)
	return out
}

// ConnectionByID returns a single connection by ID, or nil if not found.
func (s *Selector) ConnectionByID(id string) *Connection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byID[id]
}

// SnapshotAll returns every registered connection across every provider
// as a single flat slice. Used by the refresh loop to enumerate
// candidates without needing to know which providers are in play.
// The returned slice is a snapshot; mutations to it don't affect the
// selector.
func (s *Selector) SnapshotAll() []*Connection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.byID) == 0 {
		return nil
	}
	out := make([]*Connection, 0, len(s.byID))
	for _, c := range s.byID {
		out = append(out, c)
	}
	return out
}
