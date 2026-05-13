package executor

import "testing"

// TestGemini_OverrideCapabilities — Models Discovery M3.5.
// GeminiExecutor satisfies CapabilityOverrider but with identity
// semantics: no current model needs an override beyond what the
// catalog asserts. The interface is wired now so a future patch
// (e.g., gemini-2.5-flash-lite v2) can flip a flag without
// rewiring callers.
func TestGemini_OverrideCapabilities(t *testing.T) {
	e := NewGeminiExecutor("", nil)

	// Compile-time + runtime interface check.
	var _ CapabilityOverrider = e

	cases := []struct {
		name  string
		model string
		base  Capabilities
	}{
		{"flash 2.5", "gemini-2.5-flash", Capabilities{SupportsImages: true, SupportsTools: true}},
		{"pro 2.5", "gemini-2.5-pro", Capabilities{SupportsImages: true, SupportsTools: true, SupportsThinking: true}},
		{"unknown", "gemini-future-model", Capabilities{}},
		{"empty model", "", Capabilities{SupportsThinking: true}},
		{"all false", "gemini-2.0-flash", Capabilities{}},
		{"all true", "gemini-2.5-pro", Capabilities{SupportsImages: true, SupportsTools: true, SupportsThinking: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.OverrideCapabilities(c.model, c.base)
			if got != c.base {
				t.Errorf("identity violated: got %+v, want %+v", got, c.base)
			}
		})
	}
}
