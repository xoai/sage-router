package routing

import (
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
