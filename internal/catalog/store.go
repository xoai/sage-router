package catalog

import (
	"context"
	"time"
)

// Store is the persistence interface for the catalog. Implementations
// must enforce the write-side source precedence rules documented in
// ADR-2 §Conflict resolution. See `sqlite.go` for the modernc.org/sqlite
// implementation that ships with sage-router.
//
// Lock-order discipline: Store methods MUST NOT acquire any mutex
// other than their own internal SQLite locking. Callers (notably
// Registry) must release any held mutex before calling Store methods.
// The runtime re-entrance detector test (registry_test.go) enforces
// this for Registry.mu; the discipline is documented in ADR-3 and
// ADR-1 §Lock-order discipline.
type Store interface {
	// UpsertModel inserts or updates one catalog_models row, honoring
	// source-precedence rules:
	//   - seed cannot overwrite anything non-seed
	//   - user cannot be overwritten by anything except user
	UpsertModel(ctx context.Context, m Model) error

	// UpsertPricing inserts or updates one catalog_pricing row.
	// Same precedence rules as UpsertModel, plus the extra rule that
	// discovery may NOT clobber openrouter pricing (capability
	// columns only).
	UpsertPricing(ctx context.Context, provider, modelID string, p Pricing) error

	// GetModel returns the catalog row for the given (provider, modelID)
	// joined with its catalog_pricing row. Returns nil, nil when no
	// such row exists.
	GetModel(ctx context.Context, provider, modelID string) (*Model, error)

	// GetPricing returns just the pricing row. Returns nil, nil when
	// no such row exists.
	GetPricing(ctx context.Context, provider, modelID string) (*Pricing, error)

	// ListModels returns every model row for the given provider, OR all
	// rows when provider == "". Rows are ordered by (provider, model_id)
	// ASC — AC11b. This determinism guarantees the M1.9-baseline golden
	// snapshot is stable across runs.
	ListModels(ctx context.Context, provider string) ([]Model, error)

	// BulkUpsertPricing applies multiple pricing updates. Used by the
	// OpenRouter refresher (M2) and the seeder (M1.7). Respects the
	// same source precedence as UpsertPricing.
	BulkUpsertPricing(ctx context.Context, updates []PricingUpdate) error

	// DeletePricing removes the catalog_pricing row for (provider,
	// modelID). Used by the user-pricing-override DELETE endpoint
	// (M2.10) — the operator "removes my override" and lets the next
	// discovery / OpenRouter refresh cycle re-populate. Returns nil
	// (not error) when no row exists — DELETE is idempotent. The
	// model row in catalog_models is NOT touched; only pricing is
	// removed.
	DeletePricing(ctx context.Context, provider, modelID string) error

	// SetProviderMeta upserts the catalog_provider_meta row for one
	// provider. Used by the seeder + the discovery loop's
	// recordResult.
	SetProviderMeta(ctx context.Context, provider string, meta ProviderMeta) error

	// GetProviderMeta returns the catalog_provider_meta row, or
	// nil, nil if no row exists. An absent row is treated by the
	// discovery loop as "discovery disabled" (fail-closed).
	GetProviderMeta(ctx context.Context, provider string) (*ProviderMeta, error)

	// ListProviderMetas returns every catalog_provider_meta row for
	// dashboard/operator surfaces. Used by /api/providers to surface
	// last_discovered_at + last_discovery_error per provider; NOT
	// used to enumerate KnownProviders (those are static — see RC1).
	ListProviderMetas(ctx context.Context) ([]ProviderMeta, error)
}

// Registry is the read-side fast path the rest of the codebase
// depends on. It wraps a Store with an in-memory cache; cache misses
// flow through to Store with strict lock-order discipline (mu is
// released before any Store call). See registry.go for the
// implementation and ADR-3 §Lock-order discipline for the contract.
//
// Test cross-reference: TestRegistry_NoStoreCallsUnderMu in
// registry_test.go enforces the discipline at runtime via an
// instrumented Store decorator (AC31).
type Registry interface {
	// Lookup returns the Model for (provider, modelID), or nil, false
	// when no such row is cached or on the underlying Store. Hot path.
	Lookup(provider, modelID string) (*Model, bool)

	// Pricing returns the current pricing for (provider, modelID).
	// Returns nil when unknown — callers should treat that as $0,
	// matching today's config.EstimateCost behavior on unknown models.
	Pricing(provider, modelID string) *Pricing

	// EstimateCost computes the USD cost for the given token breakdown
	// using current pricing for (provider, modelID). Returns 0 on
	// unknown models. Convenience wrapper around Pricing(...).EstimateCost.
	EstimateCost(provider, modelID string, br TokenBreakdown) float64

	// ListProvider returns every model for the given provider, ordered
	// by model_id ASC. Provider == "" returns all models. The slice
	// is freshly allocated; callers may mutate it.
	ListProvider(provider string) []Model

	// Invalidate clears the in-memory cache. Call after any write to
	// the underlying Store (discovery, OpenRouter refresh, user
	// pricing override) so the next read sees fresh data.
	Invalidate()
}

// IsValidSource reports whether s is one of the four catalog source
// enum values. Used by store implementations to validate input
// before issuing SQL.
func IsValidSource(s string) bool {
	switch s {
	case SourceSeed, SourceDiscovery, SourceOpenRouter, SourceUser:
		return true
	}
	return false
}

// Ensure the unused import note doesn't fire; time is used by
// other files in this package (Model.DiscoveredAt, etc.).
var _ = time.Time{}
