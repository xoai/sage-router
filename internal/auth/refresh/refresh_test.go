package refresh

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"sage-router/internal/auth"
)

// withOverride redirects refresh for `provider` at `mockURL` for the duration
// of the test, then cleans up.
func withOverride(t *testing.T, provider, mockURL string) {
	t.Helper()
	tokenURLOverrides[provider] = mockURL
	t.Cleanup(func() { delete(tokenURLOverrides, provider) })
}

func TestRefresh_NilCredentialErrors(t *testing.T) {
	if _, err := Refresh(context.Background(), nil); err == nil {
		t.Fatal("expected error on nil credential")
	}
}

func TestRefresh_UnsupportedProviderErrors(t *testing.T) {
	cred := &auth.Credential{Provider: "weird", RefreshToken: "rtk"}
	_, err := Refresh(context.Background(), cred)
	if err == nil || !errors.Is(err, ErrUnsupportedProvider) {
		t.Errorf("want errors.Is(err, ErrUnsupportedProvider); got %v", err)
	}
}

func TestRefresh_EmptyRefreshTokenErrors(t *testing.T) {
	cred := &auth.Credential{Provider: "openai", RefreshToken: ""}
	if _, err := Refresh(context.Background(), cred); err == nil {
		t.Error("expected error when refresh_token is empty")
	}
}

// ── OAuth-form providers (OpenAI/Anthropic/Gemini) ──

// formProviderCases drives the same shared-impl test against each of the
// three OAuth-form providers so any provider-specific config bug surfaces.
type formProviderCase struct {
	provider     string
	wantClientID string // empty means "don't assert"
}

var formProviderCases = []formProviderCase{
	{provider: "openai", wantClientID: "app_EMoamEEZ73f0CkXaXp7hrann"},
	{provider: "anthropic", wantClientID: "9d1c250a-e61b-44d9-88ed-5944d1962f5e"},
	{provider: "gemini", wantClientID: ""},
}

func TestRefresh_OAuthForm_HappyPath(t *testing.T) {
	for _, tc := range formProviderCases {
		t.Run(tc.provider, func(t *testing.T) {
			var gotForm url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				body, _ := io.ReadAll(r.Body)
				gotForm, _ = url.ParseQuery(string(body))
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{
					"access_token": "new-atk",
					"refresh_token": "new-rtk",
					"expires_in": 3600
				}`))
			}))
			defer srv.Close()
			withOverride(t, tc.provider, srv.URL)

			cred := &auth.Credential{
				Provider:     tc.provider,
				ConnectionID: "c1",
				AccessToken:  "old-atk",
				RefreshToken: "old-rtk",
				AccountID:    "acct-abc",
				ExtraData:    map[string]any{"foo": "bar"},
			}
			fresh, err := Refresh(context.Background(), cred)
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			if gotForm.Get("grant_type") != "refresh_token" {
				t.Errorf("grant_type = %q, want refresh_token", gotForm.Get("grant_type"))
			}
			if gotForm.Get("refresh_token") != "old-rtk" {
				t.Errorf("refresh_token sent = %q, want old-rtk", gotForm.Get("refresh_token"))
			}
			if tc.wantClientID != "" && gotForm.Get("client_id") != tc.wantClientID {
				t.Errorf("client_id = %q, want %q", gotForm.Get("client_id"), tc.wantClientID)
			}

			if fresh.AccessToken != "new-atk" {
				t.Errorf("AccessToken = %q, want new-atk", fresh.AccessToken)
			}
			if fresh.RefreshToken != "new-rtk" {
				t.Errorf("RefreshToken = %q, want new-rtk", fresh.RefreshToken)
			}
			// Preserved fields from the input cred.
			if fresh.ConnectionID != "c1" || fresh.AccountID != "acct-abc" {
				t.Errorf("preserved fields lost: %+v", fresh)
			}
			if fresh.ExtraData["foo"] != "bar" {
				t.Errorf("ExtraData lost: %+v", fresh.ExtraData)
			}
			// Expires_at = now + 3600 - 300 buffer = now + 3300s, ±jitter.
			delta := time.Until(fresh.ExpiresAt)
			if delta < 3290*time.Second || delta > 3310*time.Second {
				t.Errorf("ExpiresAt delta = %v, want ~3300s", delta)
			}
		})
	}
}

func TestRefresh_OAuthForm_InvalidGrantReturnsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token expired"}`))
	}))
	defer srv.Close()
	withOverride(t, "openai", srv.URL)

	cred := &auth.Credential{Provider: "openai", RefreshToken: "revoked"}
	_, err := Refresh(context.Background(), cred)
	if !errors.Is(err, ErrRefreshTokenRevoked) {
		t.Errorf("want errors.Is(err, ErrRefreshTokenRevoked); got %v", err)
	}
}

