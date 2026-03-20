package translate_test

import (
	"testing"

	"sage-router/internal/translate"
	geminiTranslate "sage-router/internal/translate/gemini"
)

func TestGemini_ToCanonical(t *testing.T) {
	tr := geminiTranslate.New()
	opts := translate.TranslateOpts{Provider: "gemini", Model: "gemini-2.5-flash"}

	tests := []string{
		"simple_text",
		"system_instruction",
		"function_call",
		"function_response",
		"image",
		"generation_config",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			goldenRequestTest(t, tr, "gemini", name, opts)
		})
	}
}

func TestGemini_StreamChunkToCanonical(t *testing.T) {
	tr := geminiTranslate.New()

	tests := []string{
		"text",
		"function_call",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			goldenStreamTest(t, tr, "gemini", name)
		})
	}
}

func TestGemini_FromCanonical(t *testing.T) {
	tr := geminiTranslate.New()
	opts := translate.TranslateOpts{Provider: "gemini", Model: "gemini-2.5-flash"}

	tests := []string{
		"simple",
		"tools",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			goldenFromCanonicalTest(t, tr, "gemini", name, opts)
		})
	}
}
