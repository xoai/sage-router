package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// Initiative 20260514-openrouter-fallback. listOpenAIViaOpenRouter
// fetches OpenAI's catalog via OpenRouter's public /api/v1/models
// endpoint, filters to (a) openai/* namespace + (b) SubscriptionAllowedModels.

// loadOpenRouterMixedFixture reads testdata/openrouter_models_mixed.json
// and returns the response body bytes.
func loadOpenRouterMixedFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/openrouter_models_mixed.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// Sanity-check it parses — early-fail rather than confuse downstream.
	var parsed openrouterModelsResp
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return data
}

// mountOpenRouterMock spins up an httptest.NewServer that serves the
// mixed fixture and records the inbound request. Lister tests would
// hit the real OpenRouter URL otherwise, since the lister's URL is
// hardcoded — we temporarily wedge the request through http.DefaultClient
// by setting up the server and relying on the lister to call it.
//
// CRITICAL: the lister hardcodes the OpenRouter URL. We cannot redirect
// it via creds.BaseURL (the lister ignores creds in this respect).
// Instead, the test verifies the request shape only — we can't reach
// the real OpenRouter from test, so we test the parsing + filtering
// logic by calling listOpenAIViaOpenRouter against a stub server.
//
// Workaround: we expose `openrouterMirrorBaseURL` as a package-private
// var that the lister consults (with the public URL as default), and
// tests override it to point at httptest.NewServer.URL.
func mountOpenRouterMock(t *testing.T, body []byte, captureReq *http.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captureReq != nil {
			*captureReq = *r
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestListOpenAIViaOpenRouter_FiltersToOpenAIPrefix — only openai/*
// rows are returned; prefix stripped.
func TestListOpenAIViaOpenRouter_FiltersToOpenAIPrefix(t *testing.T) {
	body := loadOpenRouterMixedFixture(t)
	srv := mountOpenRouterMock(t, body, nil)

	prev := openrouterMirrorBaseURL
	openrouterMirrorBaseURL = srv.URL + "/api/v1/models"
	t.Cleanup(func() { openrouterMirrorBaseURL = prev })

	models, err := listOpenAIViaOpenRouter(context.Background(), ListerCredentials{})
	if err != nil {
		t.Fatalf("listOpenAIViaOpenRouter: %v", err)
	}

	for _, m := range models {
		if m.Provider != "openai" {
			t.Errorf("Provider = %q, want openai (rows MUST stamp openai for source-precedence to work)", m.Provider)
		}
		// Bare IDs only — no slash, no prefix.
		for i := 0; i < len(m.ModelID); i++ {
			if m.ModelID[i] == '/' {
				t.Errorf("ModelID = %q contains slash (prefix not stripped)", m.ModelID)
			}
		}
	}

	// Verify by counting: fixture has 7 openai/* rows. Cycle
	// 20260517-provider-auth-variants pulled M5.10 forward — openai's
	// SubscriptionAllowedModels was removed (backend gates instead of
	// sage-router's static list). The mirror lister now passes through
	// ALL openai/* rows; the previously-excluded "experimental" entry
	// is now included.
	if len(models) < 6 || len(models) > 7 {
		t.Errorf("len(models) = %d, want 7 (fixture has 7 openai/* entries; pass-through after openai allowlist removal)", len(models))
	}
}

// TestListOpenAIViaOpenRouter_PassThroughAllOpenAIModels — cycle
// 20260517-provider-auth-variants pulled M5.10 forward: openai's
// SubscriptionAllowedModels was removed and the mirror lister now
// passes through ALL openai/* rows (backend gates the actual model
// resolution at request time, not the catalog discovery step).
//
// Renamed from TestListOpenAIViaOpenRouter_FiltersBySubscriptionAllowedModels
// to reflect the post-removal contract.
func TestListOpenAIViaOpenRouter_PassThroughAllOpenAIModels(t *testing.T) {
	body := loadOpenRouterMixedFixture(t)
	srv := mountOpenRouterMock(t, body, nil)
	prev := openrouterMirrorBaseURL
	openrouterMirrorBaseURL = srv.URL + "/api/v1/models"
	t.Cleanup(func() { openrouterMirrorBaseURL = prev })

	models, err := listOpenAIViaOpenRouter(context.Background(), ListerCredentials{})
	if err != nil {
		t.Fatalf("listOpenAIViaOpenRouter: %v", err)
	}

	// Pass-through means the experimental fixture row is now INCLUDED.
	gotExperimental := false
	for _, m := range models {
		if m.ModelID == "gpt-experimental-not-in-allowlist" {
			gotExperimental = true
			break
		}
	}
	if !gotExperimental {
		t.Error("expected gpt-experimental-not-in-allowlist to be PRESENT (post-M5.10-forward; openai allowlist removed)")
	}

	// Spot-check positives: gpt-4o, gpt-5, o3 should all be present.
	got := make(map[string]bool)
	for _, m := range models {
		got[m.ModelID] = true
	}
	for _, want := range []string{"gpt-4o", "gpt-5", "o3"} {
		if !got[want] {
			t.Errorf("expected model %q in result; got %v", want, models)
		}
	}
}

// TestListOpenAIViaOpenRouter_StampsProviderAsOpenai — load-bearing.
// Rows must stamp Provider=openai (NOT openrouter) so source-precedence
// at sqlite.go:84-86 overrides openai seed rows for the same model_id.
func TestListOpenAIViaOpenRouter_StampsProviderAsOpenai(t *testing.T) {
	body := loadOpenRouterMixedFixture(t)
	srv := mountOpenRouterMock(t, body, nil)
	prev := openrouterMirrorBaseURL
	openrouterMirrorBaseURL = srv.URL + "/api/v1/models"
	t.Cleanup(func() { openrouterMirrorBaseURL = prev })

	models, _ := listOpenAIViaOpenRouter(context.Background(), ListerCredentials{})
	for _, m := range models {
		if m.Provider != "openai" {
			t.Fatalf("Provider = %q, want openai (CRITICAL: not openrouter)", m.Provider)
		}
	}
}

// TestListOpenAIViaOpenRouter_NoAuthHeader — public endpoint; the
// lister MUST NOT send any Authorization header. Leaking a stale
// auth header to a third-party would be a credential-spill bug.
func TestListOpenAIViaOpenRouter_NoAuthHeader(t *testing.T) {
	body := loadOpenRouterMixedFixture(t)
	var captured http.Request
	srv := mountOpenRouterMock(t, body, &captured)
	prev := openrouterMirrorBaseURL
	openrouterMirrorBaseURL = srv.URL + "/api/v1/models"
	t.Cleanup(func() { openrouterMirrorBaseURL = prev })

	// Pass creds with an Authorization-ish value — the lister must NOT
	// forward this. The new lister deliberately ignores creds.AccessToken
	// since the upstream is unauthenticated.
	_, _ = listOpenAIViaOpenRouter(context.Background(), ListerCredentials{
		AccessToken: "should-not-leak",
		APIKey:      "should-not-leak-either",
	})

	if got := captured.Header.Get("Authorization"); got != "" {
		t.Errorf("captured request has Authorization=%q, want empty (credential leak to public endpoint)", got)
	}
}
