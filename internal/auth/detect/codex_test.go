package detect

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stubHomeAndCodexHome stubs every environment + global the codex
// path-search code reads from, so tests are hermetic against the
// developer's real machine state. Returns the temp home directory
// path so tests can place fake `~/.codex/auth.json` files under it.
//
// MUST be called in every test that exercises DetectCodex. Without
// it, tests would either write to the developer's real `~/.codex/`,
// inherit live `$CODEX_HOME`, OR (on Windows machines with WSL)
// read the developer's real Codex CLI installation via the WSL
// distro probe at `\\wsl$\Ubuntu\home\<user>\.codex\auth.json` —
// the last of which is invisible to HOME/USERPROFILE stubs because
// the WSL path is constructed from hardcoded distro names.
//
// Pattern lifted from internal/auth/imports/imports_test.go:222,232
// (which only needed CODEX_HOME — this helper extends it for
// detect/'s broader path-search surface).
func stubHomeAndCodexHome(t *testing.T) string {
	t.Helper()
	tmpHome := t.TempDir()
	// HOME wins on Unix; USERPROFILE wins on Windows. Stub both so
	// os.UserHomeDir() resolves predictably regardless of platform.
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	// Clear CODEX_HOME and APPDATA so tests that don't set them
	// explicitly inherit "not set" rather than the developer's
	// environment value.
	t.Setenv("CODEX_HOME", "")
	t.Setenv("APPDATA", "")
	// Disable WSL distro probing — the codexCredPaths search reads
	// hardcoded `\\wsl$\<distro>\home\` paths regardless of HOME
	// stubbing. On a Windows-with-WSL machine where the user has
	// Codex CLI installed in WSL, this would leak the real
	// credentials into "Missing"/"Empty"/"Malformed" tests.
	t.Cleanup(DisableWSLForTesting())
	return tmpHome
}

// writeCodexFile writes a Codex auth.json fixture at the given path,
// creating parent directories as needed.
func writeCodexFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// validCodexBody returns a Codex auth.json body string with the given
// access_token + refresh_token + expires_at literal (any valid JSON
// scalar — int seconds, int ms, or quoted ISO-8601 string).
func validCodexBody(accessToken, refreshToken, expiresAtRaw string) string {
	return fmt.Sprintf(`{
		"tokens": {
			"access_token": %q,
			"refresh_token": %q,
			"id_token": "stub-id-token",
			"expires_at": %s
		}
	}`, accessToken, refreshToken, expiresAtRaw)
}

// TestDetectCodex_FoundValid — happy path. ~/.codex/auth.json exists
// with a non-expired access_token; DetectCodex returns Found=true,
// Expired=false, and the populated AccessToken.
func TestDetectCodex_FoundValid(t *testing.T) {
	tmpHome := stubHomeAndCodexHome(t)
	future := time.Now().Add(24 * time.Hour).Unix()
	writeCodexFile(t,
		filepath.Join(tmpHome, ".codex", "auth.json"),
		validCodexBody("atk-valid", "rtk-valid", fmt.Sprintf("%d", future)),
	)

	result, creds := DetectCodex()
	if !result.Found {
		t.Fatal("Found = false, want true")
	}
	if result.Expired {
		t.Errorf("Expired = true, want false")
	}
	if creds == nil {
		t.Fatal("creds = nil, want non-nil")
	}
	if creds.AccessToken != "atk-valid" {
		t.Errorf("AccessToken = %q, want atk-valid", creds.AccessToken)
	}
}

// TestDetectCodex_MissingFile — no $CODEX_HOME, no ~/.codex/auth.json;
// DetectCodex returns Found=false.
func TestDetectCodex_MissingFile(t *testing.T) {
	stubHomeAndCodexHome(t)

	result, creds := DetectCodex()
	if result.Found {
		t.Errorf("Found = true, want false (no credentials anywhere)")
	}
	if creds != nil {
		t.Errorf("creds = non-nil, want nil")
	}
}

// TestDetectCodex_EmptyToken — file exists but access_token is
// the empty string. Same treatment as no file at all.
func TestDetectCodex_EmptyToken(t *testing.T) {
	tmpHome := stubHomeAndCodexHome(t)
	writeCodexFile(t,
		filepath.Join(tmpHome, ".codex", "auth.json"),
		validCodexBody("", "rtk-empty-access", "0"),
	)

	result, _ := DetectCodex()
	if result.Found {
		t.Errorf("Found = true, want false (empty access_token)")
	}
}

// TestDetectCodex_MalformedJSON — garbage bytes. Must NOT panic;
// must return Found=false.
func TestDetectCodex_MalformedJSON(t *testing.T) {
	tmpHome := stubHomeAndCodexHome(t)
	writeCodexFile(t,
		filepath.Join(tmpHome, ".codex", "auth.json"),
		`{this is not valid json][}`,
	)

	result, _ := DetectCodex()
	if result.Found {
		t.Errorf("Found = true, want false (malformed JSON)")
	}
}

// TestDetectCodex_ExpiredUnixSeconds — past unix-seconds expiry;
// Found=true (file is parseable) + Expired=true.
func TestDetectCodex_ExpiredUnixSeconds(t *testing.T) {
	tmpHome := stubHomeAndCodexHome(t)
	past := time.Now().Add(-1 * time.Hour).Unix()
	writeCodexFile(t,
		filepath.Join(tmpHome, ".codex", "auth.json"),
		validCodexBody("atk-expired-s", "rtk", fmt.Sprintf("%d", past)),
	)

	result, _ := DetectCodex()
	if !result.Found {
		t.Fatal("Found = false, want true (file exists)")
	}
	if !result.Expired {
		t.Errorf("Expired = false, want true (past unix-seconds)")
	}
}

