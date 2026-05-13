package store

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// GetCacheHitRate — 5 named cases per plan task 1.8 done criteria.
// ---------------------------------------------------------------------------

func TestGetCacheHitRate_BasicRatio(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Seed two rows on the same connection inside a 24h window: total
	// input-side tokens = (100 + 50) + (200 + 100) = 450; total
	// cache_read = 50 + 100 = 150. Ratio = 150/450 = 0.333...
	seedUsage(t, st, &UsageEntry{
		Provider: "openai", Model: "gpt-4.1", ConnectionID: "c1",
		InputTokens: 100, OutputTokens: 50, CacheReadTokens: 50,
	})
	seedUsage(t, st, &UsageEntry{
		Provider: "openai", Model: "gpt-4.1", ConnectionID: "c1",
		InputTokens: 200, OutputTokens: 100, CacheReadTokens: 100,
	})

	got, err := st.GetCacheHitRate(ctx, "c1", 24*time.Hour)
	if err != nil {
		t.Fatalf("GetCacheHitRate: %v", err)
	}
	want := 150.0 / 450.0
	if got < want-1e-9 || got > want+1e-9 {
		t.Errorf("GetCacheHitRate = %v, want %v", got, want)
	}
}

func TestGetCacheHitRate_OutsideWindowIgnored(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	s := st.(*sqliteStore)

	// Recent row written via RecordUsage (production write path) —
	// this is the load-bearing assertion: if RecordUsage's storage
	// format drifts from GetCacheHitRate's lookup format, this row
	// won't be counted and the test fails. See self-learning
	// 82fa8ebabdbe4ea49ad3cdff7978e421.
	seedUsage(t, st, &UsageEntry{
		Provider: "openai", Model: "gpt-4.1", ConnectionID: "c1",
		InputTokens: 100, OutputTokens: 50, CacheReadTokens: 50,
	})

	// Out-of-window row needs a past timestamp; RecordUsage can't
	// time-travel, so raw INSERT is structurally required. Use the
	// SAME timeStr format the production write path uses so the
	// lookup-vs-storage format contract is honored consistently.
	old := time.Now().UTC().Add(-25 * time.Hour)
	_, err := s.db.Exec(
		`INSERT INTO usage_log (id, request_id, provider, model, connection_id, api_key_id,
			input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
			cost, latency_ms, status, created_at, cost_source)
		 VALUES ('old', 'old', 'openai', 'gpt-4.1', 'c1', '', 100, 50, 150, 100, 0, 0, 100, 'ok', ?, 'apikey')`,
		timeStr(old),
	)
	if err != nil {
		t.Fatalf("insert old: %v", err)
	}

	// Only the recent row counts: 50 / (100 + 50) = 0.333...
	got, err := st.GetCacheHitRate(ctx, "c1", 24*time.Hour)
	if err != nil {
		t.Fatalf("GetCacheHitRate: %v", err)
	}
	want := 50.0 / 150.0
	if got < want-1e-9 || got > want+1e-9 {
		t.Errorf("GetCacheHitRate = %v, want %v (recent row only)", got, want)
	}
}

func TestGetCacheHitRate_WrongConnectionIgnored(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// c1 has no cache; c2 does. Querying c1 must return 0, not c2's ratio.
	seedUsage(t, st, &UsageEntry{
		Provider: "openai", Model: "gpt-4.1", ConnectionID: "c1",
		InputTokens: 100, CacheReadTokens: 0,
	})
	seedUsage(t, st, &UsageEntry{
		Provider: "openai", Model: "gpt-4.1", ConnectionID: "c2",
		InputTokens: 100, CacheReadTokens: 100,
	})

	got, err := st.GetCacheHitRate(ctx, "c1", 24*time.Hour)
	if err != nil {
		t.Fatalf("GetCacheHitRate: %v", err)
	}
	// c1 has 0 cache reads out of 100 input → ratio = 0.
	if got != 0 {
		t.Errorf("GetCacheHitRate(c1) = %v, want 0 (c2 must not leak)", got)
	}
}

