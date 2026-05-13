package oauth

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewFlow_PopulatesAllFields(t *testing.T) {
	f, err := NewFlow("openai", "Work")
	if err != nil {
		t.Fatalf("NewFlow: %v", err)
	}
	if f.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", f.Provider)
	}
	if f.ConnName != "Work" {
		t.Errorf("ConnName = %q, want Work", f.ConnName)
	}
	if len(f.Verifier) != 43 {
		t.Errorf("Verifier len = %d, want 43", len(f.Verifier))
	}
	if f.Challenge != GenerateChallenge(f.Verifier) {
		t.Errorf("Challenge does not match verifier")
	}
	if len(f.State) != 32 {
		t.Errorf("State len = %d, want 32", len(f.State))
	}
	if f.CreatedAt.IsZero() {
		t.Error("CreatedAt zero")
	}
}

func TestNewFlow_UnknownProviderErrors(t *testing.T) {
	if _, err := NewFlow("not-a-provider", "Work"); err == nil {
		t.Error("NewFlow with unknown provider should error")
	}
}

func TestFlow_AuthorizeURL_HasRequiredParams(t *testing.T) {
	f, err := NewFlow("openai", "Work")
	if err != nil {
		t.Fatalf("NewFlow: %v", err)
	}
	redirectURI := "http://localhost:1455/auth/callback"
	authURL := f.AuthorizeURL(redirectURI)

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	if !strings.HasPrefix(authURL, "https://auth.openai.com/oauth/authorize") {
		t.Errorf("authorize URL has wrong base: %s", authURL)
	}
	q := u.Query()

	wants := map[string]string{
		"response_type":         "code",
		"client_id":             "app_EMoamEEZ73f0CkXaXp7hrann",
		"redirect_uri":          redirectURI,
		"code_challenge":        f.Challenge,
		"code_challenge_method": "S256",
		"state":                 f.State,
	}
	for k, want := range wants {
		if got := q.Get(k); got != want {
			t.Errorf("query[%s] = %q, want %q", k, got, want)
		}
	}
	// Scopes joined with space.
	if got := q.Get("scope"); got != "openid profile email offline_access" {
		t.Errorf("scope = %q, want %q", got, "openid profile email offline_access")
	}
	// Extra param from registry.
	if got := q.Get("codex_cli_simplified_flow"); got != "true" {
		t.Errorf("codex_cli_simplified_flow = %q, want true", got)
	}
}

func TestFlow_AuthorizeURL_Anthropic(t *testing.T) {
	f, _ := NewFlow("anthropic", "Personal")
	authURL := f.AuthorizeURL("http://localhost:53692/callback")
	if !strings.HasPrefix(authURL, "https://claude.ai/oauth/authorize") {
		t.Errorf("anthropic URL has wrong base: %s", authURL)
	}
	u, _ := url.Parse(authURL)
	if got := u.Query().Get("client_id"); got != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
		t.Errorf("anthropic client_id = %q, want %q", got, "9d1c250a-e61b-44d9-88ed-5944d1962f5e")
	}
}

