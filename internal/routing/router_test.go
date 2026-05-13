package routing

import (
	"sort"
	"testing"
)

var testCandidates = []ModelCandidate{
	{Provider: "anthropic", Model: "claude-sonnet-4-6", Tier: 1, InputPrice: 3.00},
	{Provider: "openai", Model: "gpt-4o", Tier: 1, InputPrice: 2.50},
	{Provider: "anthropic", Model: "claude-haiku-4-5-20251001", Tier: 2, InputPrice: 1.00},
	{Provider: "openai", Model: "gpt-4o-mini", Tier: 2, InputPrice: 0.15},
	{Provider: "gemini", Model: "gemini-2.5-flash", Tier: 2, InputPrice: 0.15},
	{Provider: "openai", Model: "gpt-4.1-nano", Tier: 3, InputPrice: 0.05},
	{Provider: "gemini", Model: "gemini-2.5-flash-lite", Tier: 3, InputPrice: 0.02},
}

func TestParseAutoModel(t *testing.T) {
	tests := []struct {
		input    string
		strategy Strategy
		isAuto   bool
	}{
		{"auto", StrategyBalanced, true},
		{"auto:fast", StrategyFast, true},
		{"auto:cheap", StrategyCheap, true},
		{"auto:best", StrategyBest, true},
		{"auto:balanced", StrategyBalanced, true},
		{"auto:unknown", StrategyBalanced, true},
		{"gpt-4o", "", false},
		{"anthropic/claude-sonnet-4-6", "", false},
	}

	for _, tt := range tests {
		s, ok := ParseAutoModel(tt.input)
		if ok != tt.isAuto {
			t.Errorf("ParseAutoModel(%q) isAuto = %v, want %v", tt.input, ok, tt.isAuto)
		}
		if ok && s != tt.strategy {
			t.Errorf("ParseAutoModel(%q) strategy = %q, want %q", tt.input, s, tt.strategy)
		}
	}
}

func TestRoute_StrategyCheap(t *testing.T) {
	r := NewSmartRouter()
	result := r.Route(StrategyCheap, "", testCandidates)

	if len(result) != len(testCandidates) {
		t.Fatalf("got %d results, want %d", len(result), len(testCandidates))
	}

	// First should be cheapest
	if result[0].InputPrice != 0.02 {
		t.Errorf("cheapest first: got %s ($%.2f), want gemini-2.5-flash-lite ($0.02)",
			result[0].Model, result[0].InputPrice)
	}
}

// AC24 / Task 3.4: subscription-served candidates win the cheap strategy
// regardless of catalog price. A $3.00 model with a subscription
// connection ranks above a $0.02 model that's apikey-only.
func TestRoute_StrategyCheap_SubscriptionPreferred(t *testing.T) {
	r := NewSmartRouter()

	// Same candidate list as testCandidates, but flag the $3.00 anthropic
	// model as subscription-served.
	withSub := []ModelCandidate{
		{Provider: "anthropic", Model: "claude-sonnet-4-6", Tier: 1, InputPrice: 3.00, HasSubscriptionConnection: true},
		{Provider: "openai", Model: "gpt-4o-mini", Tier: 2, InputPrice: 0.15},
		{Provider: "gemini", Model: "gemini-2.5-flash-lite", Tier: 3, InputPrice: 0.02},
	}
	result := r.Route(StrategyCheap, "", withSub)
	if len(result) != 3 {
		t.Fatalf("got %d, want 3", len(result))
	}
	if result[0].Model != "claude-sonnet-4-6" {
		t.Errorf("subscription-served candidate should be first; got %q (price $%.2f)",
			result[0].Model, result[0].InputPrice)
	}
}

func TestRoute_StrategyCheap_AllSubscriptionFallsBackToPrice(t *testing.T) {
	// When ALL candidates are subscription-served, the tiebreaker reverts
	// to per-model catalog price (cheapest still wins).
	r := NewSmartRouter()
	allSub := []ModelCandidate{
		{Provider: "anthropic", Model: "claude-opus-4", InputPrice: 15.00, HasSubscriptionConnection: true},
		{Provider: "anthropic", Model: "claude-haiku-4", InputPrice: 1.00, HasSubscriptionConnection: true},
	}
	result := r.Route(StrategyCheap, "", allSub)
	if result[0].Model != "claude-haiku-4" {
		t.Errorf("with all-subscription, cheapest catalog price should win; got %q", result[0].Model)
	}
}