func TestGetCacheHitRate_ZeroInputTokensReturnsZero(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// A row with input=0 AND cache_read=0 — denominator is 0; NULLIF
	// must prevent div-by-zero. Returns 0.
	seedUsage(t, st, &UsageEntry{
		Provider: "openai", Model: "gpt-4.1", ConnectionID: "c1",
		InputTokens: 0, CacheReadTokens: 0,
	})

	got, err := st.GetCacheHitRate(ctx, "c1", 24*time.Hour)
	if err != nil {
		t.Fatalf("GetCacheHitRate: %v", err)
	}
	if got != 0 {
		t.Errorf("GetCacheHitRate = %v, want 0 (div-by-zero guard)", got)
	}
}

func TestGetCacheHitRate_EmptyHistoryReturnsZero(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Fresh DB, no rows. Must return 0, not error.
	got, err := st.GetCacheHitRate(ctx, "c1", 24*time.Hour)
	if err != nil {
		t.Fatalf("GetCacheHitRate: %v", err)
	}
	if got != 0 {
		t.Errorf("GetCacheHitRate (empty DB) = %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// ListUsageInRange
// ---------------------------------------------------------------------------

func TestListUsageInRange_FilterByCostSource(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Use seedUsage → RecordUsage (production write path) so a future
	// timeStr format drift would break this test. The previous raw
	// INSERT with hand-formatted time string masked the M1.8
	// format-mismatch bug; see self-learning
	// 82fa8ebabdbe4ea49ad3cdff7978e421.
	seedUsage(t, st, &UsageEntry{
		ID: "apikey-1", Provider: "openai", Model: "gpt-4.1",
		ConnectionID: "c1", InputTokens: 100, OutputTokens: 50, CostSource: "apikey",
	})
	seedUsage(t, st, &UsageEntry{
		ID: "apikey-2", Provider: "openai", Model: "gpt-4.1",
		ConnectionID: "c1", InputTokens: 100, OutputTokens: 50, CostSource: "apikey",
	})
	seedUsage(t, st, &UsageEntry{
		ID: "sub-1", Provider: "openai", Model: "gpt-4.1",
		ConnectionID: "c1", InputTokens: 100, OutputTokens: 50, CostSource: "subscription",
	})

	now := time.Now().UTC()
	from := now.Add(-1 * time.Hour)
	to := now.Add(1 * time.Hour)

	apikey, err := st.ListUsageInRange(ctx, from, to, "apikey")
	if err != nil {
		t.Fatalf("ListUsageInRange apikey: %v", err)
	}
	if len(apikey) != 2 {
		t.Errorf("apikey filter len = %d, want 2", len(apikey))
	}
	for _, e := range apikey {
		if e.CostSource != "apikey" {
			t.Errorf("unexpected cost_source %q in apikey filter", e.CostSource)
		}
	}

	sub, err := st.ListUsageInRange(ctx, from, to, "subscription")
	if err != nil {
		t.Fatalf("ListUsageInRange subscription: %v", err)
	}
	if len(sub) != 1 {
		t.Errorf("subscription filter len = %d, want 1", len(sub))
	}
}

func TestListUsageInRange_EmptyCostSourceReturnsAll(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// RecordUsage path (production write) so storage-format drift is
	// caught here.
	for _, cs := range []string{"apikey", "subscription"} {
		seedUsage(t, st, &UsageEntry{
			ID: cs + "-row", Provider: "openai", Model: "gpt-4.1",
			ConnectionID: "c1", InputTokens: 100, OutputTokens: 50, CostSource: cs,
		})
	}

	now := time.Now().UTC()
	all, err := st.ListUsageInRange(ctx, now.Add(-1*time.Hour), now.Add(1*time.Hour), "")
	if err != nil {
		t.Fatalf("ListUsageInRange all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("empty cost_source filter len = %d, want 2", len(all))
	}
}

func TestListUsageInRange_OutsideRangeExcluded(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	s := st.(*sqliteStore)

	// "inside" row uses the production write path (RecordUsage). The
	// out-of-window rows need explicit past/future timestamps —
	// RecordUsage can't time-travel, so raw INSERT is structurally
	// required. The raw INSERTs use timeStr(...) to match the same
	// format RecordUsage writes, so the lookup-vs-storage contract
	// stays honored end-to-end.
	seedUsage(t, st, &UsageEntry{
		ID: "inside", Provider: "openai", Model: "gpt-4.1",
		ConnectionID: "c1", InputTokens: 100, OutputTokens: 50, CostSource: "apikey",
	})

	now := time.Now().UTC()
	for _, c := range []struct {
		id string
		ts time.Time
	}{
		{"too-old", now.Add(-48 * time.Hour)},
		{"too-new", now.Add(48 * time.Hour)},
	} {
		_, err := s.db.Exec(
			`INSERT INTO usage_log (id, request_id, provider, model, connection_id, api_key_id,
				input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
				cost, latency_ms, status, created_at, cost_source)
			 VALUES (?, ?, 'openai', 'gpt-4.1', 'c1', '', 100, 50, 150, 0, 0, 0, 100, 'ok', ?, 'apikey')`,
			c.id, c.id, timeStr(c.ts),
		)
		if err != nil {
			t.Fatalf("insert %s: %v", c.id, err)
		}
	}

	got, err := st.ListUsageInRange(ctx, now.Add(-1*time.Hour), now.Add(1*time.Hour), "")
	if err != nil {
		t.Fatalf("ListUsageInRange: %v", err)
	}
	if len(got) != 1 || got[0].ID != "inside" {
		t.Errorf("filter returned %+v, want only the 'inside' row", got)
	}
}

// ---------------------------------------------------------------------------
// UpdateUsageCost — AC32
// ---------------------------------------------------------------------------

func TestUpdateUsageCost_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Insert a usage_log row with cost=X via RecordUsage.
	id := seedUsage(t, st, &UsageEntry{
		Provider: "openai", Model: "gpt-4.1", ConnectionID: "c1",
		InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500,
		Cost: 0.0015, CostSource: "apikey",
	})

	// Update cost to Y.
	const newCost = 0.0099
	if err := st.UpdateUsageCost(ctx, id, newCost); err != nil {
		t.Fatalf("UpdateUsageCost: %v", err)
	}

	// Read back: cost = Y, cost_source unchanged.
	entries, err := st.QueryUsage(UsageFilter{Limit: 100})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	var got *UsageEntry
	for i := range entries {
		if entries[i].ID == id {
			got = &entries[i]
			break
		}
	}
	if got == nil {
		t.Fatal("row not found after UpdateUsageCost")
	}
	if got.Cost != newCost {
		t.Errorf("Cost = %v, want %v", got.Cost, newCost)
	}
	if got.CostSource != "apikey" {
		t.Errorf("CostSource = %q, want \"apikey\" (must NOT be touched)", got.CostSource)
	}
	if got.InputTokens != 1000 || got.OutputTokens != 500 {
		t.Errorf("token counts changed: input=%d output=%d", got.InputTokens, got.OutputTokens)
	}
}

func TestUpdateUsageCost_NoSuchIDIsNoOp(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Call with an ID that doesn't exist. Must NOT error — the SQL is
	// idempotent (UPDATE ... WHERE id=? matches 0 rows).
	if err := st.UpdateUsageCost(ctx, "no-such-id", 1.23); err != nil {
		t.Errorf("UpdateUsageCost on missing id returned err: %v", err)
	}

	// Also: it must not magically insert a row.
	entries, _ := st.QueryUsage(UsageFilter{Limit: 100})
	if len(entries) != 0 {
		t.Errorf("UpdateUsageCost on missing id created %d rows", len(entries))
	}
}
