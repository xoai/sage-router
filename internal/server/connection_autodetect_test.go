package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/detect"
	"sage-router/internal/store"
)

// Connection auto-detect — initiative 20260513-openai-autodetect.
//
// Tests cover the create-time conversion at routes_api.go:238-272:
// user clicks the auto-detect button → frontend POSTs createConnection
// with auth_type=auto_detect → backend reads the local CLI's auth.json
// → populates tokens → CONVERTS auth_type to subscription → stores
// the row encrypted.
//
// These tests use newDiscoveryServer + postCreateConnection from
// on_create_discovery_test.go for the test rig. They additionally
// stub the filesystem (HOME / USERPROFILE / APPDATA / CODEX_HOME /
// wslDistros) so the create-time detect reads from a temp dir.

// stubCodexHomeForServerTest stubs every environment + global the
// codex / claude path-search code reads, so tests are hermetic
// against the developer's machine state. Returns the $CODEX_HOME
// temp directory so tests can drop fixtures there.
//
// Without this stub, on a Windows-with-WSL developer machine where
// the user has Codex CLI or Claude Code installed inside WSL, the
// detect functions would read the developer's REAL credentials via
// the hardcoded `\\wsl$\<distro>\home\<user>\.codex\auth.json` /
// `~/.claude/.credentials.json` paths regardless of HOME stubs.
// `detect.DisableWSLForTesting` clears the WSL distro probe list
// for the duration of the test.
func stubCodexHomeForServerTest(t *testing.T) string {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	t.Setenv("APPDATA", "")
	envHome := t.TempDir()
	t.Setenv("CODEX_HOME", envHome)
	// Disable Windows-from-WSL probing — the hardcoded \\wsl$ paths
	// bypass HOME/USERPROFILE stubbing.
	t.Cleanup(detect.DisableWSLForTesting())
	return envHome
}

// writeCodexAuthJSON writes a Codex auth.json fixture at $CODEX_HOME.
// Used by the happy/expired tests to set up the filesystem state.
func writeCodexAuthJSON(t *testing.T, codexHome, accessToken, refreshToken string, expiresUnixSec int64) {
	t.Helper()
	body := fmt.Sprintf(`{
		"tokens": {
			"access_token": %q,
			"refresh_token": %q,
			"id_token": "stub-id-token",
			"expires_at": %d
		}
	}`, accessToken, refreshToken, expiresUnixSec)
	path := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}

// readBackConnection reads a Connection out of the store by ID.
// Returns the stored row so tests can verify the conversion (auth_type,
// access_token, refresh_token).
func readBackConnection(t *testing.T, st store.Store, id string) *store.Connection {
	t.Helper()
	conn, err := st.GetConnection(id)
	if err != nil {
		t.Fatalf("GetConnection(%q): %v", id, err)
	}
	if conn == nil {
		t.Fatalf("GetConnection(%q): nil row", id)
	}
	return conn
}

// postCreateConnectionWithBody is like postCreateConnection but also
// returns the response body so error-path tests can verify the
// surfaced message text.
func postCreateConnectionWithBody(t *testing.T, srv *Server, conn store.Connection) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(conn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/connections", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleCreateConnection(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// readBackConnectionID extracts the "id" field from a successful
// 201 Created response body.
func readBackConnectionID(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%q)", err, body)
	}
	if resp.ID == "" {
		t.Fatalf("response missing id (body=%q)", body)
	}
	return resp.ID
}

