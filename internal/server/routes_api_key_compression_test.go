package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"sage-router/internal/store"
)

// M4 T6 — the key API carries compression_enabled through create / update /
// list (AC7, API half).

func keyByID(t *testing.T, db store.Store, id string) store.APIKey {
	t.Helper()
	page, err := db.ListAPIKeysPaged(store.APIKeyFilter{})
	if err != nil {
		t.Fatalf("ListAPIKeysPaged: %v", err)
	}
	for _, k := range page.Items {
		if k.ID == id {
			return k
		}
	}
	t.Fatalf("api key %q not found in listing", id)
	return store.APIKey{}
}

func TestHandleAPIKey_CompressionEnabled(t *testing.T) {
	srv, db := setupTestServer(t, nil)

	// Create with compression_enabled: true → persisted.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/keys",
		strings.NewReader(`{"name":"compressed","compression_enabled":true}`))
	srv.handleCreateAPIKey(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: got %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create: no id in response (%v): %s", err, rec.Body.String())
	}
	if !keyByID(t, db, created.ID).CompressionEnabled {
		t.Error("create: compression_enabled:true was not persisted")
	}

	// Update flips it off — handleUpdateAPIKey passes the map straight to
	// UpdateAPIKey, which whitelists compression_enabled (T5).
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("PATCH", "/api/keys/"+created.ID,
		strings.NewReader(`{"compression_enabled":false}`))
	req2.SetPathValue("id", created.ID)
	srv.handleUpdateAPIKey(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("update: got %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
	if keyByID(t, db, created.ID).CompressionEnabled {
		t.Error("update: compression_enabled was not cleared")
	}

	// Create without the field → defaults to false.
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/api/keys", strings.NewReader(`{"name":"plain"}`))
	srv.handleCreateAPIKey(rec3, req3)
	if rec3.Code != 201 {
		t.Fatalf("create plain: got %d, want 201: %s", rec3.Code, rec3.Body.String())
	}
	var plain struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec3.Body.Bytes(), &plain)
	if keyByID(t, db, plain.ID).CompressionEnabled {
		t.Error("create without the field: must default to compression off")
	}
}
