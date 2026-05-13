package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const openaiModelsResponse = `{
  "object": "list",
  "data": [
    {"id": "gpt-4.1", "object": "model", "created": 1730000000, "owned_by": "openai"},
    {"id": "gpt-4.1-mini", "object": "model", "created": 1730000000, "owned_by": "openai"},
    {"id": "gpt-4.1-nano", "object": "model", "created": 1730000000, "owned_by": "openai"},
    {"id": "o3-mini", "object": "model", "created": 1730000000, "owned_by": "openai"}
  ]
}`

func TestListOpenAIModels_ParsesAndMapsTier(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openaiModelsResponse))
	}))
	defer srv.Close()

	models, err := listOpenAIModels(context.Background(), ListerCredentials{
		BaseURL: srv.URL, APIKey: "sk-test",
	})
	if err != nil {
		t.Fatalf("listOpenAIModels: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if !strings.HasPrefix(gotPath, "/v1/models") {
		t.Errorf("path = %q, want prefix /v1/models", gotPath)
	}
	if len(models) != 4 {
		t.Fatalf("len = %d, want 4", len(models))
	}

	wantTier := map[string]int{
		"gpt-4.1":      TierFrontier,
		"gpt-4.1-mini": TierStrong,
		"gpt-4.1-nano": TierEfficient,
		"o3-mini":      TierStrong,
	}
	for _, m := range models {
		if m.Provider != "openai" {
			t.Errorf("%s: Provider = %q, want openai", m.ModelID, m.Provider)
		}
		if got, ok := wantTier[m.ModelID]; ok && m.Tier != got {
			t.Errorf("%s: Tier = %d, want %d", m.ModelID, m.Tier, got)
		}
	}
}

func TestListOpenAIModels_HTTPErrorIsPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer srv.Close()
	_, err := listOpenAIModels(context.Background(), ListerCredentials{BaseURL: srv.URL, APIKey: "x"})
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error missing 401: %v", err)
	}
}

func TestListOpenAIModels_EmptyDataIsHandled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()
	models, err := listOpenAIModels(context.Background(), ListerCredentials{BaseURL: srv.URL, APIKey: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("len = %d, want 0", len(models))
	}
}

func TestOpenAITier_Heuristic(t *testing.T) {
	cases := []struct {
		id   string
		want int
	}{
		{"gpt-4.1", TierFrontier},
		{"gpt-4o", TierFrontier},
		{"o3", TierFrontier},
		{"gpt-4.1-mini", TierStrong},
		{"gpt-4o-mini", TierStrong},
		{"o3-mini", TierStrong},
		{"o4-mini", TierStrong},
		{"gpt-4.1-nano", TierEfficient},
		{"unknown-future-model", TierEfficient},
	}
	for _, c := range cases {
		if got := openaiTier(c.id); got != c.want {
			t.Errorf("openaiTier(%q) = %d, want %d", c.id, got, c.want)
		}
	}
}
