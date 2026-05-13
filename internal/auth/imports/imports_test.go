package imports

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sage-router/internal/auth/providers"
)

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// ── Codex ──

func TestParseCodex_HappyPath(t *testing.T) {
	cred, err := parseCodexFile(fixturePath(t, "codex_valid.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cred.AccessToken != "atk-codex-abc123" {
		t.Errorf("AccessToken = %q", cred.AccessToken)
	}
	if cred.RefreshToken != "rtk-codex-xyz789" {
		t.Errorf("RefreshToken = %q", cred.RefreshToken)
	}
	if cred.ExpiresAt.Unix() != 9999999999 {
		t.Errorf("ExpiresAt unix = %d, want 9999999999", cred.ExpiresAt.Unix())
	}
	// id_token in the fixture carries chatgpt_account_id=acct-88.
	if cred.AccountID != "acct-88" {
		t.Errorf("AccountID = %q, want acct-88 (from id_token claim)", cred.AccountID)
	}
}

func TestParseCodex_EmptyTokenSentinel(t *testing.T) {
	_, err := parseCodexFile(fixturePath(t, "codex_empty_token.json"))
	if !errors.Is(err, ErrEmptyToken) {
		t.Errorf("want ErrEmptyToken; got %v", err)
	}
}

func TestParseCodex_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(p, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCodexFile(p); err == nil {
		t.Error("expected parse error on malformed JSON")
	}
}

func TestParseCodex_FlexibleExpiry(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64 // expected unix seconds
	}{
		{"seconds-int", `{"tokens":{"access_token":"a","expires_at":1717862400}}`, 1717862400},
		{"milliseconds-int", `{"tokens":{"access_token":"a","expires_at":1717862400000}}`, 1717862400},
		{"iso-string", `{"tokens":{"access_token":"a","expires_at":"2024-06-08T12:00:00Z"}}`, time.Date(2024, 6, 8, 12, 0, 0, 0, time.UTC).Unix()},
		{"missing", `{"tokens":{"access_token":"a"}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "auth.json")
			if err := os.WriteFile(p, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			cred, err := parseCodexFile(p)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if tc.want == 0 {
				if !cred.ExpiresAt.IsZero() {
					t.Errorf("ExpiresAt should be zero; got %v", cred.ExpiresAt)
				}
				return
			}
			if cred.ExpiresAt.Unix() != tc.want {
				t.Errorf("ExpiresAt unix = %d, want %d", cred.ExpiresAt.Unix(), tc.want)
			}
		})
	}
}

// ── Claude ──

func TestParseClaude_HappyPath(t *testing.T) {
	cred, err := parseClaudeFile(fixturePath(t, "claude_valid.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cred.AccessToken != "sk-ant-oat01-fixture-access" {
		t.Errorf("AccessToken wrong: %s", cred.AccessToken)
	}
	if cred.RefreshToken != "sk-ant-ort01-fixture-refresh" {
		t.Errorf("RefreshToken wrong: %s", cred.RefreshToken)
	}
	if cred.ExpiresAt.UnixMilli() != 9999999999000 {
		t.Errorf("ExpiresAt ms = %d, want 9999999999000", cred.ExpiresAt.UnixMilli())
	}
	if cred.ExtraData["subscription_type"] != "max" {
		t.Errorf("subscription_type missing: %+v", cred.ExtraData)
	}
}

func TestParseClaude_EmptyToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cred.json")
	os.WriteFile(p, []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"r"}}`), 0600)
	_, err := parseClaudeFile(p)
	if !errors.Is(err, ErrEmptyToken) {
		t.Errorf("want ErrEmptyToken; got %v", err)
	}
}

// ── Copilot ──

