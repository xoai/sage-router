package executor

import "testing"

// TestClaude_OverrideCapabilities — Models Discovery M3.5.
// ClaudeExecutor must satisfy CapabilityOverrider (the smart-router's
// `buildSmartCandidates` does an exec.(CapabilityOverrider) assertion).
// Behavior: ADDITIVE — sets SupportsThinking=true for known thinking-
// capable variants and leaves base unchanged otherwise. Never REMOVES
// a capability the catalog asserted (additive-only override semantic).
func TestClaude_OverrideCapabilities(t *testing.T) {
	e := NewClaudeExecutor("", nil)

	// Compile-time + runtime interface check.
	var _ CapabilityOverrider = e

	cases := []struct {
		name            string
		model           string
		base            Capabilities
		wantThinking    bool
		wantImages      bool
		wantTools       bool
		notesBaseChange string
	}{
		// 4.x family — thinking flips on regardless of base.
		{
			name: "sonnet 4.6 from false → true",
			model: "claude-sonnet-4-6",
			base: Capabilities{},
			wantThinking: true,
		},
		{
			name: "sonnet 4 dated from false → true",
			model: "claude-sonnet-4-20250514",
			base: Capabilities{},
			wantThinking: true,
		},
		{
			name: "haiku 4.5 dated from false → true",
			model: "claude-haiku-4-5-20251001",
			base: Capabilities{},
			wantThinking: true,
		},
		{
			name: "opus 4.6 from false → true",
			model: "claude-opus-4-6",
			base: Capabilities{},
			wantThinking: true,
		},
		{
			name: "opus 4 dated from false → true",
			model: "claude-opus-4-20250514",
			base: Capabilities{},
			wantThinking: true,
		},

		// 3.7 family — thinking flips on (3.7 was the first thinking-capable Claude).
		{
			name: "claude-3-7-sonnet from false → true",
			model: "claude-3-7-sonnet-20250219",
			base: Capabilities{},
			wantThinking: true,
		},

		// Hypothetical 5.x — future-proofed.
		{
			name: "future opus 5 flips → true",
			model: "claude-opus-5-0",
			base: Capabilities{},
			wantThinking: true,
		},

		// Pre-thinking variants — pass-through (no flip).
		{
			name: "claude-3-5-sonnet (pre-thinking) — base preserved",
			model: "claude-3-5-sonnet-20241022",
			base: Capabilities{SupportsImages: true, SupportsTools: true, SupportsThinking: false},
			wantThinking: false,
			wantImages: true,
			wantTools: true,
		},
		{
			name: "claude-2-1 (pre-thinking) — base preserved",
			model: "claude-2-1",
			base: Capabilities{SupportsTools: true},
			wantThinking: false,
			wantTools: true,
		},

		// Unknown / non-claude — pass-through.
		{
			name: "unknown model — base preserved",
			model: "claude-unknown-future",
			base: Capabilities{SupportsThinking: true},
			wantThinking: true,
		},
		{
			name: "empty model — base preserved",
			model: "",
			base: Capabilities{SupportsImages: true},
			wantImages: true,
		},

		// Additive contract — already-true thinking stays true.
		{
			name: "thinking already true — preserved (not toggled off)",
			model: "claude-sonnet-4-6",
			base: Capabilities{SupportsThinking: true, SupportsTools: true},
			wantThinking: true,
			wantTools: true,
		},

		// Carryover #52 — additive-only override contract for non-thinking
		// variants. If the catalog over-asserts SupportsThinking=true for
		// a model claudeSupportsThinking would otherwise reject (e.g.,
		// claude-2-1), the override MUST NOT clobber the base value back
		// to false. Pins against a refactor that replaces
		// `if X { base.X = true }` with `base.X = X`.
		{
			name: "non-thinking model with base=true must not be clobbered",
			model: "claude-2-1",
			base: Capabilities{SupportsThinking: true},
			wantThinking: true,
		},

		// Carryover #50 — case sensitivity. claudeSupportsThinking uses
		// byte-exact strings.HasPrefix / strings.CutPrefix; Anthropic
		// IDs are lowercase by documentation, so a capitalized variant
		// should NOT flip thinking on. Theoretical drift only today,
		// but the subtest locks the contract against future refactor.
		{
			name: "case-sensitive: Claude-Sonnet-4-6 does NOT flip thinking",
			model: "Claude-Sonnet-4-6",
			base: Capabilities{},
			wantThinking: false,
		},

		// Carryover #51 — bare-version edges. claude-sonnet-4 (no
		// trailing version suffix) currently flips thinking (the
		// `rest == ""` guard in claudeSupportsThinking treats bare
		// major-version as a valid match). claude-sonnet- (trailing
		// hyphen with empty version) does NOT flip — the empty rest
		// after the hyphen is treated as invalid. Both already handled
		// correctly by the code; these subtests pin the bounds-safety
		// implementation against future refactor.
		{
			name: "bare version: claude-sonnet-4 flips thinking (rest=='' guard)",
			model: "claude-sonnet-4",
			base: Capabilities{},
			wantThinking: true,
		},
		{
			name: "empty version after hyphen: claude-sonnet- does NOT flip",
			model: "claude-sonnet-",
			base: Capabilities{},
			wantThinking: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.OverrideCapabilities(c.model, c.base)
			if got.SupportsThinking != c.wantThinking {
				t.Errorf("SupportsThinking = %v, want %v", got.SupportsThinking, c.wantThinking)
			}
			if got.SupportsImages != c.wantImages {
				t.Errorf("SupportsImages = %v, want %v (base preservation)", got.SupportsImages, c.wantImages)
			}
			if got.SupportsTools != c.wantTools {
				t.Errorf("SupportsTools = %v, want %v (base preservation)", got.SupportsTools, c.wantTools)
			}
		})
	}
}
