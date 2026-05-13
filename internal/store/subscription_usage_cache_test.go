package store

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// subUsageCounter — distinct IDs across rapid seedRaw calls. Same
// rationale as seedUsageCounter (Windows clock granularity).
var subUsageCounter atomic.Uint64

// seedSubRow inserts a usage_log row with explicit cache token counts
// and cost_source='subscription'. Used by the RC2 tests to verify
// that SubscriptionUsageGroups carries cache columns through to the
// aggregation.
func seedSubRow(t *testing.T, st Store, provider, model string,
	inputTokens, outputTokens, cacheRead, cacheWrite int,
) {
	t.Helper()
	s := st.(*sqliteStore)
	id := "sub-" + t.Name() + "-" + strconv.FormatUint(subUsageCounter.Add(1), 10)
	// Format must match RecordUsage's `timeStr` (ISO 8601 with T-separator
	// and Z suffix). Earlier space-separated format wouldn't match
	// ListUsageInRange's lexicographic comparison — see self-learning
	// 82fa8ebabdbe4ea49ad3cdff7978e421.
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err := s.db.Exec(
		`INSERT INTO usage_log (id, request_id, provider, model, connection_id, api_key_id,
			input_tokens, output_tokens, total_tokens,
			cache_read_tokens, cache_write_tokens,
			cost, latency_ms, status, created_at, cost_source)
		 VALUES (?, ?, ?, ?, 'c1', '', ?, ?, ?, ?, ?, 0, 100, 'ok', ?, 'subscription')`,
		id, id, provider, model,
		inputTokens, outputTokens, inputTokens+outputTokens, cacheRead, cacheWrite, now,
	)
	if err != nil {
		t.Fatalf("seedSubRow: %v", err)
	}
}

// TestSubscriptionUsageGroups_IncludesCacheTokens — RC2 fix. The
// aggregation must sum cache_read_tokens and cache_write_tokens
// alongside input_tokens / output_tokens. Without this, the
// routes_api.go:636 rewire (M1.9b) would silently multiply cache
// pricing by zero — AC25b would pass mechanically but the cache-
// aware savings promise would be undelivered.
func TestSubscriptionUsageGroups_IncludesCacheTokens(t *testing.T) {
	st := newTestStore(t)

	// Seed three rows on the same (provider, model): cache_read totals
	// 50 + 200 + 0 = 250; cache_write totals 0 + 10 + 30 = 40.
	seedSubRow(t, st, "anthropic", "claude-sonnet-4-6", 100, 50, 50, 0)
	seedSubRow(t, st, "anthropic", "claude-sonnet-4-6", 200, 100, 200, 10)
	seedSubRow(t, st, "anthropic", "claude-sonnet-4-6", 300, 150, 0, 30)
	// And a different model to verify the GROUP BY still works.
	seedSubRow(t, st, "openai", "gpt-4.1", 1000, 500, 500, 0)

	groups, err := st.SubscriptionUsageGroups(UsageFilter{})
	if err != nil {
		t.Fatalf("SubscriptionUsageGroups: %v", err)
	}

	// Two groups expected.
	if len(groups) != 2 {
		t.Fatalf("group count = %d, want 2", len(groups))
	}

	var anthropic, openai *SubscriptionUsageGroup
	for i := range groups {
		switch groups[i].Provider {
		case "anthropic":
			anthropic = &groups[i]
		case "openai":
			openai = &groups[i]
		}
	}
	if anthropic == nil || openai == nil {
		t.Fatalf("missing provider in groups: %+v", groups)
	}

	// Anthropic: input=600, output=300, cache_read=250, cache_write=40.
	if anthropic.InputTokens != 600 || anthropic.OutputTokens != 300 {
		t.Errorf("anthropic in/out = %d/%d, want 600/300",
			anthropic.InputTokens, anthropic.OutputTokens)
	}
	if anthropic.CacheReadTokens != 250 {
		t.Errorf("anthropic cache_read = %d, want 250", anthropic.CacheReadTokens)
	}
	if anthropic.CacheWriteTokens != 40 {
		t.Errorf("anthropic cache_write = %d, want 40", anthropic.CacheWriteTokens)
	}

	// OpenAI: input=1000, output=500, cache_read=500, cache_write=0.
	if openai.CacheReadTokens != 500 {
		t.Errorf("openai cache_read = %d, want 500", openai.CacheReadTokens)
	}
	if openai.CacheWriteTokens != 0 {
		t.Errorf("openai cache_write = %d, want 0", openai.CacheWriteTokens)
	}
}

// TestSubscriptionUsageGroups_ZeroCacheStillWorks — legacy rows
// where cache_*_tokens are zero must still aggregate correctly (no
// nil/panic). RC2 must not regress the existing API-key-only-row case.
func TestSubscriptionUsageGroups_ZeroCacheStillWorks(t *testing.T) {
	st := newTestStore(t)

	// Legacy-shape row: cache_*_tokens defaulted to 0.
	seedSubRow(t, st, "anthropic", "claude-haiku-4-5", 100, 50, 0, 0)

	groups, err := st.SubscriptionUsageGroups(UsageFilter{})
	if err != nil {
		t.Fatalf("SubscriptionUsageGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("group count = %d, want 1", len(groups))
	}
	if groups[0].CacheReadTokens != 0 || groups[0].CacheWriteTokens != 0 {
		t.Errorf("zero-cache row got cache = %d/%d, want 0/0",
			groups[0].CacheReadTokens, groups[0].CacheWriteTokens)
	}
}