// TestDetectCodex_ExpiredUnixMs — past unix-milliseconds expiry.
// Heuristic at parseCodexExpiry says values > 1e12 are ms.
func TestDetectCodex_ExpiredUnixMs(t *testing.T) {
	tmpHome := stubHomeAndCodexHome(t)
	pastMs := time.Now().Add(-1 * time.Hour).UnixMilli()
	writeCodexFile(t,
		filepath.Join(tmpHome, ".codex", "auth.json"),
		validCodexBody("atk-expired-ms", "rtk", fmt.Sprintf("%d", pastMs)),
	)

	result, _ := DetectCodex()
	if !result.Found {
		t.Fatal("Found = false, want true")
	}
	if !result.Expired {
		t.Errorf("Expired = false, want true (past unix-ms — heuristic %d > 1e12)", pastMs)
	}
}

// TestDetectCodex_ExpiresISO8601 — `expires_at` as RFC3339 string.
// Must parse correctly and report not-expired for future-time.
func TestDetectCodex_ExpiresISO8601(t *testing.T) {
	tmpHome := stubHomeAndCodexHome(t)
	futureISO := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	writeCodexFile(t,
		filepath.Join(tmpHome, ".codex", "auth.json"),
		validCodexBody("atk-iso", "rtk", fmt.Sprintf("%q", futureISO)),
	)

	result, creds := DetectCodex()
	if !result.Found {
		t.Fatal("Found = false, want true")
	}
	if result.Expired {
		t.Errorf("Expired = true, want false (future ISO-8601)")
	}
	if creds.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt is zero, want parsed RFC3339 time")
	}
}

// TestDetectCodex_CODEX_HOME_OverridesDefault — when BOTH
// $CODEX_HOME (with valid file) AND ~/.codex/auth.json exist,
// the $CODEX_HOME path wins. Path-precedence pin per spec § path order.
//
// Cross-platform note: stubHomeAndCodexHome sets HOME + USERPROFILE
// so os.UserHomeDir() resolves to a temp dir — without this stub the
// test would either read the developer's real ~/.codex/ (test
// pollution) or be non-deterministic.
func TestDetectCodex_CODEX_HOME_OverridesDefault(t *testing.T) {
	tmpHome := stubHomeAndCodexHome(t)

	// Write the "shouldn't be used" file at ~/.codex/auth.json.
	writeCodexFile(t,
		filepath.Join(tmpHome, ".codex", "auth.json"),
		validCodexBody("atk-default-home", "rtk", "0"),
	)

	// Write the "should win" file at $CODEX_HOME/auth.json in a
	// different directory (so we can verify the right file was read).
	envHome := t.TempDir()
	future := time.Now().Add(24 * time.Hour).Unix()
	writeCodexFile(t,
		filepath.Join(envHome, "auth.json"),
		validCodexBody("atk-env-override", "rtk-env", fmt.Sprintf("%d", future)),
	)
	t.Setenv("CODEX_HOME", envHome)

	_, creds := DetectCodex()
	if creds == nil {
		t.Fatal("creds = nil, want $CODEX_HOME file to be read")
	}
	if creds.AccessToken != "atk-env-override" {
		t.Errorf("AccessToken = %q, want atk-env-override "+
			"(CODEX_HOME must win over ~/.codex/)", creds.AccessToken)
	}
}

// TestDetectCodex_RefreshTokenPreserved — refresh_token in the file
// must propagate to CodexCredentials.RefreshToken so the refresh
// loop can rotate the access token later.
func TestDetectCodex_RefreshTokenPreserved(t *testing.T) {
	tmpHome := stubHomeAndCodexHome(t)
	future := time.Now().Add(24 * time.Hour).Unix()
	writeCodexFile(t,
		filepath.Join(tmpHome, ".codex", "auth.json"),
		validCodexBody("atk-rt", "rtk-preserved-xyz", fmt.Sprintf("%d", future)),
	)

	_, creds := DetectCodex()
	if creds == nil {
		t.Fatal("creds = nil")
	}
	if creds.RefreshToken != "rtk-preserved-xyz" {
		t.Errorf("RefreshToken = %q, want rtk-preserved-xyz", creds.RefreshToken)
	}
}

// TestDetectCodex_NeverPanicsOnBadInput — fuzz a few malformed shapes;
// DetectCodex must not panic.
func TestDetectCodex_NeverPanicsOnBadInput(t *testing.T) {
	tmpHome := stubHomeAndCodexHome(t)
	cases := []string{
		``,                          // empty file
		`{`,                         // truncated
		`null`,                      // valid JSON, wrong shape
		`{"tokens": null}`,          // tokens is null
		`{"tokens": "string"}`,      // tokens is wrong type
		`{"tokens": {"access_token": 12345}}`, // access_token wrong type
		`[]`,                        // top-level array
	}
	for i, body := range cases {
		writeCodexFile(t,
			filepath.Join(tmpHome, ".codex", "auth.json"),
			body,
		)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("case %d: panic on body %q: %v", i, body, r)
				}
			}()
			result, _ := DetectCodex()
			if result.Found {
				t.Errorf("case %d: Found = true on malformed body %q", i, body)
			}
		}()
	}
}
