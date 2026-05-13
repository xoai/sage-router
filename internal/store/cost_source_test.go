package store

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// seedUsageCounter guarantees distinct IDs across rapid consecutive
// seedUsage calls. time.Now() resolution is too coarse on Windows
// (often 15ms) to act as a uniqueness source on its own.
var seedUsageCounter atomic.Uint64

// seedUsage writes a usage row and returns its ID. Cleanup is automatic
// via the in-memory DB the test created.
func seedUsage(t *testing.T, db Store, e *UsageEntry) string {
	t.Helper()
	if e.ID == "" {
		e.ID = "u-" + t.Name() + "-" + strconv.FormatUint(seedUsageCounter.Add(1), 10) + "-" + time.Now().Format("150405")
	}
	if e.RequestID == "" {
		e.RequestID = e.ID
	}
	if err := db.RecordUsage(e); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	return e.ID
}

func TestUsageEntry_CostSourceRoundTrip(t *testing.T) {
	st := newTestStore(t)
	id := seedUsage(t, st, &UsageEntry{
		Provider:     "openai",
		Model:        "gpt-5",
		ConnectionID: "c1",
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
		Cost:         0,
		CostSource:   "subscription",
		Latency:      250 * time.Millisecond,
		Status:       "ok",
	})

	entries, err := st.QueryUsage(UsageFilter{Limit: 10})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.ID != id {
		t.Errorf("ID mismatch: %q vs %q", e.ID, id)
	}
	if e.CostSource != "subscription" {
		t.Errorf("CostSource = %q, want subscription", e.CostSource)
	}
	if e.Cost != 0 {
		t.Errorf("Cost = %v, want 0", e.Cost)
	}
}

func TestRecordUsage_DefaultsEmptyCostSourceToApikey(t *testing.T) {
	// Older call sites (or tests) that don't set CostSource should
	// default to "apikey" rather than persisting an empty string.
	st := newTestStore(t)
	seedUsage(t, st, &UsageEntry{
		Provider:  "openai",
		Model:     "gpt-5",
		Latency:   10 * time.Millisecond,
		Status:    "ok",
		// CostSource intentionally left empty.
	})

	entries, _ := st.QueryUsage(UsageFilter{Limit: 1})
	if entries[0].CostSource != "apikey" {
		t.Errorf("empty CostSource on input → expected default 'apikey', got %q", entries[0].CostSource)
	}
}

func TestUsageSummary_ByCostSource(t *testing.T) {
	st := newTestStore(t)
	// Mix of cost sources.
	seedUsage(t, st, &UsageEntry{Provider: "openai", Model: "gpt-5", CostSource: "apikey", Cost: 0.10, TotalTokens: 100, Status: "ok"})
	seedUsage(t, st, &UsageEntry{Provider: "openai", Model: "gpt-5", CostSource: "apikey", Cost: 0.20, TotalTokens: 200, Status: "ok"})
	seedUsage(t, st, &UsageEntry{Provider: "anthropic", Model: "claude-opus-4", CostSource: "subscription", Cost: 0, TotalTokens: 500, Status: "ok"})

	sum, err := st.UsageSummary(UsageFilter{})
	if err != nil {
		t.Fatalf("UsageSummary: %v", err)
	}
	if got, want := sum.ByCostSource["apikey"].Requests, 2; got != want {
		t.Errorf("apikey requests = %d, want %d", got, want)
	}
	if got, want := sum.ByCostSource["apikey"].Tokens, 300; got != want {
		t.Errorf("apikey tokens = %d, want %d", got, want)
	}
	if got, want := sum.ByCostSource["subscription"].Requests, 1; got != want {
		t.Errorf("subscription requests = %d, want %d", got, want)
	}
	if got, want := sum.ByCostSource["subscription"].Tokens, 500; got != want {
		t.Errorf("subscription tokens = %d, want %d", got, want)
	}
	if sum.ByCostSource["subscription"].Cost != 0 {
		t.Errorf("subscription cost = %v, want 0", sum.ByCostSource["subscription"].Cost)
	}
}

func TestSubscriptionUsageGroups_GroupsByProviderModel(t *testing.T) {
	st := newTestStore(t)
	// Two subscription rows for the same (provider, model) — should aggregate.
	seedUsage(t, st, &UsageEntry{Provider: "openai", Model: "gpt-5", CostSource: "subscription", InputTokens: 100, OutputTokens: 50, Status: "ok"})
	seedUsage(t, st, &UsageEntry{Provider: "openai", Model: "gpt-5", CostSource: "subscription", InputTokens: 200, OutputTokens: 100, Status: "ok"})
	// Different model.
	seedUsage(t, st, &UsageEntry{Provider: "anthropic", Model: "claude-opus-4", CostSource: "subscription", InputTokens: 300, OutputTokens: 200, Status: "ok"})
	// API-key row — should be filtered out.
	seedUsage(t, st, &UsageEntry{Provider: "openai", Model: "gpt-5", CostSource: "apikey", InputTokens: 999, OutputTokens: 999, Status: "ok"})

	groups, err := st.SubscriptionUsageGroups(UsageFilter{})
	if err != nil {
		t.Fatalf("SubscriptionUsageGroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}

	// Build a lookup so order doesn't matter.
	by := map[string]SubscriptionUsageGroup{}
	for _, g := range groups {
		by[g.Provider+"/"+g.Model] = g
	}
	if g := by["openai/gpt-5"]; g.InputTokens != 300 || g.OutputTokens != 150 {
		t.Errorf("openai/gpt-5 aggregate = (in=%d, out=%d), want (300, 150)", g.InputTokens, g.OutputTokens)
	}
	if g := by["anthropic/claude-opus-4"]; g.InputTokens != 300 || g.OutputTokens != 200 {
		t.Errorf("anthropic/claude-opus-4 = (in=%d, out=%d), want (300, 200)", g.InputTokens, g.OutputTokens)
	}
}
