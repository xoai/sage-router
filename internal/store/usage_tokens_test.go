package store

import "testing"

// M4 T7 — usage_log.tokens_before / tokens_after round-trip through
// RecordUsage → QueryUsage (AC9, store half).
func TestRecordUsage_TokenColumns(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordUsage(&UsageEntry{
		ID: "u1", RequestID: "r1", Provider: "openai", Model: "gpt-4o",
		ConnectionID: "c1", InputTokens: 800, TotalTokens: 800,
		TokensBefore: 1200, TokensAfter: 800,
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	rows, err := s.QueryUsage(UsageFilter{})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	var found *UsageEntry
	for i := range rows {
		if rows[i].ID == "u1" {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("u1 not returned by QueryUsage")
	}
	if found.TokensBefore != 1200 || found.TokensAfter != 800 {
		t.Errorf("token columns: got before=%d after=%d, want 1200/800",
			found.TokensBefore, found.TokensAfter)
	}
}

// A row recorded without the token fields persists 0 (migration 017 DEFAULT 0).
func TestRecordUsage_TokenColumns_DefaultZero(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordUsage(&UsageEntry{
		ID: "u2", RequestID: "r2", Provider: "openai", Model: "gpt-4o",
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	rows, _ := s.QueryUsage(UsageFilter{})
	for _, e := range rows {
		if e.ID == "u2" && (e.TokensBefore != 0 || e.TokensAfter != 0) {
			t.Errorf("default token columns: got before=%d after=%d, want 0/0",
				e.TokensBefore, e.TokensAfter)
		}
	}
}
