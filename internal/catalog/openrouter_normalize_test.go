package catalog

import "testing"

// Initiative: 20260514-pricing-mirror. ID normalization helpers used by
// the OpenRouterRefresher to mirror pricing into direct-provider rows
// across the OpenRouter (dot-form) vs direct-discovery (dash-form +
// optional date suffix) namespace boundary.

func TestNormalizeID_OpenAIIsNoOp(t *testing.T) {
	cases := []string{"gpt-5", "gpt-4o-mini", "o3", "o4-mini"}
	for _, id := range cases {
		if got := normalizeID("openai", id); got != id {
			t.Errorf("normalizeID(openai, %q) = %q, want %q (no-op)", id, got, id)
		}
	}
}

func TestNormalizeID_AnthropicDotToDash(t *testing.T) {
	if got := normalizeID("anthropic", "claude-opus-4.7"); got != "claude-opus-4-7" {
		t.Errorf("got %q, want claude-opus-4-7", got)
	}
	if got := normalizeID("anthropic", "claude-sonnet-4.5"); got != "claude-sonnet-4-5" {
		t.Errorf("got %q, want claude-sonnet-4-5", got)
	}
}

func TestNormalizeID_AnthropicStripsDateSuffix(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-1-20250805":   "claude-opus-4-1",
		"claude-sonnet-4-5-20250929": "claude-sonnet-4-5",
		"claude-haiku-4-5-20251001":  "claude-haiku-4-5",
		"claude-opus-4-20250514":     "claude-opus-4",
	}
	for in, want := range cases {
		if got := normalizeID("anthropic", in); got != want {
			t.Errorf("normalizeID(anthropic, %q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeID_AnthropicDotAndDateCombined(t *testing.T) {
	// Hypothetical input where OpenRouter could emit dots AND we have
	// the date-suffixed variant in catalog. Both forms must normalize
	// to the same canonical for resolveMirrorCandidates to match them.
	if got := normalizeID("anthropic", "claude-opus-4.1-20250805"); got != "claude-opus-4-1" {
		t.Errorf("got %q, want claude-opus-4-1", got)
	}
}

func TestNormalizeID_DateRegexBoundary(t *testing.T) {
	cases := map[string]string{
		// Years before 2020: do NOT strip (regex constrained to 20\d\d).
		"claude-version-19991231": "claude-version-19991231",
		// Plausible YYYY=2020, month=01, day=01: strips.
		"claude-foo-20200101": "claude-foo",
		// Leap day Feb 29 2020: regex accepts (validates DD ∈ 01-31, not month-day combos).
		"claude-foo-20200229": "claude-foo",
		// Invalid month 13: regex rejects (matches only 01-12).
		"claude-foo-20251301": "claude-foo-20251301",
		// Invalid day 32: regex rejects (matches only 01-31).
		"claude-foo-20250532": "claude-foo-20250532",
		// 7 digits (one short): does NOT strip.
		"claude-foo-2025080": "claude-foo-2025080",
		// 9 digits (one too many): does NOT strip (the regex is anchored to $).
		"claude-foo-202508051": "claude-foo-202508051",
	}
	for in, want := range cases {
		if got := normalizeID("anthropic", in); got != want {
			t.Errorf("normalizeID(anthropic, %q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeID_GeminiNoOp(t *testing.T) {
	// Gemini uses bare IDs ("gemini-2.5-pro") on both OpenRouter (via
	// "google/" prefix stripped at the call site) and our direct
	// discovery. No normalization needed.
	if got := normalizeID("gemini", "gemini-2.5-pro"); got != "gemini-2.5-pro" {
		t.Errorf("got %q, want gemini-2.5-pro (no-op)", got)
	}
}

func TestNormalizeID_UnknownProviderPassthrough(t *testing.T) {
	// Defensive: if a future caller passes a provider we haven't
	// taught the helper, return the ID unchanged rather than panicking.
	if got := normalizeID("xai", "grok-3.0-fast.beta"); got != "grok-3.0-fast.beta" {
		t.Errorf("got %q, want grok-3.0-fast.beta (passthrough)", got)
	}
}

func TestResolveMirrorCandidates_HappyPath(t *testing.T) {
	candidates := []PricingUpdate{
		{Provider: "openai", ModelID: "gpt-5", Pricing: Pricing{Input: 1.25, Source: SourceOpenRouter}},
		{Provider: "openai", ModelID: "gpt-9999", Pricing: Pricing{Input: 99.99}}, // no catalog row
	}
	existing := map[string][]string{
		"openai": {"gpt-5", "gpt-4o"},
	}
	out := resolveMirrorCandidates(candidates, existing)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1 (gpt-9999 has no catalog row)", len(out))
	}
	if out[0].ModelID != "gpt-5" {
		t.Errorf("ModelID = %q, want gpt-5", out[0].ModelID)
	}
	if out[0].Pricing.Input != 1.25 {
		t.Errorf("Input = %v, want 1.25", out[0].Pricing.Input)
	}
}

func TestResolveMirrorCandidates_AnthropicDateMatch(t *testing.T) {
	// OpenRouter sends "anthropic/claude-opus-4.1" (we already stripped
	// "anthropic/" in parse). Catalog has "claude-opus-4-1-20250805"
	// (date-suffixed). Both should normalize to "claude-opus-4-1" and
	// match. The output's ModelID must be the catalog row's exact ID
	// (not the normalized form) so the FK target is correct.
	candidates := []PricingUpdate{
		{Provider: "anthropic", ModelID: "claude-opus-4.1", Pricing: Pricing{Input: 15.0}},
	}
	existing := map[string][]string{
		"anthropic": {"claude-opus-4-1-20250805"},
	}
	out := resolveMirrorCandidates(candidates, existing)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].ModelID != "claude-opus-4-1-20250805" {
		t.Errorf("ModelID = %q, want claude-opus-4-1-20250805 (catalog's exact ID, NOT the normalized form)", out[0].ModelID)
	}
}

func TestResolveMirrorCandidates_NoMatchReturnsEmpty(t *testing.T) {
	candidates := []PricingUpdate{
		{Provider: "openai", ModelID: "gpt-future-2099", Pricing: Pricing{Input: 100.0}},
	}
	existing := map[string][]string{
		"openai": {"gpt-5", "gpt-4o"},
	}
	out := resolveMirrorCandidates(candidates, existing)
	if len(out) != 0 {
		t.Errorf("len = %d, want 0 (no matches)", len(out))
	}
}

func TestResolveMirrorCandidates_MultiMatch(t *testing.T) {
	// Hypothetical: OpenRouter's "anthropic/claude-opus-4.7" matches
	// BOTH "claude-opus-4-7" AND "claude-opus-4-7-20251119" in catalog.
	// Both rows should get the same pricing.
	candidates := []PricingUpdate{
		{Provider: "anthropic", ModelID: "claude-opus-4.7", Pricing: Pricing{Input: 5.0}},
	}
	existing := map[string][]string{
		"anthropic": {"claude-opus-4-7", "claude-opus-4-7-20251119"},
	}
	out := resolveMirrorCandidates(candidates, existing)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (one OpenRouter price → two catalog rows)", len(out))
	}
	gotIDs := map[string]bool{out[0].ModelID: true, out[1].ModelID: true}
	if !gotIDs["claude-opus-4-7"] || !gotIDs["claude-opus-4-7-20251119"] {
		t.Errorf("got IDs %v, want both claude-opus-4-7 and claude-opus-4-7-20251119", gotIDs)
	}
}

func TestUniqueProviders(t *testing.T) {
	updates := []PricingUpdate{
		{Provider: "openai"},
		{Provider: "anthropic"},
		{Provider: "openai"}, // duplicate
		{Provider: "gemini"},
		{Provider: "anthropic"}, // duplicate
	}
	out := uniqueProviders(updates)
	if len(out) != 3 {
		t.Errorf("len = %d, want 3 (openai+anthropic+gemini)", len(out))
	}
	seen := map[string]bool{}
	for _, p := range out {
		seen[p] = true
	}
	for _, want := range []string{"openai", "anthropic", "gemini"} {
		if !seen[want] {
			t.Errorf("missing provider %q in result %v", want, out)
		}
	}
}
