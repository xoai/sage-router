package executor

import "testing"

// TestGitHubCopilot_OverrideCapabilities — Models Discovery M3.5.
// Identity body for M3 baseline. github-copilot has its own
// dedicated executor (separate from DefaultExecutor) and must
// also satisfy the optional CapabilityOverrider so the smart-
// router's type assertion never silently skips for copilot-routed
// models. Identity is the correct M3 baseline — Copilot's model
// catalog is already exhaustive per provider seed.
func TestGitHubCopilot_OverrideCapabilities(t *testing.T) {
	e := NewGitHubCopilotExecutor("", nil)

	// Compile-time + runtime interface check.
	var _ CapabilityOverrider = e

	cases := []struct {
		name  string
		model string
		base  Capabilities
	}{
		{"gpt-4o via copilot", "gpt-4o", Capabilities{SupportsImages: true, SupportsTools: true}},
		{"o3-mini via copilot", "o3-mini", Capabilities{SupportsTools: true, SupportsThinking: true}},
		{"claude-3-5-sonnet via copilot", "claude-3-5-sonnet", Capabilities{SupportsImages: true, SupportsTools: true}},
		{"empty model", "", Capabilities{}},
		{"all false", "future-model", Capabilities{}},
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
