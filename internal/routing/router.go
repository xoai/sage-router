package routing

import (
	"sort"
	"strings"
)

// Strategy defines the routing optimization objective.
type Strategy string

const (
	StrategyBalanced Strategy = "balanced"
	StrategyFast     Strategy = "fast"
	StrategyCheap    Strategy = "cheap"
	StrategyBest     Strategy = "best"
)

// ModelCandidate represents a model available for routing.
type ModelCandidate struct {
	Provider         string
	Model            string
	Tier             int
	InputPrice       float64
	ContextWindow    int
	SupportsImages   bool
	SupportsTools    bool
	SupportsThinking bool

	// HasSubscriptionConnection is true when at least one connection
	// servicing this (Provider, Model) pair has AuthType="subscription"
	// AND the subscription tier permits this model. Populated by the
	// caller (server.buildSmartCandidates) at route time so the router
	// doesn't depend on the connection layer.
	//
	// Used by StrategyCheap to rank zero-marginal-cost candidates first.
	// Other strategies ignore this field.
	HasSubscriptionConnection bool

	// CacheReadPrice (USD/1M tokens) and CachedRatio (0..1) are
	// additive fields added in M1 of Models Discovery. M1 itself
	// leaves them at zero — the cheap-strategy rewrite that consumes
	// them lands in M3.3 (effectivePrice = Input*(1-r) + CacheRead*r).
	// Zero values produce identical sort keys to today's pure-Input
	// sort, so existing routing tests pass unchanged (AC10).
	CacheReadPrice float64
	CachedRatio    float64
}

// SmartRouter selects models based on strategy and session affinity.
type SmartRouter struct {
	Affinity *SessionCache
}

// NewSmartRouter creates a router with a fresh session cache.
func NewSmartRouter() *SmartRouter {
	return &SmartRouter{
		Affinity: NewSessionCache(),
	}
}

// ParseAutoModel parses "auto" or "auto:strategy" into a Strategy.
// Returns the strategy and true if the model is an auto-route request.
func ParseAutoModel(model string) (Strategy, bool) {
	if model == "auto" {
		return StrategyBalanced, true
	}
	if strings.HasPrefix(model, "auto:") {
		s := Strategy(strings.TrimPrefix(model, "auto:"))
		switch s {
		case StrategyFast, StrategyCheap, StrategyBest, StrategyBalanced:
			return s, true
		}
		// Unknown strategy, default to balanced
		return StrategyBalanced, true
	}
	return "", false
}

// Route returns an ordered list of model candidates based on strategy,
// session affinity, and hard constraints. The caller iterates through
// the list as a fallback chain (same as combo).
func (r *SmartRouter) Route(strategy Strategy, firstMsg string, available []ModelCandidate) []ModelCandidate {
	return r.RouteWithConstraints(strategy, firstMsg, available, RequestConstraints{})
}

// RouteWithConstraints routes with hard constraint filtering (Layer 2).
func (r *SmartRouter) RouteWithConstraints(strategy Strategy, firstMsg string, available []ModelCandidate, constraints RequestConstraints) []ModelCandidate {
	if len(available) == 0 {
		return nil
	}

	// 1. Filter by hard constraints (Layer 2)
	candidates := FilterByConstraints(available, constraints)

	// 2. Check session affinity
	if r.Affinity != nil && firstMsg != "" {
		if entry := r.Affinity.Get(firstMsg); entry != nil {
			return r.buildAffinityList(entry, candidates)
		}
	}

	// 3. Sort by strategy
	return sortByStrategy(strategy, candidates)
}

// buildAffinityList puts the affinity model first, then fallbacks
// ordered by: same family → same vendor → other vendors.
func (r *SmartRouter) buildAffinityList(entry *SessionEntry, available []ModelCandidate) []ModelCandidate {
	var result []ModelCandidate
	var sameFamily, sameVendor, others []ModelCandidate

	for _, c := range available {
		if c.Provider == entry.Provider && c.Model == entry.Model {
			result = append(result, c) // exact match first
			continue
		}
		if c.Provider == entry.Provider && sameModelFamily(c.Model, entry.Model) {
			sameFamily = append(sameFamily, c)
		} else if c.Provider == entry.Provider {
			sameVendor = append(sameVendor, c)
		} else {
			others = append(others, c)
		}
	}

	result = append(result, sameFamily...)
	result = append(result, sameVendor...)
	result = append(result, others...)
	return result
}

