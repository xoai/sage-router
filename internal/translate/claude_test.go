package translate_test

import (
	"testing"

	"sage-router/internal/translate"
	claudeTranslate "sage-router/internal/translate/claude"
)

func TestClaude_ToCanonical(t *testing.T) {
	tr := claudeTranslate.New()
	opts := translate.TranslateOpts{Provider: "anthropic"}

	tests := []string{
		"simple_text",
		"multi_turn",
		"system_prompt",
		"tool_use",
		"tool_result",
		"thinking",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			goldenRequestTest(t, tr, "claude", name, opts)
		})
	}
}

func TestClaude_StreamChunkToCanonical(t *testing.T) {
	tr := claudeTranslate.New()

	tests := []string{
		"text_delta",
		"tool_use",
		"thinking",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			goldenStreamTest(t, tr, "claude", name)
		})
	}
}

func TestClaude_FromCanonical(t *testing.T) {
	tr := claudeTranslate.New()
	opts := translate.TranslateOpts{Provider: "anthropic", Model: "claude-sonnet-4-6"}

	tests := []string{
		"simple",
		"tools",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			goldenFromCanonicalTest(t, tr, "claude", name, opts)
		})
	}
}

func TestClaude_FromCanonical_Deterministic(t *testing.T) {
	tr := claudeTranslate.New()
	opts := translate.TranslateOpts{Provider: "anthropic", Model: "claude-sonnet-4-6"}
	goldenFromCanonicalDeterminismTest(t, tr, "claude", []string{"simple", "tools"}, opts)
}
