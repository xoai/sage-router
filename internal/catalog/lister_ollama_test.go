package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ollama /api/tags response — a list of locally-installed models.
const ollamaTagsResponse = `{
  "models": [
    {
      "name": "llama3.3:70b",
      "modified_at": "2024-12-06T...",
      "size": 42000000000,
      "digest": "abc",
      "details": {"parameter_size": "70B"}
    },
    {
      "name": "mistral-nemo:12b",
      "modified_at": "2024-11-01T...",
      "size": 7100000000,
      "digest": "def",
      "details": {"parameter_size": "12B"}
    }
  ]
}`

func TestListOllamaModels_ParsesAndStampsTierFree(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(ollamaTagsResponse))
	}))
	defer srv.Close()

	models, err := listOllamaModels(context.Background(), ListerCredentials{
		BaseURL: srv.URL,
		// No API key for Ollama — local-only.
	})
	if err != nil {
		t.Fatalf("listOllamaModels: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (Ollama is local, no auth)", gotAuth)
	}
	if gotPath != "/tags" {
		t.Errorf("path = %q, want /tags (BaseURL already has /api; see plan 20260514-discovery-url-doubling)", gotPath)
	}

	if len(models) != 2 {
		t.Fatalf("len = %d, want 2", len(models))
	}
	for _, m := range models {
		if m.Provider != "ollama" {
			t.Errorf("%s: Provider = %q, want ollama", m.ModelID, m.Provider)
		}
		if m.Tier != TierFree {
			t.Errorf("%s: Tier = %d, want %d (TierFree — Ollama is local)",
				m.ModelID, m.Tier, TierFree)
		}
		// Ollama IDs include the tag (e.g., "llama3.3:70b") — preserve verbatim.
		if !strings.Contains(m.ModelID, ":") {
			t.Errorf("ModelID %q lost its :tag suffix", m.ModelID)
		}
	}
}

func TestListOllamaModels_EmptyTagsReturnsNoModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models": []}`))
	}))
	defer srv.Close()
	models, err := listOllamaModels(context.Background(), ListerCredentials{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("len = %d, want 0", len(models))
	}
}

func TestListOllamaModels_HTTPErrorIsPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`ollama server error`))
	}))
	defer srv.Close()
	_, err := listOllamaModels(context.Background(), ListerCredentials{BaseURL: srv.URL})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error missing 500: %v", err)
	}
}

// TestListOllamaModels_ProductionShapeBaseURL — initiative
// 20260514-discovery-url-doubling §8. Ollama uses /api/tags (NOT
// /v1/models). Asserts path composition works when BaseURL ends
// in /api (production shape).
func TestListOllamaModels_ProductionShapeBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(ollamaTagsResponse))
	}))
	defer srv.Close()

	models, err := listOllamaModels(context.Background(), ListerCredentials{
		BaseURL: srv.URL + "/api",
	})
	if err != nil {
		t.Fatalf("listOllamaModels: %v", err)
	}
	if gotPath != "/api/tags" {
		t.Errorf("path = %q, want /api/tags (BaseURL already has /api)", gotPath)
	}
	if len(models) == 0 {
		t.Error("expected non-empty model list")
	}
}
