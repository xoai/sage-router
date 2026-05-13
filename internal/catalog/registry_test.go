package catalog

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// reentranceDetector wraps a Store and panics if any Store method is
// called while the Registry's mu is held. This is the runtime
// enforcement of the ADR-1/ADR-3 lock-order discipline: Registry.mu
// MUST be released before any Store call. AC31 verification.
type reentranceDetector struct {
	inner    Store
	muHeld   *atomic.Int32
	violated atomic.Bool
}

func newReentranceDetector(inner Store, muHeld *atomic.Int32) *reentranceDetector {
	return &reentranceDetector{inner: inner, muHeld: muHeld}
}

func (d *reentranceDetector) check(method string) {
	if d.muHeld.Load() > 0 {
		d.violated.Store(true)
		panic("AC31 violation: Store." + method + " called while Registry.mu held")
	}
}

func (d *reentranceDetector) UpsertModel(ctx context.Context, m Model) error {
	d.check("UpsertModel")
	return d.inner.UpsertModel(ctx, m)
}
func (d *reentranceDetector) UpsertPricing(ctx context.Context, provider, modelID string, p Pricing) error {
	d.check("UpsertPricing")
	return d.inner.UpsertPricing(ctx, provider, modelID, p)
}
func (d *reentranceDetector) GetModel(ctx context.Context, provider, modelID string) (*Model, error) {
	d.check("GetModel")
	return d.inner.GetModel(ctx, provider, modelID)
}
func (d *reentranceDetector) GetPricing(ctx context.Context, provider, modelID string) (*Pricing, error) {
	d.check("GetPricing")
	return d.inner.GetPricing(ctx, provider, modelID)
}
func (d *reentranceDetector) ListModels(ctx context.Context, provider string) ([]Model, error) {
	d.check("ListModels")
	return d.inner.ListModels(ctx, provider)
}
func (d *reentranceDetector) BulkUpsertPricing(ctx context.Context, updates []PricingUpdate) error {
	d.check("BulkUpsertPricing")
	return d.inner.BulkUpsertPricing(ctx, updates)
}
func (d *reentranceDetector) DeletePricing(ctx context.Context, provider, modelID string) error {
	d.check("DeletePricing")
	return d.inner.DeletePricing(ctx, provider, modelID)
}
func (d *reentranceDetector) SetProviderMeta(ctx context.Context, provider string, m ProviderMeta) error {
	d.check("SetProviderMeta")
	return d.inner.SetProviderMeta(ctx, provider, m)
}
func (d *reentranceDetector) GetProviderMeta(ctx context.Context, provider string) (*ProviderMeta, error) {
	d.check("GetProviderMeta")
	return d.inner.GetProviderMeta(ctx, provider)
}
func (d *reentranceDetector) ListProviderMetas(ctx context.Context) ([]ProviderMeta, error) {
	d.check("ListProviderMetas")
	return d.inner.ListProviderMetas(ctx)
}

