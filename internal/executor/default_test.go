package executor

import "testing"

// TestDefault_OverrideCapabilities — Models Discovery M3.5.
// DefaultExecutor is the OpenAI-compat path shared by openai /
// openrouter / github-copilot (when routed via OpenAI-compat) /
// ollama / custom endpoints. Identity body for the M3 baseline —
// per plan §3.5 the interface is wired so a future patch (e.g.,
// GPT-5 capability inheritance) doesn't need to rewire callers.
func TestDefault_OverrideCapabilities(t *testing.T) {
	e := NewDefaultExecutor("openai", "", nil)

	// Compile-time + runtime interface check.
	var _ CapabilityOverrider = e

	cases := []struct {
		name  string
		model string
		base  Capabilities
	}{
		{"gpt-4o", "gpt-4o", Capabilities{SupportsImages: true, SupportsTools: true}},
		{"gpt-4.1", "gpt-4.1", Capabilities{SupportsImages: true, SupportsTools: true}},
		{"o3", "o3", Capabilities{SupportsTools: true, SupportsThinking: true}},
		{"future gpt-5", "gpt-5", Capabilities{SupportsImages: true, SupportsTools: true, SupportsThinking: true}},
		{"empty model", "", Capabilities{}},
		{"all false", "gpt-3.5-turbo", Capabilities{}},
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
