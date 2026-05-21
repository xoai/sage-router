package provider

import (
	"errors"
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
}

// NewSelector creates an empty Selector.
func NewSelector() *Selector {
	return &Selector{
		conns: make(map[string][]*Connection),
		byID:  make(map[string]*Connection),
	}
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
	orderCandidates(candidates, strategy)

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
// SelectP2C and SelectResetAware are placeholders until M3 T5/T6 — they
// deliberately fall through to the SelectDefault ordering for now, so the
// strategy parameter is fully plumbed before the new orderings land.
func orderCandidates(candidates []*Connection, strategy SelectStrategy) {
	switch strategy {
	case SelectP2C:
		// T5 fills this arm with power-of-two-choices ordering.
		sort.SliceStable(candidates, defaultLess(candidates))
	case SelectResetAware:
		// T6 fills this arm with soonest-reset ordering.
		sort.SliceStable(candidates, defaultLess(candidates))
	default:
		sort.SliceStable(candidates, defaultLess(candidates))
	}
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
