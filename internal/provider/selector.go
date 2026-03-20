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

// SelectResult carries the outcome of a selection attempt.
type SelectResult struct {
	// Connection is the chosen connection, or nil if none are available.
	Connection *Connection

	// AllRateLimited is true when every connection for the provider is in
	// Cooldown or RateLimited state.
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

// Select picks the best available connection for the given provider and model.
//
// Algorithm:
//  1. Filter out connections whose IDs appear in excludeIDs.
//  2. Filter to connections that are available for the requested model.
//  3. Sort candidates by: state priority (asc) → user priority (asc) → consecutive uses (asc).
//  4. Return the top candidate.
//
// If no candidate is available, the returned SelectResult indicates whether all
// connections are rate-limited and the earliest retry time.
func (s *Selector) Select(provider, model string, excludeIDs []string) (*SelectResult, error) {
	s.mu.RLock()
	conns, ok := s.conns[provider]
	if !ok || len(conns) == 0 {
		s.mu.RUnlock()
		return nil, ErrNoConnections
	}

	// Snapshot the slice reference under read lock; individual connections have
	// their own locks for state reads.
	snapshot := make([]*Connection, len(conns))
	copy(snapshot, conns)
	s.mu.RUnlock()

	// Build exclusion set.
	excluded := make(map[string]bool, len(excludeIDs))
	for _, id := range excludeIDs {
		excluded[id] = true
	}

	// Partition into candidates and rate-limited connections.
	var candidates []*Connection
	var rateLimitedCount int
	var earliestRetry time.Time

	for _, c := range snapshot {
		if excluded[c.ID] {
			continue
		}

		if c.IsAvailable(model) {
			candidates = append(candidates, c)
			continue
		}

		// Track rate-limited / cooldown connections for the result metadata.
		st := c.State()
		if st == StateRateLimited || st == StateCooldown {
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

	// Sort: state priority ascending, then user priority ascending, then
	// consecutive uses ascending (spread traffic).
	sort.SliceStable(candidates, func(i, j int) bool {
		si := statePriority(candidates[i].State())
		sj := statePriority(candidates[j].State())
		if si != sj {
			return si < sj
		}
		pi := candidates[i].Priority
		pj := candidates[j].Priority
		if pi != pj {
			return pi < pj
		}
		return candidates[i].ConsecutiveUses() < candidates[j].ConsecutiveUses()
	})

	return &SelectResult{Connection: candidates[0]}, nil
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
