package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Fixture: Anthropic /v1/models response in the documented shape
// (per platform.claude.com docs verified 2026-05-12 — see
// .sage/docs/anthropic-discovery-feasibility.md). Trimmed to two
// models; pagination not exercised here (M2.2 follow-up).
const anthropicModelsResponse = `{
  "data": [
    {
      "id": "claude-sonnet-4-20250514",
      "display_name": "Claude Sonnet 4",
      "max_input_tokens": 200000,
      "max_tokens": 64000,
      "capabilities": {
        "image_input": {"supported": true},
        "pdf_input": {"supported": true},
        "thinking": {"supported": true, "types": {"enabled": {"supported": true}}},
        "code_execution": {"supported": true},
        "batch": {"supported": true},
        "citations": {"supported": true},
        "structured_outputs": {"supported": false}
      },
      "created_at": "2025-05-14T00:00:00Z",
      "type": "model"
    },
    {
      "id": "claude-haiku-4-5-20251001",
      "display_name": "Claude Haiku 4.5",
      "max_input_tokens": 200000,
      "max_tokens": 64000,
      "capabilities": {
        "image_input": {"supported": true},
        "pdf_input": {"supported": false},
        "thinking": {"supported": true, "types": {"enabled": {"supported": true}}},
        "code_execution": {"supported": false},
        "batch": {"supported": false},
        "citations": {"supported": false},
        "structured_outputs": {"supported": false}
      },
      "created_at": "2025-10-01T00:00:00Z",
      "type": "model"
    }
  ],
  "first_id": "claude-sonnet-4-20250514",
  "last_id": "claude-haiku-4-5-20251001",
  "has_more": false
}`

// TestListAnthropicModels_ParsesDocumentedShape — happy path. Verifies
// the lister sends x-api-key + anthropic-version headers, hits the
// documented endpoint, and maps the response into []catalog.Model
// with tier inference (claude-opus-* / claude-sonnet-* = 1; haiku = 2).
func TestListAnthropicModels_ParsesDocumentedShape(t *testing.T) {
	var (
		gotAuthHeader    string
		gotVersionHeader string
		gotPath          string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("x-api-key")
		gotVersionHeader = r.Header.Get("anthropic-version")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicModelsResponse))
	}))
	defer srv.Close()

	creds := ListerCredentials{
		BaseURL: srv.URL,
		APIKey:  "sk-ant-test-key",
	}
	models, err := listAnthropicModels(context.Background(), creds)
	if err != nil {
		t.Fatalf("listAnthropicModels: %v", err)
	}

	// Auth + version headers.
	if gotAuthHeader != "sk-ant-test-key" {
		t.Errorf("x-api-key = %q, want %q", gotAuthHeader, "sk-ant-test-key")
	}
	if gotVersionHeader == "" {
		t.Error("anthropic-version header missing")
	}
	if gotPath != "/models" {
		t.Errorf("path = %q, want /models (BaseURL already has /v1; see plan 20260514-discovery-url-doubling)", gotPath)
	}

	if len(models) != 2 {
		t.Fatalf("models len = %d, want 2", len(models))
	}

	// Spot-check shape mapping.
	var sonnet, haiku *Model
	for i := range models {
		switch models[i].ModelID {
		case "claude-sonnet-4-20250514":
			sonnet = &models[i]
		case "claude-haiku-4-5-20251001":
			haiku = &models[i]
		}
	}
	if sonnet == nil || haiku == nil {
		t.Fatalf("missing expected model in response: %+v", models)
	}

	// Anthropic provider stamp + tier inference.
	if sonnet.Provider != "anthropic" {
		t.Errorf("sonnet.Provider = %q, want anthropic", sonnet.Provider)
	}
	if sonnet.Tier != TierFrontier {
		t.Errorf("sonnet.Tier = %d, want %d (TierFrontier)", sonnet.Tier, TierFrontier)
	}
	if haiku.Tier != TierStrong {
		t.Errorf("haiku.Tier = %d, want %d (TierStrong)", haiku.Tier, TierStrong)
	}

	// max_input_tokens → ContextWindow; max_tokens → MaxOutput.
	if sonnet.ContextWindow != 200000 {
		t.Errorf("sonnet.ContextWindow = %d, want 200000", sonnet.ContextWindow)
	}
	if sonnet.MaxOutput != 64000 {
		t.Errorf("sonnet.MaxOutput = %d, want 64000", sonnet.MaxOutput)
	}

	// Capability mapping.
	if !sonnet.Caps.SupportsImages {
		t.Error("sonnet.SupportsImages = false, want true (image_input.supported)")
	}
	if !sonnet.Caps.SupportsThinking {
		t.Error("sonnet.SupportsThinking = false, want true")
	}
	// Anthropic models all support tools per docs (no explicit
	// `tools` field in the API response — hardcoded per the
	// feasibility doc).
	if !sonnet.Caps.SupportsTools {
		t.Error("sonnet.SupportsTools = false, want true (hardcoded for Anthropic)")
	}

	// DisplayName comes from the response.
	if sonnet.DisplayName != "Claude Sonnet 4" {
		t.Errorf("sonnet.DisplayName = %q, want \"Claude Sonnet 4\"", sonnet.DisplayName)
	}
}

