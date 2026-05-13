package catalog

import (
	"math"
	"testing"

	"sage-router/internal/config"
)

// TestEstimateCost_MatchesStaticConfigForSeed verifies AC6 — the new
// Registry.EstimateCost must return byte-equal USD cost to today's
// config.EstimateCost for every model in the seed catalog when
// CacheReadIn=0, CacheWriteIn=0, Thinking=0 (i.e., the no-cache
// scenario today's static calculator handles).
//
// The Registry isn't built yet; we exercise the underlying
// Pricing.EstimateCost helper directly with a TokenBreakdown carrying
// only Input + Output to match config.EstimateCost's signature.
//
// Iterates every entry in config.ModelCatalog so the golden assertion
// is exhaustive across the seed.
func TestEstimateCost_MatchesStaticConfigForSeed(t *testing.T) {
	cases := []struct {
		inTok, outTok int
	}{
		{0, 0},
		{1000, 500},
		{1_000_000, 500_000},
		{42, 13},
	}

	for modelID, m := range config.ModelCatalog {
		p := Pricing{
			Input:  m.InputPrice,
			Output: m.OutputPrice,
		}
		for _, c := range cases {
			got := p.EstimateCost(TokenBreakdown{
				Input:  c.inTok,
				Output: c.outTok,
			})
			want := config.EstimateCost(m.Provider, modelID, c.inTok, c.outTok)
			if math.Abs(got-want) > 1e-12 {
				t.Errorf("model %q breakdown %v: got %v, want %v (delta %v)",
					modelID, c, got, want, got-want)
			}
		}
	}
}

// TestEstimateCost_CacheReadIsBilledAtCacheReadPrice — RC2 + AC25b
// foundation. Cache-read tokens MUST be priced at CacheRead, not Input.
func TestEstimateCost_CacheReadIsBilledAtCacheReadPrice(t *testing.T) {
	p := Pricing{
		Input:     3.00, // $/1M
		Output:    15.00,
		CacheRead: 0.30,
	}
	// 1M cache-read tokens at $0.30/1M = $0.30, not $3.00.
	got := p.EstimateCost(TokenBreakdown{CacheReadIn: 1_000_000})
	if math.Abs(got-0.30) > 1e-12 {
		t.Errorf("cache-read pricing not applied: got $%v, want $0.30", got)
	}
}

// TestEstimateCost_CacheWriteUsesStoredValueVerbatim — RC2 NM5 fix.
// CacheWrite=0 must NOT fall back to Input*1.25 (that was the r1
// cross-provider default we killed). 0 means "free at write."
func TestEstimateCost_CacheWriteUsesStoredValueVerbatim(t *testing.T) {
	// OpenAI-style row: CacheWrite explicitly 0 → free at write.
	openai := Pricing{Input: 2.00, Output: 8.00, CacheWrite: 0}
	got := openai.EstimateCost(TokenBreakdown{CacheWriteIn: 1_000_000})
	if got != 0 {
		t.Errorf("OpenAI CacheWrite=0 should be free, got $%v", got)
	}

	// Anthropic-style row: CacheWrite explicitly populated → billed
	// at the stored rate.
	anthropic := Pricing{Input: 3.00, Output: 15.00, CacheWrite: 3.75}
	got = anthropic.EstimateCost(TokenBreakdown{CacheWriteIn: 1_000_000})
	if math.Abs(got-3.75) > 1e-12 {
		t.Errorf("Anthropic CacheWrite=3.75 should bill 1M at $3.75, got $%v", got)
	}
}

// TestEstimateCost_ThinkingFallsBackToOutput — when Thinking is unset
// (0), thinking tokens bill at Output rate (the most common provider
// contract today).
func TestEstimateCost_ThinkingFallsBackToOutput(t *testing.T) {
	p := Pricing{Input: 2.00, Output: 8.00, Thinking: 0}
	got := p.EstimateCost(TokenBreakdown{Thinking: 1_000_000})
	if math.Abs(got-8.00) > 1e-12 {
		t.Errorf("Thinking=0 should fall back to Output rate, got $%v", got)
	}

	// Explicit override wins.
	p = Pricing{Input: 2.00, Output: 8.00, Thinking: 10.00}
	got = p.EstimateCost(TokenBreakdown{Thinking: 1_000_000})
	if math.Abs(got-10.00) > 1e-12 {
		t.Errorf("Thinking=10.00 should override Output, got $%v", got)
	}
}

// TestSourceConstants — the source enum must use the exact strings
// the CHECK constraint in migrations 009/010 accepts.
func TestSourceConstants(t *testing.T) {
	if SourceSeed != "seed" {
		t.Errorf("SourceSeed = %q, want \"seed\"", SourceSeed)
	}
	if SourceDiscovery != "discovery" {
		t.Errorf("SourceDiscovery = %q, want \"discovery\"", SourceDiscovery)
	}
	if SourceOpenRouter != "openrouter" {
		t.Errorf("SourceOpenRouter = %q, want \"openrouter\"", SourceOpenRouter)
	}
	if SourceUser != "user" {
		t.Errorf("SourceUser = %q, want \"user\"", SourceUser)
	}
}