// TestRegistry_NoStoreCallsUnderMu — AC31. Verifies the runtime
// contract that Registry never holds its own mu while calling Store
// methods. Exercises every Registry-public method (Lookup, Pricing,
// EstimateCost, ListProvider, Invalidate) and asserts no panic.
func TestRegistry_NoStoreCallsUnderMu(t *testing.T) {
	cs, _, ctx := freshStore(t)
	// Seed two known rows so Lookup actually goes to the store.
	if err := cs.UpsertModel(ctx, Model{
		Provider: "openai", ModelID: "gpt-4.1",
		DisplayName: "GPT-4.1", Tier: TierFrontier,
		Source: SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := cs.UpsertPricing(ctx, "openai", "gpt-4.1",
		Pricing{Input: 2.0, Output: 8.0, Source: SourceSeed}); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	// Build a Registry, then re-wrap its underlying store with the
	// detector. The detector reads from the same muHeld counter the
	// Registry uses internally.
	reg := NewRegistry(cs)
	rr := reg.(*registry)
	det := newReentranceDetector(rr.store, &rr.muHeld)
	rr.store = det

	// Exercise every public method. None must trigger the detector.
	_, _ = reg.Lookup("openai", "gpt-4.1") // cold cache — must hit store
	_, _ = reg.Lookup("openai", "gpt-4.1") // warm cache — no store call
	if p := reg.Pricing("openai", "gpt-4.1"); p == nil || p.Input != 2.0 {
		t.Errorf("Pricing returned %+v", p)
	}
	if cost := reg.EstimateCost("openai", "gpt-4.1", TokenBreakdown{Input: 1_000_000}); cost != 2.0 {
		t.Errorf("EstimateCost = %v, want 2.0", cost)
	}
	if models := reg.ListProvider("openai"); len(models) == 0 {
		t.Error("ListProvider returned nothing")
	}
	reg.Invalidate()
	// Force another cold-cache read after invalidate.
	_, _ = reg.Lookup("openai", "gpt-4.1")

	if det.violated.Load() {
		t.Fatal("AC31: detector tripped — Registry called Store while holding mu")
	}
}

// TestRegistry_ConcurrentAccessRaceClean stresses Lookup / Invalidate
// under concurrent goroutines. Must be race-clean under `go test -race`.
// (Note: the AC31 reentrance detector is NOT used here. The detector
// is a single-goroutine discipline check — its muHeld counter is a
// global atomic, not per-goroutine, so under concurrency the detector
// can fire when goroutine A holds mu for Invalidate while goroutine B
// is in its lock-free store call. The detector remains canonical for
// single-goroutine discipline; concurrency safety is verified here
// by -race detection.)
func TestRegistry_ConcurrentAccessRaceClean(t *testing.T) {
	cs, _, ctx := freshStore(t)
	if err := cs.UpsertModel(ctx, Model{
		Provider: "openai", ModelID: "gpt-4.1",
		Source: SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := cs.UpsertPricing(ctx, "openai", "gpt-4.1",
		Pricing{Input: 2.0, Source: SourceSeed}); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	reg := NewRegistry(cs)

	const goroutines = 32
	const iterations = 100
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, _ = reg.Lookup("openai", "gpt-4.1")
				if j%10 == 0 {
					reg.Invalidate()
				}
			}
		}()
	}
	wg.Wait()
	// Test body is success-by-completion: -race would report any data
	// race on the shared cache; absence of a panic / race report is
	// the assertion.
}

// TestRegistry_Lookup_CachesAfterFirstRead — second call must NOT
// invoke the underlying Store.GetModel (verifies the cache actually
// works).
func TestRegistry_Lookup_CachesAfterFirstRead(t *testing.T) {
	cs, _, ctx := freshStore(t)
	if err := cs.UpsertModel(ctx, Model{
		Provider: "openai", ModelID: "gpt-4.1", Source: SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}

	counter := &callCountingStore{inner: cs}
	reg := NewRegistry(counter)

	_, _ = reg.Lookup("openai", "gpt-4.1")
	_, _ = reg.Lookup("openai", "gpt-4.1")
	_, _ = reg.Lookup("openai", "gpt-4.1")

	if counter.getModelCalls != 1 {
		t.Errorf("expected 1 GetModel call (cache hits should bypass store), got %d", counter.getModelCalls)
	}
}

// TestRegistry_Lookup_NotFoundIsNotCached — negative results MUST NOT
// be cached, so a later discovery / seed write can make the model
// appear. (Documented in ADR-3 §Part A read-path semantics.)
func TestRegistry_Lookup_NotFoundIsNotCached(t *testing.T) {
	cs, _, ctx := freshStore(t)
	counter := &callCountingStore{inner: cs}
	reg := NewRegistry(counter)

	if _, ok := reg.Lookup("openai", "no-such"); ok {
		t.Error("Lookup returned ok=true for unknown model")
	}
	if _, ok := reg.Lookup("openai", "no-such"); ok {
		t.Error("Lookup returned ok=true on second call")
	}

	if counter.getModelCalls != 2 {
		t.Errorf("expected 2 GetModel calls (misses must not cache), got %d", counter.getModelCalls)
	}

	// Now seed the row.
	if err := cs.UpsertModel(ctx, Model{
		Provider: "openai", ModelID: "no-such", Source: SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}

	// Next Lookup must see it (no negative-cache occlusion).
	if _, ok := reg.Lookup("openai", "no-such"); !ok {
		t.Error("Lookup did not pick up the new row after seed")
	}
}

// TestRegistry_Invalidate_ClearsCache — Invalidate must force the
// next Lookup to round-trip to the store.
func TestRegistry_Invalidate_ClearsCache(t *testing.T) {
	cs, _, ctx := freshStore(t)
	if err := cs.UpsertModel(ctx, Model{
		Provider: "openai", ModelID: "gpt-4.1", Source: SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}

	counter := &callCountingStore{inner: cs}
	reg := NewRegistry(counter)

	_, _ = reg.Lookup("openai", "gpt-4.1")
	_, _ = reg.Lookup("openai", "gpt-4.1")
	if counter.getModelCalls != 1 {
		t.Errorf("expected 1 GetModel call before invalidate, got %d", counter.getModelCalls)
	}

	reg.Invalidate()
	_, _ = reg.Lookup("openai", "gpt-4.1")
	if counter.getModelCalls != 2 {
		t.Errorf("expected 2 GetModel calls after invalidate, got %d", counter.getModelCalls)
	}
}

// callCountingStore is a test-only Store decorator that counts
// invocations of methods we care about.
type callCountingStore struct {
	inner         Store
	getModelCalls int
}

func (c *callCountingStore) UpsertModel(ctx context.Context, m Model) error {
	return c.inner.UpsertModel(ctx, m)
}
func (c *callCountingStore) UpsertPricing(ctx context.Context, p, m string, pr Pricing) error {
	return c.inner.UpsertPricing(ctx, p, m, pr)
}
func (c *callCountingStore) GetModel(ctx context.Context, provider, modelID string) (*Model, error) {
	c.getModelCalls++
	return c.inner.GetModel(ctx, provider, modelID)
}
func (c *callCountingStore) GetPricing(ctx context.Context, provider, modelID string) (*Pricing, error) {
	return c.inner.GetPricing(ctx, provider, modelID)
}
func (c *callCountingStore) ListModels(ctx context.Context, provider string) ([]Model, error) {
	return c.inner.ListModels(ctx, provider)
}
func (c *callCountingStore) BulkUpsertPricing(ctx context.Context, u []PricingUpdate) error {
	return c.inner.BulkUpsertPricing(ctx, u)
}
func (c *callCountingStore) DeletePricing(ctx context.Context, provider, modelID string) error {
	return c.inner.DeletePricing(ctx, provider, modelID)
}
func (c *callCountingStore) SetProviderMeta(ctx context.Context, p string, m ProviderMeta) error {
	return c.inner.SetProviderMeta(ctx, p, m)
}
func (c *callCountingStore) GetProviderMeta(ctx context.Context, p string) (*ProviderMeta, error) {
	return c.inner.GetProviderMeta(ctx, p)
}
func (c *callCountingStore) ListProviderMetas(ctx context.Context) ([]ProviderMeta, error) {
	return c.inner.ListProviderMetas(ctx)
}
