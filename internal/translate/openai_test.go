package translate_test

import (
	"testing"

	"sage-router/internal/translate"
	openaiTranslate "sage-router/internal/translate/openai"
)

func TestOpenAI_ToCanonical(t *testing.T) {
	tr := openaiTranslate.New()
	opts := translate.TranslateOpts{Provider: "openai"}

	tests := []string{
		"simple_text",
		"system_message",
		"image",
		"tool_calls",
		"tool_result",
		"multi_turn",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			goldenRequestTest(t, tr, "openai", name, opts)
		})
	}
}

func TestOpenAI_StreamChunkToCanonical(t *testing.T) {
	tr := openaiTranslate.New()

	tests := []string{
		"text_delta",
		"tool_calls",
		"reasoning",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			goldenStreamTest(t, tr, "openai", name)
		})
	}
}

func TestOpenAI_FromCanonical(t *testing.T) {
	tr := openaiTranslate.New()
	opts := translate.TranslateOpts{Provider: "openai", Model: "gpt-4.1"}

	tests := []string{
		"simple",
		"tools",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			goldenFromCanonicalTest(t, tr, "openai", name, opts)
		})
	}
}

func TestOpenAI_FromCanonical_Deterministic(t *testing.T) {
	tr := openaiTranslate.New()
	opts := translate.TranslateOpts{Provider: "openai", Model: "gpt-4.1"}
	goldenFromCanonicalDeterminismTest(t, tr, "openai", []string{"simple", "tools"}, opts)
}