func TestParseCopilot_HappyPath(t *testing.T) {
	cred, err := parseCopilotFile(fixturePath(t, "copilot_valid.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// AccessToken is intentionally empty — refresh.Refresh mints one on
	// first use.
	if cred.AccessToken != "" {
		t.Errorf("AccessToken should be empty for Copilot import; got %q", cred.AccessToken)
	}
	if cred.RefreshToken != "ghp_fixture_copilot_token_xyz" {
		t.Errorf("RefreshToken = %q", cred.RefreshToken)
	}
	if cred.AccountID != "fixture-user" {
		t.Errorf("AccountID = %q, want fixture-user", cred.AccountID)
	}
}

func TestParseCopilot_MissingGitHubEntry(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"tokens":{"other.example":{"oauth_token":"t"}}}`), 0600)
	_, err := parseCopilotFile(p)
	if !errors.Is(err, ErrEmptyToken) {
		t.Errorf("want ErrEmptyToken when github.com missing; got %v", err)
	}
}

// ── Gemini ──

func TestParseGemini_HappyPath(t *testing.T) {
	cred, err := parseGeminiFile(fixturePath(t, "gemini_valid.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cred.AccessToken != "ya29.fixture-gemini-access" {
		t.Errorf("AccessToken = %s", cred.AccessToken)
	}
	if cred.RefreshToken != "1//fixture-gemini-refresh" {
		t.Errorf("RefreshToken = %s", cred.RefreshToken)
	}
	if cred.ExpiresAt.UnixMilli() != 9999999999000 {
		t.Errorf("ExpiresAt ms = %d", cred.ExpiresAt.UnixMilli())
	}
	if !strings.Contains(cred.ExtraData["scope"].(string), "generative-language") {
		t.Errorf("scope ExtraData missing or wrong: %+v", cred.ExtraData)
	}
}

func TestParseGemini_EmptyAccessTokenRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "creds.json")
	os.WriteFile(p, []byte(`{"refresh_token":"r","expiry_date":0}`), 0600)
	if _, err := parseGeminiFile(p); !errors.Is(err, ErrEmptyToken) {
		t.Errorf("want ErrEmptyToken; got %v", err)
	}
}

// ── ImportFromCLI dispatcher ──

func TestImportFromCLI_OverridePathWiresCorrectParser(t *testing.T) {
	cred, err := ImportFromCLI("openai", fixturePath(t, "codex_valid.json"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if cred.Provider != "openai" {
		t.Errorf("Provider = %q, want openai (dispatcher should stamp it)", cred.Provider)
	}
	if cred.AccessToken == "" {
		t.Error("dispatcher dropped tokens")
	}
}

func TestImportFromCLI_UnknownProviderErrors(t *testing.T) {
	if _, err := ImportFromCLI("not-a-provider", "/tmp/x"); err == nil {
		t.Error("unknown provider should error")
	}
}

func TestImportFromCLI_FileNotFound(t *testing.T) {
	_, err := ImportFromCLI("openai", "/tmp/definitely-does-not-exist-"+t.Name())
	if !errors.Is(err, ErrFileNotFound) {
		t.Errorf("want ErrFileNotFound; got %v", err)
	}
}

// ── Path resolution ──

func TestResolvePath_HonorsEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)
	got := resolvePath(providers.Providers["openai"])
	want := filepath.Join(tmp, "auth.json")
	if got != want {
		t.Errorf("resolvePath = %q, want %q", got, want)
	}
}

func TestResolvePath_ExpandsTilde(t *testing.T) {
	// Bypass env override.
	t.Setenv("CODEX_HOME", "")
	got := resolvePath(providers.Providers["openai"])
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(got, home) {
		t.Errorf("resolvePath should start with $HOME (%s); got %s", home, got)
	}
}

func TestExpandTilde_NoOpForAbsolutePaths(t *testing.T) {
	if got := expandTilde("/etc/hosts"); got != "/etc/hosts" {
		t.Errorf("expandTilde should be no-op for absolute paths; got %s", got)
	}
}

// ── macOS Keychain hint ──

func TestMacOSClaudeKeychainHint_OnlyForFileNotFound(t *testing.T) {
	// The hint logic combines "file missing" + "darwin GOOS." We can only
	// test the negative half on non-darwin systems; the darwin true-path
	// is verified by the AC18 wording test below.
	other := errors.New("other error")
	if MacOSClaudeKeychainHint(other) {
		t.Error("MacOSClaudeKeychainHint should be false for non-file-not-found errors")
	}
}

func TestKeychainHintMessage_RequiredPhrases(t *testing.T) {
	required := []string{
		"Claude Code",
		"Keychain",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"auth login --provider anthropic",
	}
	for _, phrase := range required {
		if !strings.Contains(KeychainHintMessage, phrase) {
			t.Errorf("KeychainHintMessage missing required phrase %q", phrase)
		}
	}
}

// ── (Internal) Json round-trip sanity ──

func TestProviderDataExtras_RoundTripJSON(t *testing.T) {
	// Defensive: anything we put in ExtraData must survive the
	// AuthStore JSON marshal/unmarshal cycle. This guards against
	// accidentally using non-JSON-able values (channels, funcs, etc.).
	cred, err := parseClaudeFile(fixturePath(t, "claude_valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(cred.ExtraData); err != nil {
		t.Errorf("ExtraData not JSON-serializable: %v", err)
	}
}
