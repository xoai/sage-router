// Package catalog owns the runtime catalog of models, pricing, and
// per-provider discovery state. It replaces the read path of
// internal/config/{models,pricing,providers}.go, which now lives on
// only as the embedded seed source.
//
// Three SQLite tables back the catalog (migrations 009/010/011):
//   - catalog_models — capability metadata per (provider, model_id)
//   - catalog_pricing — prices per (provider, model_id), FK→models
//   - catalog_provider_meta — per-provider discovery state
//
// See .sage/docs/decision-models-{catalog-storage,discovery-loop,
// pricing-resolution}.md for design rationale.
package catalog

import "time"

// Source identifies where a catalog row's data came from. The four
// values are an enum enforced by CHECK constraints in the catalog
// migrations; mismatched strings will be rejected at insert time.
//
// Write-side precedence (see ADR-2 §Conflict resolution):
//
//	user > openrouter > discovery > seed
//
// for pricing (with the extra rule that discovery may NOT clobber
// openrouter pricing — capability columns only); for capability
// rows, user > discovery > seed.
const (
	SourceSeed       = "seed"
	SourceDiscovery  = "discovery"
	SourceOpenRouter = "openrouter"
	SourceUser       = "user"
)

// ValidSources returns the canonical catalog source enum values in
// precedence order (lowest to highest). Exposed so tests can iterate
// the canonical set from one place — TestPrecedenceMatrix_SourcesAreCanonical
// asserts the precedence matrix covers every value returned here, so a
// future fifth source added without matrix coverage fails loudly.
//
// IsValidSource MUST agree with ValidSources for any string s:
// `IsValidSource(s) == slices.Contains(ValidSources(), s)`. The drift
// is pinned by TestValidSources_AgreesWithIsValidSource.
//
// Returns a freshly-allocated slice; callers may mutate it.
func ValidSources() []string {
	return []string{SourceSeed, SourceDiscovery, SourceOpenRouter, SourceUser}
}

// Capability tier constants — kept in sync with config.Tier* so the
// seed transfer is a straight copy. Lower = more capable.
const (
	TierFrontier  = 1
	TierStrong    = 2
	TierEfficient = 3
	TierFree      = 4
)

// Model is one row from catalog_models. Identified by (Provider,
// ModelID) — the bare model ID (e.g. "claude-sonnet-4-6", not
// "anthropic/claude-sonnet-4-6"). OpenRouter rows DO use qualified
// IDs in ModelID by design; the provider is still "openrouter".
type Model struct {
	Provider      string
	ModelID       string
	DisplayName   string
	Tier          int
	ContextWindow int
	MaxOutput     int
	Caps          Capabilities
	Pricing       Pricing
	Source        string // matches one of the Source* constants
	DiscoveredAt  time.Time
	UpdatedAt     time.Time
}

// Capabilities are the binary flags used by smart-routing constraint
// filtering. Default (DB row) values can be overridden per-request by
// an executor implementing executor.CapabilityOverrider — see ADR-3
// §Part D.
type Capabilities struct {
	SupportsImages   bool
	SupportsTools    bool
	SupportsThinking bool
}

// Pricing is one row from catalog_pricing. Prices are USD per
// 1,000,000 tokens. CacheWrite=0 means "free at write" by convention
// (e.g., OpenAI), not "unknown — derive from Input." Anthropic-family
// seeds explicitly write CacheWrite = Input * 1.25. There is no
// cross-provider default — see ADR-3 §Part B for the per-provider
// table and the rationale for killing the 1.25× cross-provider
// default that the r1 design used.
type Pricing struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
	Thinking   float64
	Source     string
}

// TokenBreakdown describes a single request's token usage across all
// price tiers. CacheReadIn + Input together equal the upstream's
// total input-side tokens; CacheWriteIn is separate (cache-creation,
// billed at CacheWrite). Thinking tokens (Anthropic / OpenAI o-series)
// bill at Pricing.Thinking when set, otherwise fall back to Output.
type TokenBreakdown struct {
	Input        int
	Output       int
	CacheReadIn  int
	CacheWriteIn int
	Thinking     int
}

// EstimateCost computes the request's cost in USD given a Pricing
// row. The math matches today's config.EstimateCost for the common
// case (CacheReadIn=CacheWriteIn=Thinking=0) — verified by
// TestEstimateCost_MatchesStaticConfigForSeed.
//
// CacheWrite is used verbatim. CacheRead is used verbatim. Thinking
// falls back to Output when unset (0). All other zero fields
// contribute zero cost.
func (p Pricing) EstimateCost(br TokenBreakdown) float64 {
	thinkingPrice := p.Thinking
	if thinkingPrice == 0 {
		thinkingPrice = p.Output
	}
	cost := 0.0
	cost += float64(br.Input) * p.Input / 1_000_000
	cost += float64(br.Output) * p.Output / 1_000_000
	cost += float64(br.CacheReadIn) * p.CacheRead / 1_000_000
	cost += float64(br.CacheWriteIn) * p.CacheWrite / 1_000_000
	cost += float64(br.Thinking) * thinkingPrice / 1_000_000
	return cost
}

// ProviderMeta is one row from catalog_provider_meta — per-provider
// discovery-loop state. Static provider definitions (Name, Format,
// BaseURL, AuthTypes) live in config.KnownProviders and are NOT
// represented here — see ADR-1 §"Field source" table and the
// holistic /review RC1 resolution.
type ProviderMeta struct {
	Provider                 string
	DiscoveryEnabled         bool
	SubscriptionDiscoverable bool
	LastDiscoveredAt         time.Time
	LastDiscoveryError       string
	BackoffStep              int       // 0..5, maps to [1h, 2h, 4h, 8h, 24h] in refresh.go
	NextDiscoveryAfter       time.Time // skip discovery if now.Before(NextDiscoveryAfter)
}

// PricingUpdate is a partial update for catalog_pricing, used by
// BulkUpsertPricing (e.g., from the OpenRouter refresher).
type PricingUpdate struct {
	Provider string
	ModelID  string
	Pricing  Pricing // Source must be set
}
