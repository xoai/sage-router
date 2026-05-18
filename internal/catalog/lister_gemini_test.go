package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const geminiModelsResponse = `{
  "models": [
    {
      "name": "models/gemini-2.5-pro",
      "displayName": "Gemini 2.5 Pro",
      "inputTokenLimit": 1048576,
      "outputTokenLimit": 65536,
      "supportedGenerationMethods": ["generateContent", "streamGenerateContent"]
    },
    {
      "name": "models/gemini-2.5-flash",
      "displayName": "Gemini 2.5 Flash",
      "inputTokenLimit": 1048576,
      "outputTokenLimit": 65536,
      "supportedGenerationMethods": ["generateContent"]
    },
    {
      "name": "models/gemini-2.5-flash-lite",
      "displayName": "Gemini 2.5 Flash Lite",
      "inputTokenLimit": 1048576,
      "outputTokenLimit": 65536,
      "supportedGenerationMethods": ["generateContent"]
    },
    {
      "name": "models/embedding-001",
      "displayName": "Embeddings (excluded)",
      "supportedGenerationMethods": ["embedContent"]
    }
  ]
}`

// TestListGeminiModels_StripsPrefixAndFiltersEmbeddings — Gemini
// returns model IDs like "models/gemini-2.5-flash"; we want just
// the bare ID. Models that don't support generateContent (e.g.,
// embedding-only models) are filtered out — they aren't chat models
// and shouldn't pollute the catalog.
func TestListGeminiModels_StripsPrefixAndFiltersEmbeddings(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(geminiModelsResponse))
	}))
	defer srv.Close()

	models, err := listGeminiModels(context.Background(), ListerCredentials{
		BaseURL: srv.URL, APIKey: "AIzaTEST",
	})
	if err != nil {
		t.Fatalf("listGeminiModels: %v", err)
	}
	if gotKey != "AIzaTEST" {
		t.Errorf("?key = %q, want AIzaTEST", gotKey)
	}

	// Embedding model filtered out → 3 not 4.
	if len(models) != 3 {
		t.Fatalf("len = %d, want 3 (embedding-only filtered)", len(models))
	}

	// IDs stripped of "models/" prefix.
	for _, m := range models {
		if strings.HasPrefix(m.ModelID, "models/") {
			t.Errorf("ModelID %q still has \"models/\" prefix", m.ModelID)
		}
		if m.Provider != "gemini" {
			t.Errorf("%s: Provider = %q, want gemini", m.ModelID, m.Provider)
		}
	}

	// Tier mapping spot-check.
	wantTier := map[string]int{
		"gemini-2.5-pro":        TierFrontier,
		"gemini-2.5-flash":      TierStrong,
		"gemini-2.5-flash-lite": TierEfficient,
	}
	for _, m := range models {
		if got, ok := wantTier[m.ModelID]; ok && m.Tier != got {
			t.Errorf("%s: Tier = %d, want %d", m.ModelID, m.Tier, got)
		}
	}
}

func TestListGeminiModels_HTTPErrorIsPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()
	_, err := listGeminiModels(context.Background(), ListerCredentials{BaseURL: srv.URL, APIKey: "x"})
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error missing 403: %v", err)
	}
}

func TestGeminiTier_Heuristic(t *testing.T) {
	cases := []struct {
		id   string
		want int
	}{
		{"gemini-2.5-pro", TierFrontier},
		{"gemini-pro", TierFrontier},
		{"gemini-2.5-flash", TierStrong},
		{"gemini-flash", TierStrong},
		{"gemini-2.5-flash-lite", TierEfficient},
		{"gemini-2.0-flash", TierEfficient},
		{"gemini-nano", TierEfficient},
		{"random-id", TierEfficient},
	}
	for _, c := range cases {
		if got := geminiTier(c.id); got != c.want {
			t.Errorf("geminiTier(%q) = %d, want %d", c.id, got, c.want)
		}
	}
}

// TestListGeminiModels_ProductionShapeBaseURL — initiative
// 20260514-discovery-url-doubling §8. Gemini uses ?key= query auth +
// /v1beta path. Asserts path composition + query string both work
// when BaseURL ends in /v1beta (production shape).
func TestListGeminiModels_ProductionShapeBaseURL(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.URL.Query().Get("key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(geminiModelsResponse))
	}))
	defer srv.Close()

	models, err := listGeminiModels(context.Background(), ListerCredentials{
		BaseURL: srv.URL + "/v1beta",
		APIKey:  "AIza-prodshape",
	})
	if err != nil {
		t.Fatalf("listGeminiModels: %v", err)
	}
	if gotPath != "/v1beta/models" {
		t.Errorf("path = %q, want /v1beta/models (BaseURL already has /v1beta)", gotPath)
	}
	if gotKey != "AIza-prodshape" {
		t.Errorf("?key = %q, want AIza-prodshape", gotKey)
	}
	if len(models) == 0 {
		t.Error("expected non-empty model list")
	}
}