func TestFlow_Exchange_HappyPath(t *testing.T) {
	// Mock token endpoint: returns a well-formed token response when called
	// with the expected form fields. We verify the request body shape AND
	// the returned TokenResponse.
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Errorf("Content-Type = %q, want form", ct)
		}
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"access_token": "atk-abc",
			"refresh_token": "rtk-xyz",
			"id_token": "jwt-blob",
			"expires_in": 3600,
			"token_type": "Bearer"
		}`))
	}))
	defer srv.Close()

	f, err := NewFlow("openai", "Work")
	if err != nil {
		t.Fatal(err)
	}
	// Point flow at the mock server.
	f.tokenURL = srv.URL

	resp, err := f.Exchange(context.Background(), "the-code", "http://localhost:1455/auth/callback")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	// Verify the form fields we sent upstream.
	want := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "the-code",
		"client_id":     "app_EMoamEEZ73f0CkXaXp7hrann",
		"code_verifier": f.Verifier,
		"redirect_uri":  "http://localhost:1455/auth/callback",
	}
	for k, v := range want {
		if got := gotForm.Get(k); got != v {
			t.Errorf("form[%s] = %q, want %q", k, got, v)
		}
	}

	// Verify the parsed TokenResponse.
	if resp.AccessToken != "atk-abc" {
		t.Errorf("AccessToken = %q, want atk-abc", resp.AccessToken)
	}
	if resp.RefreshToken != "rtk-xyz" {
		t.Errorf("RefreshToken = %q, want rtk-xyz", resp.RefreshToken)
	}
	if resp.IDToken != "jwt-blob" {
		t.Errorf("IDToken = %q, want jwt-blob", resp.IDToken)
	}
	if resp.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn = %d, want 3600", resp.ExpiresIn)
	}
}

func TestFlow_Exchange_PropagatesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	f, _ := NewFlow("openai", "Work")
	f.tokenURL = srv.URL
	_, err := f.Exchange(context.Background(), "bad-code", "http://localhost:1455/auth/callback")
	if err == nil {
		t.Fatal("Exchange should error on 400 response")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error should mention status and upstream message; got %q", err.Error())
	}
}

func TestToCredential_ComputesExpiresAtWithBuffer(t *testing.T) {
	f, _ := NewFlow("openai", "Work")
	resp := &TokenResponse{
		AccessToken:  "atk",
		RefreshToken: "rtk",
		ExpiresIn:    3600,
	}
	before := time.Now()
	cred := f.ToCredential(resp)
	after := time.Now()

	if cred.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", cred.Provider)
	}
	if cred.AccessToken != "atk" || cred.RefreshToken != "rtk" {
		t.Errorf("tokens not copied: %+v", cred)
	}
	// ExpiresAt = now + 3600 - 300 = now + 3300s, allowing for test-run jitter.
	expected := before.Add(time.Duration(3300) * time.Second)
	if cred.ExpiresAt.Before(expected) || cred.ExpiresAt.After(after.Add(time.Duration(3300)*time.Second)) {
		t.Errorf("ExpiresAt out of expected window: got %v, want ~%v", cred.ExpiresAt, expected)
	}
}

func TestExtractIDTokenClaim_FromMockJWT(t *testing.T) {
	// Synthetic JWT: header.payload.signature where header/payload are
	// base64url-no-pad-encoded JSON. Signature isn't validated by our
	// extractor (we trust the user's own token).
	payload := `{"https://api.openai.com/auth.chatgpt_account_id":"acct-123","sub":"user-1"}`
	header := `{"alg":"RS256","typ":"JWT"}`
	jwt := b64u(header) + "." + b64u(payload) + "." + b64u(`signature-here`)

	got, err := ExtractIDTokenClaim(jwt, "https://api.openai.com/auth.chatgpt_account_id")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != "acct-123" {
		t.Errorf("claim = %q, want acct-123", got)
	}
}

func TestExtractIDTokenClaim_MissingClaimReturnsEmpty(t *testing.T) {
	payload := `{"sub":"user-1"}`
	header := `{"alg":"RS256","typ":"JWT"}`
	jwt := b64u(header) + "." + b64u(payload) + "." + b64u(`sig`)

	got, err := ExtractIDTokenClaim(jwt, "missing_claim")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != "" {
		t.Errorf("missing claim should return empty; got %q", got)
	}
}

func TestExtractIDTokenClaim_MalformedJWT(t *testing.T) {
	if _, err := ExtractIDTokenClaim("not-a-jwt", "any"); err == nil {
		t.Error("Extract on non-JWT should error")
	}
	if _, err := ExtractIDTokenClaim("a.b", "any"); err == nil {
		t.Error("Extract on 2-segment string should error")
	}
}

// b64u is a tiny helper for tests to keep JWT-fixture creation readable.
func b64u(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
