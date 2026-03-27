package cost

import (
	"strings"
	"testing"

	"sage-router/pkg/canonical"
)

func TestInjectCacheHints_ClaudeSmallSystem(t *testing.T) {
	req := &canonical.Request{
		System: []canonical.SystemBlock{
			{Text: "You are helpful."},
		},
	}
	injected := InjectCacheHints(req, "anthropic")
	if injected {
		t.Error("should not inject on small system prompt")
	}
	if req.System[0].CacheControl != nil {
		t.Error("cache_control should be nil")
	}
}

func TestInjectCacheHints_ClaudeLargeSystem(t *testing.T) {
	// Create a system prompt > 1024 tokens (~4096+ chars)
	largeText := strings.Repeat("This is a detailed instruction for the assistant. ", 100)
	req := &canonical.Request{
		System: []canonical.SystemBlock{
			{Text: largeText},
		},
	}

	injected := InjectCacheHints(req, "anthropic")
	if !injected {
		t.Error("should inject on large system prompt")
	}
	if req.System[0].CacheControl == nil {
		t.Fatal("cache_control should be set")
	}
	if req.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("expected ephemeral, got %s", req.System[0].CacheControl.Type)
	}
}

func TestInjectCacheHints_ClaudeMultiBlockLastOnly(t *testing.T) {
	largeText := strings.Repeat("Detailed context block. ", 200)
	req := &canonical.Request{
		System: []canonical.SystemBlock{
			{Text: largeText},
			{Text: "Final instructions."},
		},
	}

	injected := InjectCacheHints(req, "anthropic")
	if !injected {
		t.Error("should inject")
	}
	if req.System[0].CacheControl != nil {
		t.Error("first block should NOT have cache_control")
	}
	if req.System[1].CacheControl == nil {
		t.Fatal("last block should have cache_control")
	}
}

func TestInjectCacheHints_RespectsUserConfig(t *testing.T) {
	largeText := strings.Repeat("Large system prompt. ", 200)
	req := &canonical.Request{
		System: []canonical.SystemBlock{
			{Text: largeText, CacheControl: &canonical.CacheControl{Type: "ephemeral"}},
		},
	}

	injected := InjectCacheHints(req, "anthropic")
	if injected {
		t.Error("should not inject when user already set cache_control")
	}
}

func TestInjectCacheHints_NonAnthropicProvider(t *testing.T) {
	largeText := strings.Repeat("Large system prompt. ", 200)
	req := &canonical.Request{
		System: []canonical.SystemBlock{
			{Text: largeText},
		},
	}

	injected := InjectCacheHints(req, "openai")
	if injected {
		t.Error("should not inject for non-Anthropic provider")
	}
}

func TestInjectCacheHints_NoSystem(t *testing.T) {
	req := &canonical.Request{
		Messages: []canonical.Message{{Role: "user"}},
	}
	injected := InjectCacheHints(req, "anthropic")
	if injected {
		t.Error("should not inject when no system blocks")
	}
}

func TestInjectCacheHints_NilRequest(t *testing.T) {
	injected := InjectCacheHints(nil, "anthropic")
	if injected {
		t.Error("should handle nil request")
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"", 0},
		{"hi", 1},
		{"hello world", 3},
		{strings.Repeat("a", 4000), 1000},
	}
	for _, tt := range tests {
		got := estimateTokens(tt.input)
		if got != tt.expected {
			t.Errorf("estimateTokens(%q) = %d, want %d", tt.input[:min(len(tt.input), 20)], got, tt.expected)
		}
	}
}

func TestInjectCacheHints_SkipsWhenContentHasCacheControl(t *testing.T) {
	largeText := strings.Repeat("Large system prompt. ", 200)
	req := &canonical.Request{
		System: []canonical.SystemBlock{
			{Text: largeText},
		},
		Messages: []canonical.Message{
			{
				Role: "user",
				Content: []canonical.Content{
					{
						Type:         "text",
						Text:         "hello",
						CacheControl: &canonical.CacheControl{Type: "ephemeral"},
					},
				},
			},
		},
	}

	injected := InjectCacheHints(req, "anthropic")
	if injected {
		t.Error("should not inject when content blocks have cache_control")
	}
	// Verify system block was NOT modified
	if req.System[0].CacheControl != nil {
		t.Error("system block should not have cache_control when skipping")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
