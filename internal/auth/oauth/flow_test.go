package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
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
	// Scopes joined with space. Must match the Codex CLI canonical
	// scope string byte-for-byte — the 4 core OIDC scopes plus the
	// 2 connectors scopes that OpenAI's authorization server requires
	// for this client_id (server.rs:495 in openai/codex).
	const wantScope = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	if got := q.Get("scope"); got != wantScope {
		t.Errorf("scope = %q, want %q", got, wantScope)
	}
	// ExtraAuthParams from the registry, mirroring Codex CLI's
	// authorize URL (server.rs:504-508). All three are load-bearing:
	// - codex_cli_simplified_flow: skips an interstitial Codex consent
	// - id_token_add_organizations: enables the chatgpt_account_id
	//   claim in the returned id_token (AccountIDClaim path)
	// - originator: client identifier expected by OpenAI's auth server
	wantsExtra := map[string]string{
		"codex_cli_simplified_flow":  "true",
		"id_token_add_organizations": "true",
		"originator":                 "codex_cli_rs",
	}
	for k, want := range wantsExtra {
		if got := q.Get(k); got != want {
			t.Errorf("query[%s] = %q, want %q", k, got, want)
		}
	}
}

// TestFlow_AuthorizeURL_OpenAI_GoldenFixture pins the FULL sorted
// query-string shape of the OpenAI authorize URL against the canonical
// Codex CLI form. Any future drift from upstream (added/removed/renamed
// param, changed scope ordering) fails this test loudly, with a clear
// side-by-side diff in the error message.
//
// This is the preventive control that would have caught the original
// "Lỗi xác thực" bug — sage-router's authorize URL had drifted from
// Codex CLI upstream silently, and the only way to discover it was a
// user-screenshot in production.
//
// Update by refreshing the golden string after re-fetching upstream
// (openai/codex codex-rs/login/src/server.rs) and confirming the new
// param set is correct.
func TestFlow_AuthorizeURL_OpenAI_GoldenFixture(t *testing.T) {
	f, err := NewFlow("openai", "Work")
	if err != nil {
		t.Fatalf("NewFlow: %v", err)
	}
	const redirectURI = "http://localhost:1455/auth/callback"
	authURL := f.AuthorizeURL(redirectURI)

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Sorted query params (excluding the dynamic state + code_challenge
	// which carry per-flow randomness). The remaining params must
	// match upstream byte-for-byte.
	got := u.Query()
	got.Del("state")
	got.Del("code_challenge")

	// Sort keys for deterministic comparison.
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type kv struct{ k, v string }
	var pairs []kv
	for _, k := range keys {
		pairs = append(pairs, kv{k, got.Get(k)})
	}
	wantPairs := []kv{
		{"client_id", "app_EMoamEEZ73f0CkXaXp7hrann"},
		{"code_challenge_method", "S256"},
		{"codex_cli_simplified_flow", "true"},
		{"id_token_add_organizations", "true"},
		{"originator", "codex_cli_rs"},
		{"redirect_uri", redirectURI},
		{"response_type", "code"},
		{"scope", "openid profile email offline_access api.connectors.read api.connectors.invoke"},
	}
	if len(pairs) != len(wantPairs) {
		t.Fatalf("got %d params (after stripping state+challenge), want %d.\n  got: %+v\n  want: %+v",
			len(pairs), len(wantPairs), pairs, wantPairs)
	}
	for i := range pairs {
		if pairs[i] != wantPairs[i] {
			t.Errorf("param %d: got %+v, want %+v", i, pairs[i], wantPairs[i])
		}
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
	//
	// NOTE: OpenAI provider auto-chains into ExchangeForAPIKey after the
	// authorization_code grant (RequiresAPIKeyExchange=true). The handler
	// below differentiates by grant_type and only records the first
	// (auth-code) form for assertion. The chained token-exchange call is
	// covered separately by TestFlow_Exchange_AutoChainsAPIKeyExchange_OpenAI.
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Errorf("Content-Type = %q, want form", ct)
		}
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))

		w.Header().Set("Content-Type", "application/json")
		switch form.Get("grant_type") {
		case "authorization_code":
			gotForm = form
			_, _ = w.Write([]byte(`{
				"access_token": "atk-abc",
				"refresh_token": "rtk-xyz",
				"id_token": "jwt-blob",
				"expires_in": 3600,
				"token_type": "Bearer"
			}`))
		case "urn:ietf:params:oauth:grant-type:token-exchange":
			// Auto-chained step for openai. Return any non-empty access_token —
			// the assertions in this test only inspect the auth-code form.
			_, _ = w.Write([]byte(`{"access_token":"exchanged-for-test"}`))
		default:
			t.Errorf("unexpected grant_type=%q", form.Get("grant_type"))
			w.WriteHeader(http.StatusBadRequest)
		}
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

