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
	if gotPath != "/models" {
		t.Errorf("path = %q, want /models (BaseURL already has /v1; see plan 20260514-discovery-url-doubling)", gotPath)
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

// TestListOpenAIModels_PrefersAccessTokenOverAPIKey — subscription
// connections supply AccessToken; APIKey is empty. The lister must
// send Bearer <AccessToken>, not the empty APIKey. Without this fix,
// the request sends "Authorization: Bearer " (empty) and 401s
// silently, which is exactly the user-reported "only seed models"
// symptom for OpenAI subscription connections.
func TestListOpenAIModels_PrefersAccessTokenOverAPIKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openaiModelsResponse))
	}))
	defer srv.Close()

	_, err := listOpenAIModels(context.Background(), ListerCredentials{
		BaseURL:     srv.URL,
		AccessToken: "oauth-jwt-token",
		// APIKey deliberately empty — this is the subscription shape.
	})
	if err != nil {
		t.Fatalf("listOpenAIModels: %v", err)
	}
	if gotAuth != "Bearer oauth-jwt-token" {
		t.Errorf("Authorization = %q, want Bearer oauth-jwt-token (AccessToken must take precedence)", gotAuth)
	}
}

// TestListOpenAIModels_FallsBackToAPIKey — regression guard. When
// AccessToken is empty (apikey connections), the lister falls back to
// the APIKey for the Bearer token, preserving pre-fix behavior.
func TestListOpenAIModels_FallsBackToAPIKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openaiModelsResponse))
	}))
	defer srv.Close()

	_, err := listOpenAIModels(context.Background(), ListerCredentials{
		BaseURL: srv.URL,
		APIKey:  "sk-fallback",
		// AccessToken deliberately empty.
	})
	if err != nil {
		t.Fatalf("listOpenAIModels: %v", err)
	}
	if gotAuth != "Bearer sk-fallback" {
		t.Errorf("Authorization = %q, want Bearer sk-fallback (APIKey fallback)", gotAuth)
	}
}

// TestListOpenAIModels_ForwardsChatGPTAccountIDHeader — for OpenAI
// subscription, the discovery request must include the
// ChatGPT-Account-ID header (extracted from id_token at auto-detect
// time and threaded through ListerCredentials.ExtraHeaders). Without
// it, /v1/models may reject the JWT.
func TestListOpenAIModels_ForwardsChatGPTAccountIDHeader(t *testing.T) {
	var gotAccountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.Header.Get("ChatGPT-Account-ID")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openaiModelsResponse))
	}))
	defer srv.Close()

	_, err := listOpenAIModels(context.Background(), ListerCredentials{
		BaseURL:     srv.URL,
		AccessToken: "oauth-jwt",
		ExtraHeaders: map[string]string{
			"ChatGPT-Account-ID": "acct-789",
		},
	})
	if err != nil {
		t.Fatalf("listOpenAIModels: %v", err)
	}
	if gotAccountID != "acct-789" {
		t.Errorf("ChatGPT-Account-ID = %q, want acct-789", gotAccountID)
	}
}

// TestListOpenAIModels_ProductionShapeBaseURL_ParsesAndAuths —
// initiative 20260514-discovery-url-doubling §8. Re-runs the happy
// path with a production-shape BaseURL (srv.URL + "/v1") to catch any
// future regression where header or response-parsing logic only works
// when the BaseURL is bare. §7 (lister_urls_test.go) covers path
// composition for all 5 providers; §8 is a per-provider richer
// assertion (path + Authorization + parses non-empty response).
func TestListOpenAIModels_ProductionShapeBaseURL_ParsesAndAuths(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openaiModelsResponse))
	}))
	defer srv.Close()

	models, err := listOpenAIModels(context.Background(), ListerCredentials{
		BaseURL: srv.URL + "/v1", // production shape — matches config.KnownProviders["openai"].BaseURL pattern
		APIKey:  "sk-prodshape",
	})
	if err != nil {
		t.Fatalf("listOpenAIModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models (production-shape URL composition)", gotPath)
	}
	if gotAuth != "Bearer sk-prodshape" {
		t.Errorf("Authorization = %q, want Bearer sk-prodshape", gotAuth)
	}
	if len(models) == 0 {
		t.Error("expected non-empty model list from valid response")
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
