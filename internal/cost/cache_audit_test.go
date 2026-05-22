package cost

import (
	"strings"
	"testing"

	"sage-router/internal/compress/tokenizer"
)

// M4 T10 — cache-hint audit. ADR-2's prompt-caching gate marks a Claude
// system prompt cacheable once it exceeds minTokensForCache (1024). The token
// count is estimated by estimateTokens (chars/4). This test asks the question
// the M4 spec §7 deferred to a test: does chars/4 ever disagree with the real
// o200k_base tokenizer about whether a system prompt crosses 1024 tokens?
//
// Finding (also recorded in .sage/decisions.md): for realistic
// system-prompt fixtures the chars/4 estimate stays within ~25% of the
// tokenizer and agrees with it on the cache/no-cache decision away from the
// exact boundary. estimateTokens is therefore LEFT as chars/4 — the 1024
// break-even is itself an approximation (ADR-2), a near-boundary
// misjudgement only marginally mis-optimizes a cost heuristic (never a
// correctness fault), and the tokenizer dependency should not spread beyond
// the compression gate without a demonstrated need (spec §7).
func TestCacheHintAudit_CharsPerFourVsTokenizer(t *testing.T) {
	tk, err := tokenizer.LoadTokenizer()
	if err != nil {
		t.Fatalf("LoadTokenizer: %v", err)
	}

	prose := "You are an expert software engineer. You write clear, well-tested " +
		"code and explain trade-offs precisely without hedging. "

	cases := []struct {
		name          string
		text          string
		clearDecision int // -1 clearly under 1024, +1 clearly over, 0 near the boundary
	}{
		{"short instruction", "You are a helpful assistant.", -1},
		{"moderate prompt", strings.Repeat(prose, 6), -1},
		{"near threshold", strings.Repeat(prose, 70), 0},
		{"large prompt", strings.Repeat(prose, 400), 1},
	}

	for _, c := range cases {
		est := estimateTokens(c.text)
		real := tk.Count(c.text)
		t.Logf("%-18s chars=%-7d chars/4=%-6d tokenizer=%-6d ratio=%.2f",
			c.name, len(c.text), est, real, float64(est)/float64(real))

		// Estimator quality: chars/4 must track the tokenizer within a
		// generous band for English prose.
		ratio := float64(est) / float64(real)
		if ratio < 0.6 || ratio > 1.6 {
			t.Errorf("%s: chars/4 estimate (%d) diverges too far from the tokenizer (%d) — ratio %.2f",
				c.name, est, real, ratio)
		}

		// Away from the exact boundary, the cache decision must agree.
		switch c.clearDecision {
		case -1:
			if est >= minTokensForCache || real >= minTokensForCache {
				t.Errorf("%s: expected both estimators clearly UNDER %d (chars/4=%d, tokenizer=%d)",
					c.name, minTokensForCache, est, real)
			}
		case 1:
			if est < minTokensForCache || real < minTokensForCache {
				t.Errorf("%s: expected both estimators clearly OVER %d (chars/4=%d, tokenizer=%d)",
					c.name, minTokensForCache, est, real)
			}
		}
	}
}