// TestCreateConnection_OpenAI_AutoDetect_ReadsCodexFile — happy path.
// Set $CODEX_HOME to a temp dir with a valid auth.json; POST with
// {provider: "openai", auth_type: "auto_detect"}; assert 201 +
// stored connection has auth_type=subscription with populated tokens.
// Per AC-AD-7 (CORRECTED): create-time read + conversion to subscription.
func TestCreateConnection_OpenAI_AutoDetect_ReadsCodexFile(t *testing.T) {
	codexHome := stubCodexHomeForServerTest(t)
	future := time.Now().Add(24 * time.Hour).Unix()
	writeCodexAuthJSON(t, codexHome, "atk-codex-fixture", "rtk-codex-fixture", future)

	srv, st, _ := newDiscoveryServer(t, nil)

	code, body := postCreateConnectionWithBody(t, srv, store.Connection{
		Provider: "openai",
		Name:     "Codex CLI",
		AuthType: "auto_detect",
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%q", code, body)
	}

	connID := readBackConnectionID(t, body)
	stored := readBackConnection(t, st, connID)

	if stored.AuthType != auth.AuthTypeSubscription {
		t.Errorf("stored.AuthType = %q, want %q (create-time conversion)",
			stored.AuthType, auth.AuthTypeSubscription)
	}
	if stored.AccessToken != "atk-codex-fixture" {
		t.Errorf("stored.AccessToken = %q, want atk-codex-fixture", stored.AccessToken)
	}
	if stored.RefreshToken != "rtk-codex-fixture" {
		t.Errorf("stored.RefreshToken = %q, want rtk-codex-fixture (refresh-loop needs this)",
			stored.RefreshToken)
	}
	if stored.Provider != "openai" {
		t.Errorf("stored.Provider = %q, want openai", stored.Provider)
	}
}

// TestCreateConnection_OpenAI_AutoDetect_MissingReturns400 — error path.
// $CODEX_HOME points at empty temp dir (no auth.json). POST returns 400
// with the spec-pinned error message text. Per AC-AD-8.
func TestCreateConnection_OpenAI_AutoDetect_MissingReturns400(t *testing.T) {
	stubCodexHomeForServerTest(t) // CODEX_HOME points at empty temp dir

	srv, _, _ := newDiscoveryServer(t, nil)

	code, body := postCreateConnectionWithBody(t, srv, store.Connection{
		Provider: "openai",
		Name:     "Codex CLI",
		AuthType: "auto_detect",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", code, body)
	}
	if !bytes.Contains(body, []byte("Codex CLI credentials not found")) {
		t.Errorf("body does not include 'Codex CLI credentials not found'; got %q", body)
	}
}

// TestCreateConnection_OpenAI_AutoDetect_ExpiredReturns400 — second
// error path. Valid file but past expiry. Per AC-AD-8.
func TestCreateConnection_OpenAI_AutoDetect_ExpiredReturns400(t *testing.T) {
	codexHome := stubCodexHomeForServerTest(t)
	past := time.Now().Add(-1 * time.Hour).Unix()
	writeCodexAuthJSON(t, codexHome, "atk-expired", "rtk-expired", past)

	srv, _, _ := newDiscoveryServer(t, nil)

	code, body := postCreateConnectionWithBody(t, srv, store.Connection{
		Provider: "openai",
		Name:     "Codex CLI",
		AuthType: "auto_detect",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", code, body)
	}
	if !bytes.Contains(body, []byte("Codex CLI credentials are expired")) {
		t.Errorf("body does not include 'Codex CLI credentials are expired'; got %q", body)
	}
}

// TestCreateConnection_OpenAI_AutoDetect_DoesNotAffectClaude —
// regression: the parallel arm added for openai must not break the
// existing anthropic arm. Run the anthropic arm with NO Claude
// credentials available; it should still return 400 with the
// Claude-specific error message (not Codex's).
func TestCreateConnection_OpenAI_AutoDetect_DoesNotAffectClaude(t *testing.T) {
	// stubCodexHomeForServerTest stubs HOME + USERPROFILE too, so
	// detect.DetectClaude also reads from the temp dir (and finds
	// nothing). This gives us the negative case for Claude.
	stubCodexHomeForServerTest(t)

	srv, _, _ := newDiscoveryServer(t, nil)

	code, body := postCreateConnectionWithBody(t, srv, store.Connection{
		Provider: "anthropic",
		Name:     "Claude Code",
		AuthType: "auto_detect",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", code, body)
	}
	if !bytes.Contains(body, []byte("Claude Code credentials not found")) {
		t.Errorf("Claude error message regressed; expected 'Claude Code credentials not found', got %q", body)
	}
	// Critical: the new Codex arm must NOT shadow the Claude arm.
	if bytes.Contains(body, []byte("Codex")) {
		t.Errorf("Claude path returned Codex-mentioning error — anthropic arm broken: %q", body)
	}
}

// TestHandleDetectCodex_ReturnsCorrectShape — AC-AD-4. GET /api/detect/codex
// returns {found:true, expired:false} (subscription_type omitted via
// omitempty since codex auth.json has no tier field).
func TestHandleDetectCodex_ReturnsCorrectShape(t *testing.T) {
	codexHome := stubCodexHomeForServerTest(t)
	future := time.Now().Add(24 * time.Hour).Unix()
	writeCodexAuthJSON(t, codexHome, "atk-detect-shape", "rtk", future)

	srv, _, _ := newDiscoveryServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/detect/codex", nil)
	rec := httptest.NewRecorder()
	srv.handleDetectCodex(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var result struct {
		Found            bool   `json:"found"`
		SubscriptionType string `json:"subscription_type"`
		Expired          bool   `json:"expired"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (body=%q)", err, rec.Body.Bytes())
	}
	if !result.Found {
		t.Errorf("Found = false, want true")
	}
	if result.Expired {
		t.Errorf("Expired = true, want false")
	}
	// SubscriptionType is intentionally empty for codex (no tier field in
	// auth.json today). The JSON should have omitted the field via
	// omitempty — verify by checking the raw body doesn't contain it.
	if bytes.Contains(rec.Body.Bytes(), []byte(`"subscription_type"`)) {
		t.Errorf("response body contains subscription_type field; expected omitempty to drop it. body=%q",
			rec.Body.Bytes())
	}
}

// (Sanity compile check — these imports are used above.)
var _ = context.Background
