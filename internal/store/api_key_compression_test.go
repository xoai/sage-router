package store

import "testing"

// M4 T5 — api_keys.compression_enabled round-trips through create / list /
// update (AC7, store half). The update path is the spec-review MA2 guard:
// compression_enabled must be in UpdateAPIKey's column whitelist or the edit
// silently no-ops.
func TestAPIKey_CompressionEnabled_RoundTrips(t *testing.T) {
	s := newTestStore(t)

	k := &APIKey{ID: "k1", Name: "compressed", KeyHash: "hash-1", Prefix: "sk-sage-aaa", CompressionEnabled: true}
	if err := s.CreateAPIKey(k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	got, err := s.GetAPIKeyByHash("hash-1")
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if !got.CompressionEnabled {
		t.Error("CompressionEnabled lost on the create→get round-trip")
	}

	page, err := s.ListAPIKeysPaged(APIKeyFilter{})
	if err != nil {
		t.Fatalf("ListAPIKeysPaged: %v", err)
	}
	found := false
	for _, item := range page.Items {
		if item.ID == "k1" {
			found = true
			if !item.CompressionEnabled {
				t.Error("CompressionEnabled lost on the list round-trip")
			}
		}
	}
	if !found {
		t.Fatal("k1 missing from the listing")
	}

	// Flip it off via UpdateAPIKey — proves compression_enabled is whitelisted.
	if err := s.UpdateAPIKey("k1", map[string]any{"compression_enabled": false}); err != nil {
		t.Fatalf("UpdateAPIKey: %v", err)
	}
	got2, err := s.GetAPIKeyByHash("hash-1")
	if err != nil {
		t.Fatalf("GetAPIKeyByHash after update: %v", err)
	}
	if got2.CompressionEnabled {
		t.Error("UpdateAPIKey did not clear CompressionEnabled — compression_enabled missing from the whitelist?")
	}
}

// A key created without the flag defaults to compression off (migration 016
// DEFAULT 0).
func TestAPIKey_CompressionEnabled_DefaultsFalse(t *testing.T) {
	s := newTestStore(t)
	k := &APIKey{ID: "k2", Name: "plain", KeyHash: "hash-2", Prefix: "sk-sage-bbb"}
	if err := s.CreateAPIKey(k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	got, err := s.GetAPIKeyByHash("hash-2")
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if got.CompressionEnabled {
		t.Error("a key created without the flag must default to compression off")
	}
}
