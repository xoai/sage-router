package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sage-router/internal/auth"
	"sage-router/internal/translate/openai-responses"
	"sage-router/pkg/canonical"
)

// codexBearerPrefix is a known prefix on Codex subscription PKCE access tokens
// (`sk-ant-...` shape for anthropic, `oai-...` for openai — the predecessor
// cycle's live captures show OpenAI uses a long base64-ish token). For our
// tests, any non-empty access token shape works.
const codexTestAccessToken = "oai-pkce-fake-access-token-for-tests"
const codexTestAccountID = "443dac56-54fa-43a5-8764-fake-account"

// helperCodexCreds returns a Credentials value populated with the minimum
// fields a CodexSubscriptionExecutor request needs. ExtraHeaders MUST
// include chatgpt-account-id — the executor returns an error if missing.
func helperCodexCreds() *Credentials {
	return &Credentials{
		ConnectionID: "test-conn",
		AuthType:     auth.AuthTypeSubscription,
		AccessToken:  codexTestAccessToken,
		ExtraHeaders: map[string]string{
			"chatgpt-account-id": codexTestAccountID,
		},
	}
}

// TestCodexSubscription_GoldenURL — outbound request hits the codex backend
// path (NOT api.openai.com/v1/responses). Pin via httptest.NewServer that
// captures r.URL.Path.
func TestCodexSubscription_GoldenURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := newCodexSubscriptionExecutorForTest(srv.URL, NewClientPool())
	_, err := e.Execute(context.Background(), &ExecuteRequest{
		Model:       "gpt-5.4",
		Body:        []byte(`{"model":"gpt-5.4"}`),
		Credentials: helperCodexCreds(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotPath != "/codex/responses" {
		t.Errorf("upstream path = %q, want /codex/responses", gotPath)
	}
}

// TestCodexSubscription_GoldenHeaders — pin the 5 required headers
// (Content-Type, Authorization, chatgpt-account-id, OpenAI-Beta, originator).
// Per ADR-codex-subscription-contract.md.
func TestCodexSubscription_GoldenHeaders(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := newCodexSubscriptionExecutorForTest(srv.URL, NewClientPool())
	if _, err := e.Execute(context.Background(), &ExecuteRequest{
		Model:       "gpt-5.4",
		Body:        []byte(`{"model":"gpt-5.4"}`),
		Credentials: helperCodexCreds(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	want := map[string]string{
		"Content-Type":       "application/json",
		"Authorization":      "Bearer " + codexTestAccessToken,
		"Chatgpt-Account-Id": codexTestAccountID,
		"Openai-Beta":        "responses=experimental",
		"Originator":         "codex_cli_rs",
	}
	for k, v := range want {
		if got := gotHeaders.Get(k); got != v {
			t.Errorf("header[%s] = %q, want %q", k, got, v)
		}
	}
}

// TestCodexSubscription_MissingAccessToken — empty AccessToken returns an
// explicit error BEFORE any HTTP call (no roundtrip to upstream).
func TestCodexSubscription_MissingAccessToken(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := newCodexSubscriptionExecutorForTest(srv.URL, NewClientPool())
	creds := helperCodexCreds()
	creds.AccessToken = ""
	_, err := e.Execute(context.Background(), &ExecuteRequest{
		Model: "gpt-5.4", Body: []byte(`{}`), Credentials: creds,
	})
	if err == nil {
		t.Fatal("Execute returned nil error; want missing-access-token error")
	}
	if !strings.Contains(err.Error(), "access_token") && !strings.Contains(err.Error(), "AccessToken") {
		t.Errorf("error message %q does not mention access_token", err.Error())
	}
	if called {
		t.Error("upstream was called despite missing access token")
	}
}

// TestCodexSubscription_MissingAccountIDHeader — ExtraHeaders without
// chatgpt-account-id returns an explicit error BEFORE upstream call.
func TestCodexSubscription_MissingAccountIDHeader(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := newCodexSubscriptionExecutorForTest(srv.URL, NewClientPool())
	creds := helperCodexCreds()
	creds.ExtraHeaders = map[string]string{} // empty — no chatgpt-account-id
	_, err := e.Execute(context.Background(), &ExecuteRequest{
		Model: "gpt-5.4", Body: []byte(`{}`), Credentials: creds,
	})
	if err == nil {
		t.Fatal("Execute returned nil error; want missing-account-id error")
	}
	if !strings.Contains(err.Error(), "chatgpt-account-id") {
		t.Errorf("error message %q does not mention chatgpt-account-id", err.Error())
	}
	if called {
		t.Error("upstream was called despite missing chatgpt-account-id")
	}
}

// TestCodexSubscription_FormatReturnsResponses — variant declares
// canonical.FormatResponses (Responses API body shape) so the routing
// layer's formatOf() helper returns the right format for translation.
func TestCodexSubscription_FormatReturnsResponses(t *testing.T) {
	e := NewCodexSubscriptionExecutor(NewClientPool())
	if got := e.Format(); got != canonical.FormatResponses {
		t.Errorf("Format() = %q, want %q", got, canonical.FormatResponses)
	}
}

// TestCodexSubscription_ParseAuthError_TierError — 401 + body containing
// "Missing scopes: api.responses.write" returns ErrTierMissingScopes
// (defensive — the api.openai.com error shape; M0.8 live test against
// chatgpt.com/backend-api didn't surface this, but the sniff stays so
// future tier-limit surfaces map to a friendly LastError).
func TestCodexSubscription_ParseAuthError_TierError(t *testing.T) {
	e := NewCodexSubscriptionExecutor(NewClientPool())
	body := []byte(`{"error":{"message":"Missing scopes: api.responses.write"}}`)
	err := e.ParseAuthError(401, body)
	if !errors.Is(err, openairesp.ErrTierMissingScopes) {
		t.Errorf("ParseAuthError(401, tier-error body) = %v, want ErrTierMissingScopes", err)
	}
}

// TestCodexSubscription_ParseAuthError_OtherCases — non-401 statuses and
// non-tier 401 bodies return nil; the caller proceeds with default error
// handling.
func TestCodexSubscription_ParseAuthError_OtherCases(t *testing.T) {
	e := NewCodexSubscriptionExecutor(NewClientPool())
	cases := []struct {
		name   string
		status int
		body   []byte
	}{
		{"200", 200, []byte(`{}`)},
		{"403", 403, []byte(`{"error":"forbidden"}`)},
		{"401 without tier-error string", 401, []byte(`{"error":"token expired"}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := e.ParseAuthError(c.status, c.body); err != nil {
				t.Errorf("ParseAuthError(%d, ...) = %v, want nil", c.status, err)
			}
		})
	}
}

// TestCodexSubscription_PreflightCredentials_EmptyAccess — empty AccessToken
// returns ErrTierMissingScopes; the route handler skips the upstream call
// and transitions the connection to AuthExpired immediately (saves a
// roundtrip and surfaces the friendly message faster).
func TestCodexSubscription_PreflightCredentials_EmptyAccess(t *testing.T) {
	e := NewCodexSubscriptionExecutor(NewClientPool())
	err := e.PreflightCredentials(&Credentials{AccessToken: ""})
	if !errors.Is(err, openairesp.ErrTierMissingScopes) {
		t.Errorf("PreflightCredentials(empty) = %v, want ErrTierMissingScopes", err)
	}
}

// TestCodexSubscription_PreflightCredentials_PopulatedAccess — non-empty
// AccessToken passes the preflight; upstream call proceeds.
func TestCodexSubscription_PreflightCredentials_PopulatedAccess(t *testing.T) {
	e := NewCodexSubscriptionExecutor(NewClientPool())
	if err := e.PreflightCredentials(helperCodexCreds()); err != nil {
		t.Errorf("PreflightCredentials(valid) = %v, want nil", err)
	}
}

// TestCodexSubscription_ProviderID — Provider() returns "openai" so the
// variant fits the existing provider-keyed registry conventions (smart
// router metadata, catalog dispatch, etc.).
func TestCodexSubscription_ProviderID(t *testing.T) {
	e := NewCodexSubscriptionExecutor(NewClientPool())
	if got := e.Provider(); got != "openai" {
		t.Errorf("Provider() = %q, want openai", got)
	}
}

// TestCodexSubscription_HeaderIsolation (M-R2 review fold) — concurrent
// requests across CodexSubscriptionExecutor + DefaultExecutor (openai
// apikey) sharing one ClientPool do NOT leak codex-specific headers
// (chatgpt-account-id, OpenAI-Beta, originator) into the apikey request.
//
// Memory `1ad0007d` pattern adapted: each request builds its own
// http.Request; cross-pollution would only happen if a shared transport
// pre-populated headers, which Go's http.Transport does NOT do. This
// test pins that property at the executor level.
func TestCodexSubscription_HeaderIsolation(t *testing.T) {
	var capturedHeaders []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = append(capturedHeaders, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	pool := NewClientPool()
	codex := newCodexSubscriptionExecutorForTest(srv.URL, pool)
	apikey := NewDefaultExecutor("openai", srv.URL, pool)

	// Fire codex first.
	if _, err := codex.Execute(context.Background(), &ExecuteRequest{
		Body: []byte(`{}`), Credentials: helperCodexCreds(),
	}); err != nil {
		t.Fatalf("codex Execute: %v", err)
	}
	// Fire apikey next, sharing the same pool.
	apikeyCreds := &Credentials{
		ConnectionID: "apikey-conn",
		AuthType:     auth.AuthTypeAPIKey,
		APIKey:       "sk-test-apikey",
	}
	if _, err := apikey.Execute(context.Background(), &ExecuteRequest{
		Body: []byte(`{}`), Credentials: apikeyCreds,
	}); err != nil {
		t.Fatalf("apikey Execute: %v", err)
	}

	if len(capturedHeaders) != 2 {
		t.Fatalf("captured %d requests, want 2", len(capturedHeaders))
	}
	apikeyReq := capturedHeaders[1]
	forbidden := []string{"Chatgpt-Account-Id", "Openai-Beta", "Originator"}
	for _, h := range forbidden {
		if v := apikeyReq.Get(h); v != "" {
			t.Errorf("apikey request leaked codex header %q = %q", h, v)
		}
	}
	if got := apikeyReq.Get("Authorization"); got != "Bearer sk-test-apikey" {
		t.Errorf("apikey Authorization = %q, want Bearer sk-test-apikey", got)
	}
}
