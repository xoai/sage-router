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
			return a.InputPrice < b.InputPrice
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