// TestListAnthropicModels_HTTPErrorIsPropagated — 401/403/5xx must
// produce a useful error (the discovery loop persists it to
// last_discovery_error).
func TestListAnthropicModels_HTTPErrorIsPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"unauthorized","message":"invalid api key"}}`))
	}))
	defer srv.Close()

	creds := ListerCredentials{BaseURL: srv.URL, APIKey: "bad-key"}
	_, err := listAnthropicModels(context.Background(), creds)
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q does not mention HTTP 401", err.Error())
	}
}

// TestListAnthropicModels_EmptyResponseIsHandled — `data: []` is valid
// (means the provider returned zero models, e.g., for a key with no
// allowlist). The lister must return `(nil, nil)` (no error) and let
// the empty-list-safety branch in the DiscoveryRunner preserve
// existing rows (AC14b).
func TestListAnthropicModels_EmptyResponseIsHandled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [], "has_more": false}`))
	}))
	defer srv.Close()

	creds := ListerCredentials{BaseURL: srv.URL, APIKey: "test"}
	models, err := listAnthropicModels(context.Background(), creds)
	if err != nil {
		t.Fatalf("listAnthropicModels: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("expected empty model list, got %d", len(models))
	}
}

// TestListAnthropicModels_UsesBearerWhenAccessTokenSet — subscription
// connections supply AccessToken; APIKey is empty. The lister must
// send Authorization: Bearer <jwt> and NOT send the x-api-key header,
// mirroring the executor's auth-type-aware pattern at default.go:55-69.
//
// R1 caveat: if Anthropic /v1/models rejects Bearer JWTs (only
// x-api-key) in production, the user-visible outcome stays the same
// as today (401 either way); only the test assertion is affected.
func TestListAnthropicModels_UsesBearerWhenAccessTokenSet(t *testing.T) {
	var gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicModelsResponse))
	}))
	defer srv.Close()

	_, err := listAnthropicModels(context.Background(), ListerCredentials{
		BaseURL:     srv.URL,
		AccessToken: "anthropic-oauth-jwt",
	})
	if err != nil {
		t.Fatalf("listAnthropicModels: %v", err)
	}
	if gotAuth != "Bearer anthropic-oauth-jwt" {
		t.Errorf("Authorization = %q, want Bearer anthropic-oauth-jwt", gotAuth)
	}
	if gotAPIKey != "" {
		t.Errorf("x-api-key = %q, want empty when AccessToken is set (no dual-auth headers)", gotAPIKey)
	}
}

// TestListAnthropicModels_UsesAPIKeyHeaderForApiKeyAuth — regression
// guard. Pre-fix behavior preserved when AccessToken is empty:
// x-api-key header is sent, no Authorization header.
func TestListAnthropicModels_UsesAPIKeyHeaderForApiKeyAuth(t *testing.T) {
	var gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicModelsResponse))
	}))
	defer srv.Close()

	_, err := listAnthropicModels(context.Background(), ListerCredentials{
		BaseURL: srv.URL,
		APIKey:  "sk-ant-classic-key",
		// AccessToken empty.
	})
	if err != nil {
		t.Fatalf("listAnthropicModels: %v", err)
	}
	if gotAPIKey != "sk-ant-classic-key" {
		t.Errorf("x-api-key = %q, want sk-ant-classic-key", gotAPIKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty when only APIKey is set", gotAuth)
	}
}

// TestListAnthropicModels_ProductionShapeBaseURL_ParsesAndAuths —
// initiative 20260514-discovery-url-doubling §8. Same intent as the
// openai counterpart: assert headers + parsing work when BaseURL has
// the /v1 segment (production shape).
func TestListAnthropicModels_ProductionShapeBaseURL_ParsesAndAuths(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicModelsResponse))
	}))
	defer srv.Close()

	models, err := listAnthropicModels(context.Background(), ListerCredentials{
		BaseURL: srv.URL + "/v1",
		APIKey:  "sk-ant-prodshape",
	})
	if err != nil {
		t.Fatalf("listAnthropicModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models (production-shape URL composition)", gotPath)
	}
	if gotAPIKey != "sk-ant-prodshape" {
		t.Errorf("x-api-key = %q, want sk-ant-prodshape", gotAPIKey)
	}
	if gotVersion == "" {
		t.Error("anthropic-version header missing")
	}
	if len(models) == 0 {
		t.Error("expected non-empty model list from valid response")
	}
}

// TestAnthropicTier_HeuristicCovers — direct unit test of the
// tier-inference heuristic, decoupled from HTTP. Locks the contract
// from .sage/docs/anthropic-discovery-feasibility.md.
func TestAnthropicTier_HeuristicCovers(t *testing.T) {
	cases := []struct {
		id   string
		want int
	}{
		{"claude-opus-4-20250514", TierFrontier},
		{"claude-opus-4-6", TierFrontier},
		{"claude-sonnet-4-20250514", TierFrontier},
		{"claude-sonnet-4-6", TierFrontier},
		{"claude-haiku-4-5-20251001", TierStrong},
		{"claude-haiku-3", TierStrong},
		{"claude-3-future", TierEfficient}, // unknown family defaults
		{"unknown-anthropic-model", TierEfficient},
	}
	for _, c := range cases {
		got := anthropicTier(c.id)
		if got != c.want {
			t.Errorf("anthropicTier(%q) = %d, want %d", c.id, got, c.want)
		}
	}
}
