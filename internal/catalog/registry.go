package catalog

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// registry is the read-side fast path over a Store. It caches Model
// rows in memory and invalidates on demand. Hot path is Lookup +
// Pricing + EstimateCost.
//
// Lock-order contract (ADR-3 §Lock-order discipline + ADR-1):
// mu MUST NOT be held while calling any Store method. The cold-cache
// path is: RLock → check → RUnlock → store call (no lock) → Lock →
// write cache → Unlock. The runtime re-entrance detector test
// (registry_test.go: TestRegistry_NoStoreCallsUnderMu — AC31)
// enforces this discipline at test time via an instrumented Store
// decorator.
//
// muHeld is an atomic counter the detector reads. Every acquire of
// mu increments it; every release decrements it. The detector
// panics if muHeld > 0 when any Store method is called. The counter
// is intentionally exposed via a struct field on the concrete type
// (not the interface) so it's testable without polluting the
// public API.
type registry struct {
	store  Store
	mu     sync.RWMutex
	muHeld atomic.Int32
	cache  map[string]*Model
}

// NewRegistry wraps a Store with the in-memory caching Registry.
// Safe to call from multiple goroutines after construction.
func NewRegistry(store Store) Registry {
	return &registry{
		store: store,
		cache: make(map[string]*Model),
	}
}

const lookupTimeout = 2 * time.Second

func cacheKey(provider, modelID string) string {
	return provider + "/" + modelID
}

// rlock / runlock / lock / unlock are the ONLY places mu is touched.
// They keep the muHeld counter in sync with the actual lock state.
func (r *registry) rlock() {
	r.muHeld.Add(1)
	r.mu.RLock()
}
func (r *registry) runlock() {
	r.mu.RUnlock()
	r.muHeld.Add(-1)
}
func (r *registry) lock() {
	r.muHeld.Add(1)
	r.mu.Lock()
}
func (r *registry) unlock() {
	r.mu.Unlock()
	r.muHeld.Add(-1)
}

// Lookup returns the Model for (provider, modelID), or nil, false
// when no such row exists. Read-mostly hot path: cache hit returns
// without any store roundtrip; cache miss releases the lock before
// the store call to honor the lock-order contract.
//
// Negative results are NOT cached — a future seed / discovery write
// will make the row available without a manual Invalidate. See
// ADR-3 §Part A.
func (r *registry) Lookup(provider, modelID string) (*Model, bool) {
	key := cacheKey(provider, modelID)
	r.rlock()
	if m, ok := r.cache[key]; ok {
		r.runlock()
		// Defensive copy: callers may mutate the returned struct
		// without affecting the cached row.
		cp := *m
		return &cp, true
	}
	r.runlock()

	// Cold cache. NO LOCK HELD across the store call.
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	m, err := r.store.GetModel(ctx, provider, modelID)
	if err != nil {
		slog.Warn("catalog: lookup failed",
			"provider", provider, "model", modelID, "err", err)
		return nil, false
	}
	if m == nil {
		// Negative cache disabled: don't cache the miss.
		return nil, false
	}

	r.lock()
	r.cache[key] = m
	r.unlock()

	cp := *m
	return &cp, true
}

// Pricing returns just the Pricing for (provider, modelID), or nil
// when unknown. Reuses Lookup so the caching behavior is shared.
func (r *registry) Pricing(provider, modelID string) *Pricing {
	m, ok := r.Lookup(provider, modelID)
	if !ok {
		return nil
	}
	p := m.Pricing
	return &p
}

// EstimateCost computes USD cost for the given token breakdown using
// the current pricing for (provider, modelID). Returns 0 for unknown
// models — matches today's config.EstimateCost behavior.
func (r *registry) EstimateCost(provider, modelID string, br TokenBreakdown) float64 {
	p := r.Pricing(provider, modelID)
	if p == nil {
		return 0
	}
	return p.EstimateCost(br)
}

// ListProvider returns every model for the given provider, or all
// models when provider == "". Ordered by (provider, model_id) ASC
// per the Store contract (AC11b). NOT cached — list operations are
// cold-path (dashboard, smart-candidate build) and the bounded
// catalog size makes per-call SQLite queries cheap.
//
// The lock-order contract is trivially honored here: the call to
// store.ListModels happens outside any mu acquisition.
func (r *registry) ListProvider(provider string) []Model {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	models, err := r.store.ListModels(ctx, provider)
	if err != nil {
		slog.Warn("catalog: list provider failed",
			"provider", provider, "err", err)
		return nil
	}
	return models
}

// Invalidate clears the in-memory cache. Call after any write to
// the underlying Store (discovery refresh, OpenRouter pricing
// refresh, user pricing override) so the next read sees fresh data.
func (r *registry) Invalidate() {
	r.lock()
	r.cache = make(map[string]*Model)
	r.unlock()
}
