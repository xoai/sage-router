package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Minimal OpenRouter response — the M2.8 OpenRouter pricing refresher
// will use a more complete vendored fixture for pricing parsing.
// This lister focuses on catalog (model id, display name, capabilities,
// context window) — not pricing.
const openrouterModelsResponse = `{
  "data": [
    {
      "id": "anthropic/claude-sonnet-4",
      "name": "Anthropic: Claude Sonnet 4",
      "context_length": 200000,
      "top_provider": {"max_completion_tokens": 64000}
    },
    {
      "id": "openai/gpt-4o-mini",
      "name": "OpenAI: GPT-4o Mini",
      "context_length": 128000,
      "top_provider": {"max_completion_tokens": 16384}
    },
    {
      "id": "meta-llama/llama-3.3-70b-instruct",
      "name": "Meta: Llama 3.3 70B Instruct",
      "context_length": 131072,
      "top_provider": {"max_completion_tokens": 32768}
    }
  ]
}`

func TestListOpenRouterModels_PreservesQualifiedIDs(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openrouterModelsResponse))
	}))
	defer srv.Close()

	models, err := listOpenRouterModels(context.Background(), ListerCredentials{
		BaseURL: srv.URL, APIKey: "sk-or-test",
	})
	if err != nil {
		t.Fatalf("listOpenRouterModels: %v", err)
	}
	if gotAuth != "Bearer sk-or-test" {
		t.Errorf("Authorization = %q, want Bearer sk-or-test", gotAuth)
	}
	if len(models) != 3 {
		t.Fatalf("len = %d, want 3", len(models))
	}

	// OpenRouter model IDs stay qualified ("provider/model"). Provider
	// is always "openrouter" — distinct from the underlying upstream's
	// row.
	for _, m := range models {
		if m.Provider != "openrouter" {
			t.Errorf("%s: Provider = %q, want openrouter", m.ModelID, m.Provider)
		}
		if !strings.Contains(m.ModelID, "/") {
			t.Errorf("ModelID %q lost its qualified prefix", m.ModelID)
		}
	}

	// context_length → ContextWindow; top_provider.max_completion_tokens → MaxOutput.
	for _, m := range models {
		if m.ModelID == "anthropic/claude-sonnet-4" {
			if m.ContextWindow != 200000 {
				t.Errorf("context window = %d, want 200000", m.ContextWindow)
			}
			if m.MaxOutput != 64000 {
				t.Errorf("max output = %d, want 64000", m.MaxOutput)
			}
		}
	}
}

func TestListOpenRouterModels_HTTPErrorIsPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`upstream down`))
	}))
	defer srv.Close()
	_, err := listOpenRouterModels(context.Background(), ListerCredentials{BaseURL: srv.URL, APIKey: "x"})
	if err == nil {
		t.Fatal("expected error on 502")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error missing 502: %v", err)
	}
}
