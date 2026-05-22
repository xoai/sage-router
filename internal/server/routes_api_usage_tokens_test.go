package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"sage-router/internal/store"
)

// M4 T9 / AC10 — handleGetUsage projects the compression-savings token
// columns (tokens_before / tokens_after) into the /api/usage response that
// the dashboard's "≈ Saved" column reads.
func TestHandleGetUsage_ProjectsTokenColumns(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	if err := db.RecordUsage(&store.UsageEntry{
		ID: "u-tok", RequestID: "r-tok", Provider: "openai", Model: "gpt-4o",
		InputTokens: 800, TotalTokens: 800, TokensBefore: 1200, TokensAfter: 800,
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.handleGetUsage(rec, httptest.NewRequest("GET", "/api/usage", nil))
	if rec.Code != 200 {
		t.Fatalf("handleGetUsage: got %d: %s", rec.Code, rec.Body.String())
	}

	var rows []struct {
		ID           string `json:"id"`
		TokensBefore int    `json:"tokens_before"`
		TokensAfter  int    `json:"tokens_after"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode /api/usage response: %v", err)
	}

	found := false
	for _, r := range rows {
		if r.ID == "u-tok" {
			found = true
			if r.TokensBefore != 1200 || r.TokensAfter != 800 {
				t.Errorf("usage row: tokens_before/after = %d/%d, want 1200/800",
					r.TokensBefore, r.TokensAfter)
			}
		}
	}
	if !found {
		t.Fatal("the recorded usage row is missing from the /api/usage response")
	}
}