func TestRoute_StrategyBest_IgnoresSubscriptionFlag(t *testing.T) {
	// The HasSubscriptionConnection field only affects StrategyCheap;
	// other strategies sort by tier/price as before.
	r := NewSmartRouter()
	candidates := []ModelCandidate{
		{Provider: "openai", Model: "gpt-4o-mini", Tier: 2, InputPrice: 0.15, HasSubscriptionConnection: true},
		{Provider: "openai", Model: "gpt-5", Tier: 1, InputPrice: 5.00, HasSubscriptionConnection: false},
	}
	result := r.Route(StrategyBest, "", candidates)
	// Best should pick the higher-tier (lower Tier number) model.
	if result[0].Model != "gpt-5" {
		t.Errorf("StrategyBest should ignore subscription flag; got %q", result[0].Model)
	}
}

func TestRoute_StrategyBest(t *testing.T) {
	r := NewSmartRouter()
	result := r.Route(StrategyBest, "", testCandidates)

	// First should be Tier 1 (frontier)
	if result[0].Tier != 1 {
		t.Errorf("best first: got tier %d (%s), want tier 1", result[0].Tier, result[0].Model)
	}
	// Among tier 1, cheapest first (gpt-4o at $2.50)
	if result[0].InputPrice != 2.50 {
		t.Errorf("best first (cheapest tier-1): got %s ($%.2f), want gpt-4o ($2.50)",
			result[0].Model, result[0].InputPrice)
	}
}

func TestRoute_StrategyFast(t *testing.T) {
	r := NewSmartRouter()
	result := r.Route(StrategyFast, "", testCandidates)

	// Fast strategy: higher tier number first (cheaper/faster models)
	if result[0].Tier != 3 {
		t.Errorf("fast first: got tier %d (%s), want tier 3", result[0].Tier, result[0].Model)
	}
}

func TestRoute_SessionAffinity(t *testing.T) {
	r := NewSmartRouter()

	// Set affinity
	r.Affinity.Set("hello world", "anthropic", "claude-sonnet-4-6")

	result := r.Route(StrategyCheap, "hello world", testCandidates)

	// Affinity model should be first regardless of strategy
	if result[0].Provider != "anthropic" || result[0].Model != "claude-sonnet-4-6" {
		t.Errorf("affinity hit: got %s/%s, want anthropic/claude-sonnet-4-6",
			result[0].Provider, result[0].Model)
	}
}

func TestRoute_AffinityFallbackOrder(t *testing.T) {
	r := NewSmartRouter()
	r.Affinity.Set("hello", "anthropic", "claude-sonnet-4-6")

	result := r.Route(StrategyBalanced, "hello", testCandidates)

	// First: exact match
	if result[0].Model != "claude-sonnet-4-6" {
		t.Errorf("first should be affinity match, got %s", result[0].Model)
	}
	// Second: same family (claude-haiku is same "claude" family)
	if result[1].Provider != "anthropic" {
		t.Errorf("second should be same vendor, got %s/%s", result[1].Provider, result[1].Model)
	}
}

func TestRoute_NoAffinity(t *testing.T) {
	r := NewSmartRouter()
	result := r.Route(StrategyBalanced, "new conversation", testCandidates)

	// No affinity → sorted by strategy (balanced = tier asc, price asc)
	if result[0].Tier != 1 {
		t.Errorf("balanced first: got tier %d, want tier 1", result[0].Tier)
	}
}

func TestRoute_EmptyCandidates(t *testing.T) {
	r := NewSmartRouter()
	result := r.Route(StrategyBalanced, "hello", nil)
	if result != nil {
		t.Error("expected nil for empty candidates")
	}
}

