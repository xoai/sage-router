package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stubHomeForClaudeTest stubs every environment + global the claude
// path-search code reads from, so tests are hermetic against the
// developer's real machine state. Mirrors stubHomeAndCodexHome in
// codex_test.go.
func stubHomeForClaudeTest(t *testing.T) string {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	t.Setenv("APPDATA", "")
	t.Cleanup(DisableWSLForTesting())
	return tmpHome
}

// writeClaudeFile writes a .credentials.json fixture at the given path,
// creating parents as needed.
func writeClaudeFile(t *testing.T, path, accessToken, refreshToken string, expiresAtMs int64) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      accessToken,
			"refreshToken":     refreshToken,
			"expiresAt":        expiresAtMs,
			"scopes":           []string{"user:profile", "user:inference"},
			"subscriptionType": "max",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestDetectClaude_FreshFileFound — single-path happy path. Verifies
// the two-pass logic returns immediately on a non-expired file.
func TestDetectClaude_FreshFileFound(t *testing.T) {
	tmpHome := stubHomeForClaudeTest(t)
	future := time.Now().Add(24 * time.Hour).UnixMilli()
	writeClaudeFile(t, filepath.Join(tmpHome, ".claude", ".credentials.json"),
		"atk-fresh", "rtk-fresh", future)

	result, creds := DetectClaude()
	if !result.Found {
		t.Fatal("Found = false, want true")
	}
	if result.Expired {
		t.Error("Expired = true, want false (file has future expiresAt)")
	}
	if creds == nil || creds.AccessToken != "atk-fresh" {
		t.Errorf("AccessToken = %v, want atk-fresh", creds)
	}
}

// TestDetectClaude_FallsBackToFirstExpiredWhenAllExpired — fix
// 20260514-claude-oauth-and-detect Bug 2. When no non-expired file
// is found, the fallback path returns the first found file so the
// "Claude Code credentials are expired" UX still fires (per
// routes_api.go:253). Locks the two-pass loop's fallback contract.
func TestDetectClaude_FallsBackToFirstExpiredWhenAllExpired(t *testing.T) {
	tmpHome := stubHomeForClaudeTest(t)
	past := time.Now().Add(-24 * time.Hour).UnixMilli()
	writeClaudeFile(t, filepath.Join(tmpHome, ".claude", ".credentials.json"),
		"atk-expired", "rtk-expired", past)

	result, creds := DetectClaude()
	if !result.Found {
		t.Fatal("Found = false, want true (expired file still returned)")
	}
	if !result.Expired {
		t.Error("Expired = false, want true (file has past expiresAt)")
	}
	if creds == nil || creds.AccessToken != "atk-expired" {
		t.Errorf("AccessToken = %v, want atk-expired (fallback path)", creds)
	}
}

// TestDetectClaude_NoFilesAnywhere — regression guard. With every
// candidate path empty, returns Found=false (not a panic).
func TestDetectClaude_NoFilesAnywhere(t *testing.T) {
	stubHomeForClaudeTest(t)

	result, creds := DetectClaude()
	if result.Found {
		t.Errorf("Found = true, want false")
	}
	if creds != nil {
		t.Errorf("creds = %v, want nil", creds)
	}
}

// Note: Claude doesn't have a CLAUDE_HOME env-var override (unlike
// CODEX_HOME for codex), so deterministically injecting a SECOND
// candidate path for the prefer-non-expired test is platform-specific
// (only Windows yields multiple paths via APPDATA + USERPROFILE WSL
// probes). The multi-path two-pass branch is exercised end-to-end via
// codex_test.go's TestDetectCodex_PrefersNonExpiredAcrossPaths (which
// uses CODEX_HOME to inject a deterministic second path). The same
// two-pass logic is mirrored in claude.go (cross-referenced in the
// DetectClaude doc comment), so codex coverage protects against
// claude regression structurally.