// TestFlow_Exchange_AnthropicSendsJSON pins the Anthropic-specific token
// exchange contract: platform.claude.com/v1/oauth/token requires
// Content-Type: application/json with a JSON body, not the RFC 6749
// default form-urlencoded shape used by OpenAI.
//
// Root cause for this fix (M0.8 of cycle 20260517-provider-auth-variants):
// PKCE login was failing with `oauth bridge: exchange failed → provider
// returned 400: {"error":{"type":"invalid_request_error","message":
// "Invalid request format"}}` (request_id req_011Cb86cWhWMtdUsmAK4cLRc).
// Live curl + opencode-anthropic-auth plugin source comparison showed
// the plugin (which works) sends JSON; sage-router was sending form-encoded.
//
// Field set mirrors the plugin's auth.ts::exchangeCode (verbatim):
//   code, state, grant_type, client_id, redirect_uri, code_verifier
// `state` is the only addition over the OpenAI form body — defensive,
// matching the plugin's body shape.
func TestFlow_Exchange_AnthropicSendsJSON(t *testing.T) {
	var gotContentType string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("body is not JSON: %v (raw=%s)", err, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "anthropic-atk",
			"refresh_token": "anthropic-rtk",
			"expires_in": 3600,
			"token_type": "Bearer"
		}`))
	}))
	defer srv.Close()

	f, err := NewFlow("anthropic", "Claude")
	if err != nil {
		t.Fatal(err)
	}
	f.tokenURL = srv.URL

	resp, err := f.Exchange(context.Background(), "the-code", "http://localhost:53692/callback")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}

	// The 6 fields the plugin sends, verbatim.
	wantBody := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "the-code",
		"state":         f.State,
		"client_id":     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		"redirect_uri":  "http://localhost:53692/callback",
		"code_verifier": f.Verifier,
	}
	if len(gotBody) != len(wantBody) {
		t.Errorf("body field count = %d, want %d (got=%v)", len(gotBody), len(wantBody), gotBody)
	}
	for k, v := range wantBody {
		if got := gotBody[k]; got != v {
			t.Errorf("body[%s] = %q, want %q", k, got, v)
		}
	}

	if resp.AccessToken != "anthropic-atk" {
		t.Errorf("AccessToken = %q, want anthropic-atk", resp.AccessToken)
	}
	if resp.RefreshToken != "anthropic-rtk" {
		t.Errorf("RefreshToken = %q, want anthropic-rtk", resp.RefreshToken)
	}
}

// TestFlow_Exchange_OpenAISendsForm is a regression pin for the existing
// form-encoded path on the OpenAI exchange. Pairs with the JSON test
// above so a future refactor that flips OpenAI's TokenRequestFormat
// fails loudly here, not silently in production.
func TestFlow_Exchange_OpenAISendsForm(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		// Handle both grant types — the auth-code branch + the chained
		// token-exchange that OpenAI's RequiresAPIKeyExchange triggers.
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		switch form.Get("grant_type") {
		case "authorization_code":
			_, _ = w.Write([]byte(`{"access_token":"atk","refresh_token":"rtk","id_token":"jwt","expires_in":3600}`))
		case "urn:ietf:params:oauth:grant-type:token-exchange":
			_, _ = w.Write([]byte(`{"access_token":"exchanged"}`))
		}
	}))
	defer srv.Close()

	f, _ := NewFlow("openai", "Work")
	f.tokenURL = srv.URL
	if _, err := f.Exchange(context.Background(), "the-code", "http://localhost:1455/auth/callback"); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !strings.HasPrefix(gotContentType, "application/x-www-form-urlencoded") {
		t.Errorf("openai Content-Type = %q, want form-urlencoded (regression)", gotContentType)
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


// 6 token-exchange + auto-chain tests deleted in cycle
// 20260517-provider-auth-variants M2.6.6 — Flow.ExchangeForAPIKey,
// Flow.Exchange auto-chain, TokenResponse.ExchangedToken, and the
// ToCredential ExchangedToken pass-through were all removed at M2.6.2
// + M2.6.3 (the RFC 8693 chain was wrong-path per memory `f32bbc73`).