func TestModelFamily(t *testing.T) {
	tests := []struct {
		a, b   string
		expect bool
	}{
		{"claude-sonnet-4-6", "claude-haiku-4-5-20251001", true},
		{"gpt-4o", "gpt-4o-mini", true},
		{"gpt-4.1", "gpt-4.1-nano", true},
		{"o3", "o4-mini", true},
		{"claude-sonnet-4-6", "gpt-4o", false},
		{"gemini-2.5-flash", "gemini-2.5-pro", true},
	}

	for _, tt := range tests {
		got := sameModelFamily(tt.a, tt.b)
		if got != tt.expect {
			t.Errorf("sameModelFamily(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.expect)
		}
	}
}

// Models Discovery M3.3 — cache-aware cheap strategy (AC26 + AC26b).
//
// StrategyCheap now sorts by `effectivePrice` instead of InputPrice:
//   effectivePrice = Input * (1 - CachedRatio) + CacheRead * CachedRatio
//
// CachedRatio ∈ [0, 1] is the per-connection cache-hit-rate over the
// 24h lookback window (see Store.GetCacheHitRate, populated by
// buildSmartCandidates in M3.4b). When CachedRatio=0 the formula
// degenerates to Input, matching today's pure-InputPrice ranking
// byte-for-byte. AC26b pins that behaviour.

// TestSmartRoute_Cheap_UsesEffectivePrice — AC26. Two candidates
// chosen so the ranking flips with CachedRatio:
//
//   A: Input=10.0, CacheRead=0.0   (expensive input, free cache reads)
//   B: Input=3.0,  CacheRead=3.0   (cheap input, no cache discount)
//
// At CachedRatio=0.8 (heavy cache use):
//   A's effective = 10.0*0.2 + 0.0*0.8 = 2.0
//   B's effective = 3.0*0.2  + 3.0*0.8 = 3.0
//   → A wins.
//
// At CachedRatio=0.0 (no cache benefit):
//   A's effective = 10.0
//   B's effective = 3.0
//   → B wins.
//
// The flip is the AC26 contract — sort order must change with cache
// ratio when cache pricing varies between candidates.
func TestSmartRoute_Cheap_UsesEffectivePrice(t *testing.T) {
	r := NewSmartRouter()

	heavyCacheCandidates := []ModelCandidate{
		// A: expensive input, free cache reads — wins under heavy cache use.
		{Provider: "p1", Model: "high-input-free-cache",
			Tier: 1, InputPrice: 10.0, CacheReadPrice: 0.0, CachedRatio: 0.8},
		// B: cheap input, no cache discount — loses under heavy cache use.
		{Provider: "p2", Model: "flat-price",
			Tier: 1, InputPrice: 3.0, CacheReadPrice: 3.0, CachedRatio: 0.8},
	}
	result := r.Route(StrategyCheap, "", heavyCacheCandidates)
	if len(result) != 2 {
		t.Fatalf("got %d, want 2", len(result))
	}
	if result[0].Model != "high-input-free-cache" {
		t.Errorf("under CachedRatio=0.8, free-cache-read candidate should rank first; got %q (effectivePrice=A:2.0 B:3.0)",
			result[0].Model)
	}

	// Same candidates, but with CachedRatio=0 — cache benefit disappears,
	// pure-InputPrice ranking should restore. Now B (cheaper input) wins.
	noCacheCandidates := []ModelCandidate{
		{Provider: "p1", Model: "high-input-free-cache",
			Tier: 1, InputPrice: 10.0, CacheReadPrice: 0.0, CachedRatio: 0.0},
		{Provider: "p2", Model: "flat-price",
			Tier: 1, InputPrice: 3.0, CacheReadPrice: 3.0, CachedRatio: 0.0},
	}
	result = r.Route(StrategyCheap, "", noCacheCandidates)
	if result[0].Model != "flat-price" {
		t.Errorf("under CachedRatio=0, cheaper-input candidate should rank first; got %q (effectivePrice=A:10.0 B:3.0)",
			result[0].Model)
	}
}

// TestSmartRoute_Cheap_CachedRatioZero_MatchesTodaysBehavior — AC26b.
// With all CachedRatio=0 (the default for connections without enough
// usage history yet, or for tests that pre-date M3.4b's enrichment),
// the cheap ranking must be byte-equal to the pre-M3.3 pure-InputPrice
// sort. This locks the no-regression contract: existing routing tests
// continue to pass without modification.
func TestSmartRoute_Cheap_CachedRatioZero_MatchesTodaysBehavior(t *testing.T) {
	r := NewSmartRouter()
	// testCandidates (package-level) has CachedRatio=0 by default —
	// route them through StrategyCheap and assert the order matches
	// the pre-M3.3 InputPrice ascending sort.
	result := r.Route(StrategyCheap, "", testCandidates)

	// Derive the expected order programmatically from testCandidates'
	// own InputPrice values (carryover #49). The previous version
	// hardcoded a 7-element slice that tracked testCandidates'
	// declaration order; any future add/remove/reorder there would
	// fail this test as a positional mismatch, not as a real
	// regression of the AC26b "CachedRatio=0 ranking matches pre-M3.3"
	// contract. sort.SliceStable on InputPrice mirrors the SmartRouter's
	// own stable-sort behavior, so tied prices preserve declaration
	// order in both expected and result (gpt-4o-mini before
	// gemini-2.5-flash at $0.15).
	expected := make([]ModelCandidate, len(testCandidates))
	copy(expected, testCandidates)
	sort.SliceStable(expected, func(i, j int) bool {
		return expected[i].InputPrice < expected[j].InputPrice
	})

	if len(result) != len(expected) {
		t.Fatalf("len = %d, want %d", len(result), len(expected))
	}
	for i, exp := range expected {
		if result[i].Model != exp.Model {
			t.Errorf("position %d: got %q ($%.2f), want %q ($%.2f) "+
				"(AC26b: CachedRatio=0 ranking must match InputPrice ascending)",
				i, result[i].Model, result[i].InputPrice,
				exp.Model, exp.InputPrice)
		}
	}
}
