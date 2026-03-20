package routing

import (
	"testing"
)

var constraintCandidates = []ModelCandidate{
	{Provider: "anthropic", Model: "claude-sonnet-4-6", Tier: 1, InputPrice: 3.00, ContextWindow: 1000000, SupportsImages: true, SupportsTools: true, SupportsThinking: true},
	{Provider: "openai", Model: "gpt-4.1", Tier: 1, InputPrice: 2.00, ContextWindow: 1000000, SupportsImages: true, SupportsTools: true, SupportsThinking: false},
	{Provider: "openai", Model: "gpt-4.1-nano", Tier: 3, InputPrice: 0.05, ContextWindow: 1000000, SupportsImages: false, SupportsTools: true, SupportsThinking: false},
	{Provider: "gemini", Model: "gemini-2.5-flash-lite", Tier: 3, InputPrice: 0.02, ContextWindow: 1048576, SupportsImages: true, SupportsTools: false, SupportsThinking: false},
	{Provider: "openai", Model: "gpt-4o-mini", Tier: 2, InputPrice: 0.15, ContextWindow: 128000, SupportsImages: true, SupportsTools: true, SupportsThinking: false},
}

func TestFilterByConstraints_NoConstraints(t *testing.T) {
	result := FilterByConstraints(constraintCandidates, RequestConstraints{})
	if len(result) != len(constraintCandidates) {
		t.Errorf("no constraints should return all, got %d", len(result))
	}
}

func TestFilterByConstraints_NeedsImages(t *testing.T) {
	result := FilterByConstraints(constraintCandidates, RequestConstraints{NeedsImages: true})
	for _, c := range result {
		if !c.SupportsImages {
			t.Errorf("%s should be filtered (no image support)", c.Model)
		}
	}
	// gpt-4.1-nano has no image support, should be removed
	for _, c := range result {
		if c.Model == "gpt-4.1-nano" {
			t.Error("gpt-4.1-nano should be filtered out (no images)")
		}
	}
}

func TestFilterByConstraints_NeedsTools(t *testing.T) {
	result := FilterByConstraints(constraintCandidates, RequestConstraints{NeedsTools: true})
	for _, c := range result {
		if !c.SupportsTools {
			t.Errorf("%s should be filtered (no tool support)", c.Model)
		}
	}
	// gemini-2.5-flash-lite has no tool support
	for _, c := range result {
		if c.Model == "gemini-2.5-flash-lite" {
			t.Error("gemini-2.5-flash-lite should be filtered out (no tools)")
		}
	}
}

func TestFilterByConstraints_NeedsThinking(t *testing.T) {
	result := FilterByConstraints(constraintCandidates, RequestConstraints{NeedsThinking: true})
	if len(result) != 1 {
		t.Errorf("only claude-sonnet-4-6 supports thinking, got %d results", len(result))
	}
	if len(result) > 0 && result[0].Model != "claude-sonnet-4-6" {
		t.Errorf("expected claude-sonnet-4-6, got %s", result[0].Model)
	}
}

func TestFilterByConstraints_NeedsLongContext(t *testing.T) {
	result := FilterByConstraints(constraintCandidates, RequestConstraints{NeedsLongCtx: true, EstTokens: 150000})
	// gpt-4o-mini has 128K context — should be filtered
	for _, c := range result {
		if c.Model == "gpt-4o-mini" {
			t.Error("gpt-4o-mini should be filtered (128K context < 200K threshold)")
		}
	}
}

func TestFilterByConstraints_MultipleConstraints(t *testing.T) {
	result := FilterByConstraints(constraintCandidates, RequestConstraints{
		NeedsImages: true,
		NeedsTools:  true,
	})
	// gpt-4.1-nano (no images) and gemini-flash-lite (no tools) should be filtered
	for _, c := range result {
		if c.Model == "gpt-4.1-nano" || c.Model == "gemini-2.5-flash-lite" {
			t.Errorf("%s should be filtered", c.Model)
		}
	}
}

func TestFilterByConstraints_AllFilteredFallback(t *testing.T) {
	// If all candidates are filtered, return originals as fallback
	candidates := []ModelCandidate{
		{Model: "basic", SupportsThinking: false},
	}
	result := FilterByConstraints(candidates, RequestConstraints{NeedsThinking: true})
	if len(result) != 1 {
		t.Error("should fallback to original list when all filtered")
	}
}

func TestDetectConstraints(t *testing.T) {
	c := DetectConstraints(true, false, true, 200000)
	if !c.NeedsImages {
		t.Error("NeedsImages should be true")
	}
	if c.NeedsTools {
		t.Error("NeedsTools should be false")
	}
	if !c.NeedsThinking {
		t.Error("NeedsThinking should be true")
	}
	if !c.NeedsLongCtx {
		t.Error("NeedsLongCtx should be true for 200K tokens")
	}
}

func TestRouteWithConstraints(t *testing.T) {
	r := NewSmartRouter()

	// Route with thinking constraint — only claude should survive
	result := r.RouteWithConstraints(StrategyBest, "", constraintCandidates, RequestConstraints{NeedsThinking: true})
	if len(result) != 1 || result[0].Model != "claude-sonnet-4-6" {
		t.Errorf("expected only claude-sonnet-4-6, got %v", result)
	}
}