// sameModelFamily checks if two models are in the same family.
// E.g., "claude-sonnet-4-6" and "claude-haiku-4-5" are both Claude.
// "gpt-4o" and "gpt-4o-mini" are both GPT-4o.
func sameModelFamily(a, b string) bool {
	fa := modelFamily(a)
	fb := modelFamily(b)
	return fa != "" && fa == fb
}

func modelFamily(model string) string {
	switch {
	case strings.HasPrefix(model, "claude-sonnet"), strings.HasPrefix(model, "claude-haiku"),
		strings.HasPrefix(model, "claude-opus"):
		return "claude"
	case strings.HasPrefix(model, "gpt-4o"):
		return "gpt-4o"
	case strings.HasPrefix(model, "gpt-4.1"):
		return "gpt-4.1"
	case strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
		return "o-series"
	case strings.HasPrefix(model, "gemini-2.5"):
		return "gemini-2.5"
	case strings.HasPrefix(model, "gemini-2.0"):
		return "gemini-2.0"
	default:
		return ""
	}
}

// effectivePrice (Models Discovery M3.3) returns the cache-aware
// per-1M-token cost for the cheap strategy:
//
//	effectivePrice = Input * (1 - CachedRatio) + CacheRead * CachedRatio
//
// CachedRatio is the per-connection 24h cache-hit-rate computed at
// route time by `server.buildSmartCandidates` from
// `Store.GetCacheHitRate`. Values outside [0, 1] are clamped — the
// downstream sort would still terminate, but a negative ratio could
// invert the cheap ordering relative to "lower wins", and a > 1 ratio
// could produce nonsense like a CacheRead-dominated sort that ignores
// non-cached tokens. NaN is also clamped to 0 (NaN-self-inequality
// guard — `r != r` is true only for NaN). Clamping is defensive
// against future enrichment bugs (e.g., a NULL-coalesce returning 1.5
// or a divide-by-zero leaking NaN).
//
// When CachedRatio=0 (the default for connections with no cache
// history yet, or for strategies that don't populate it), the formula
// degenerates to InputPrice — locking AC26b's no-regression contract.
//
// Ties on effectivePrice fall back to `sort.SliceStable`'s preserved
// declaration order (which itself comes from the caller's iteration —
// for the smart router, that's `buildSmartCandidates`'s deterministic
// connection sort in M3.4a).
func effectivePrice(c ModelCandidate) float64 {
	r := c.CachedRatio
	switch {
	case r != r: // NaN
		r = 0
	case r < 0:
		r = 0
	case r > 1:
		r = 1
	}
	return c.InputPrice*(1-r) + c.CacheReadPrice*r
}

// sortByStrategy returns a sorted copy of candidates by the given strategy.
func sortByStrategy(strategy Strategy, candidates []ModelCandidate) []ModelCandidate {
	result := make([]ModelCandidate, len(candidates))
	copy(result, candidates)

	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		switch strategy {
		case StrategyFast:
			// Lower tier → faster (proxy for latency without historical data)
			// Within same tier, cheaper models tend to be faster
			if a.Tier != b.Tier {
				return a.Tier > b.Tier // higher tier number = cheaper/faster
			}
			return a.InputPrice < b.InputPrice
		case StrategyCheap:
			// Subscription-served models cost the user $0 at the margin,
			// so they win over any priced model regardless of catalog price.
			// Among same-subscription-status models, fall through to
			// per-model price.
			if a.HasSubscriptionConnection != b.HasSubscriptionConnection {
				return a.HasSubscriptionConnection // true sorts before false
			}
			// Models Discovery M3.3 (AC26): cache-aware effective price.
			// effectivePrice = Input*(1-CachedRatio) + CacheRead*CachedRatio.
			// When the per-connection 24h cache-hit-rate is high, models
			// with cheap cache_read pricing win even if their InputPrice
			// is nominally higher. When CachedRatio=0 (no cache history
			// or cache disabled), the formula degenerates to InputPrice,
			// preserving the pre-M3.3 ranking byte-for-byte (AC26b).
			return effectivePrice(a) < effectivePrice(b)
		case StrategyBest:
			if a.Tier != b.Tier {
				return a.Tier < b.Tier // lower tier = better
			}
			return a.InputPrice < b.InputPrice // break ties by cost
		case StrategyBalanced:
			if a.Tier != b.Tier {
				return a.Tier < b.Tier
			}
			return a.InputPrice < b.InputPrice
		default:
			return a.Tier < b.Tier
		}
	})

	return result
}