func TestRefresh_OAuthForm_OtherErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`upstream busy`))
	}))
	defer srv.Close()
	withOverride(t, "openai", srv.URL)

	cred := &auth.Credential{Provider: "openai", RefreshToken: "rtk"}
	_, err := Refresh(context.Background(), cred)
	if err == nil {
		t.Fatal("expected error on 503")
	}
	if errors.Is(err, ErrRefreshTokenRevoked) {
		t.Errorf("503 should NOT be classified as revoked; got %v", err)
	}
}

func TestRefresh_OAuthForm_PreservesRefreshTokenWhenUnrotated(t *testing.T) {
	// Some providers omit refresh_token on refresh; we should keep the
	// previous one rather than blanking the field.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"new-atk","expires_in":3600}`))
	}))
	defer srv.Close()
	withOverride(t, "openai", srv.URL)

	cred := &auth.Credential{Provider: "openai", RefreshToken: "kept-rtk"}
	fresh, err := Refresh(context.Background(), cred)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if fresh.RefreshToken != "kept-rtk" {
		t.Errorf("RefreshToken = %q, want kept-rtk (preserved when provider doesn't rotate)", fresh.RefreshToken)
	}
}

func TestRefresh_OAuthForm_EmptyAccessTokenErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server returns 200 but no access_token — pathological provider bug.
		w.Write([]byte(`{"expires_in":3600}`))
	}))
	defer srv.Close()
	withOverride(t, "openai", srv.URL)

	cred := &auth.Credential{Provider: "openai", RefreshToken: "rtk"}
	if _, err := Refresh(context.Background(), cred); err == nil {
		t.Error("expected error when access_token missing from 200 response")
	}
}

// ── Copilot two-step refresh ──

func TestRefresh_Copilot_HappyPath(t *testing.T) {
	expiresAt := time.Now().Add(1 * time.Hour).Unix()
	var gotAuthHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		gotAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"copilot-bearer-xyz","expires_at":` +
			strings.NewReplacer().Replace(strconvI64(expiresAt)) + `}`))
	}))
	defer srv.Close()
	withOverride(t, "github-copilot", srv.URL)

	cred := &auth.Credential{
		Provider:     "github-copilot",
		ConnectionID: "c1",
		AccessToken:  "stale-copilot-token",
		RefreshToken: "gh-oauth-token",
	}
	fresh, err := Refresh(context.Background(), cred)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if !strings.HasPrefix(gotAuthHeader, "token ") {
		t.Errorf("Authorization header should start with 'token '; got %q", gotAuthHeader)
	}
	if !strings.Contains(gotAuthHeader, "gh-oauth-token") {
		t.Errorf("Authorization header should contain GH token; got %q", gotAuthHeader)
	}
	if fresh.AccessToken != "copilot-bearer-xyz" {
		t.Errorf("AccessToken = %q, want copilot-bearer-xyz", fresh.AccessToken)
	}
	if fresh.RefreshToken != "gh-oauth-token" {
		t.Errorf("RefreshToken should be preserved (GH token); got %q", fresh.RefreshToken)
	}
}

func TestRefresh_Copilot_401IsRevoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()
	withOverride(t, "github-copilot", srv.URL)

	cred := &auth.Credential{Provider: "github-copilot", RefreshToken: "dead-gh-token"}
	_, err := Refresh(context.Background(), cred)
	if !errors.Is(err, ErrRefreshTokenRevoked) {
		t.Errorf("want errors.Is(err, ErrRefreshTokenRevoked); got %v", err)
	}
}

func TestRefresh_Copilot_5xxIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	withOverride(t, "github-copilot", srv.URL)

	cred := &auth.Credential{Provider: "github-copilot", RefreshToken: "gh-tk"}
	_, err := Refresh(context.Background(), cred)
	if err == nil {
		t.Fatal("expected error on 502")
	}
	if errors.Is(err, ErrRefreshTokenRevoked) {
		t.Errorf("502 should NOT be classified as revoked; got %v", err)
	}
}

func TestRefresh_Copilot_EmptyTokenInResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"expires_at":9999999999}`))
	}))
	defer srv.Close()
	withOverride(t, "github-copilot", srv.URL)

	cred := &auth.Credential{Provider: "github-copilot", RefreshToken: "gh-tk"}
	if _, err := Refresh(context.Background(), cred); err == nil {
		t.Error("expected error when token missing from response")
	}
}

// strconvI64 avoids importing strconv just for this test (the existing
// pattern in the rest of this codebase is to keep imports tight).
func strconvI64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
