package providers

import (
	"testing"
)

func TestResolve_KnownAliases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"openai", "openai"},
		{"anthropic", "anthropic"},
		{"claude", "anthropic"},
		{"copilot", "github-copilot"},
		{"github-copilot", "github-copilot"},
		{"gemini", "gemini"},
		{"google", "gemini"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := Resolve(tt.input)
			if err != nil {
				t.Fatalf("Resolve(%q) returned err: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("Resolve(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolve_CaseInsensitiveAndTrimmed(t *testing.T) {
	got, err := Resolve("  Claude  ")
	if err != nil {
		t.Fatalf("Resolve trimmed/cased: %v", err)
	}
	if got != "anthropic" {
		t.Errorf("Resolve trimmed = %q, want anthropic", got)
	}
}

func TestResolve_UnknownErrors(t *testing.T) {
	_, err := Resolve("not-a-provider")
	if err == nil {
		t.Fatal("Resolve(unknown) should error")
	}
}

func TestResolve_AutoDetectIsLiteral(t *testing.T) {
	// auto_detect is a transient AuthType handled in the request path —
	// NOT a provider alias. Resolve should NOT recognize it.
	if _, err := Resolve("auto_detect"); err == nil {
		t.Error(`Resolve("auto_detect") should error — it is an AuthType value, not a provider alias`)
	}
}

func TestProviders_HaveRequiredFields(t *testing.T) {
	// Sanity check on the static registry — each provider entry has the
	// fields the OAuth + import code paths need.
	wantProviders := []string{"openai", "anthropic", "github-copilot", "gemini"}
	for _, id := range wantProviders {
		t.Run(id, func(t *testing.T) {
			cfg, ok := Providers[id]
			if !ok {
				t.Fatalf("provider %s missing from registry", id)
			}
			if cfg.ID != id {
				t.Errorf("ID = %q, want %q", cfg.ID, id)
			}
			if cfg.DisplayName == "" {
				t.Error("DisplayName empty")
			}
			if cfg.FlowType == "" {
				t.Error("FlowType empty")
			}
			// Subscription-allowed model list must be non-empty for v1.
			if len(cfg.SubscriptionAllowedModels) == 0 {
				t.Error("SubscriptionAllowedModels empty")
			}
			// PKCE providers need the auth URL set; import-only providers don't.
			if cfg.FlowType == FlowPKCE {
				if cfg.AuthorizeURL == "" || cfg.TokenURL == "" || cfg.ClientID == "" {
					t.Errorf("PKCE provider %s missing required OAuth fields", id)
				}
				if cfg.RedirectPort == 0 {
					t.Errorf("PKCE provider %s missing RedirectPort", id)
				}
				if cfg.RedirectPath == "" {
					t.Errorf("PKCE provider %s missing RedirectPath", id)
				}
			}
			if cfg.FlowType == FlowImportOnly {
				if cfg.ImportPath == "" {
					t.Errorf("Import-only provider %s missing ImportPath", id)
				}
			}
		})
	}
}

func TestPKCEProviders_HaveExpectedClientIDs(t *testing.T) {
	// These are the public Codex CLI / Claude Code client IDs from the
	// spec. Hardcoded values verified once here so accidental edits are
	// caught.
	if Providers["openai"].ClientID != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Errorf("openai ClientID changed unexpectedly: %s", Providers["openai"].ClientID)
	}
	if Providers["anthropic"].ClientID != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
		t.Errorf("anthropic ClientID changed unexpectedly: %s", Providers["anthropic"].ClientID)
	}
}

func TestPKCEProviders_HaveExpectedRedirectPorts(t *testing.T) {
	// Provider OAuth registrations require specific redirect ports.
	// Changing these breaks the redirect_uri the provider trusts.
	if got, want := Providers["openai"].RedirectPort, 1455; got != want {
		t.Errorf("openai RedirectPort = %d, want %d", got, want)
	}
	if got, want := Providers["anthropic"].RedirectPort, 53692; got != want {
		t.Errorf("anthropic RedirectPort = %d, want %d", got, want)
	}
}

// TestOpenAI_PKCE_HasCodexCLIAuthShape pins the OpenAI Scopes slice
// and ExtraAuthParams map byte-for-byte against the Codex CLI canonical
// shape at openai/codex codex-rs/login/src/server.rs:495,504-512 and
// codex-rs/login/src/auth/default_client.rs:36 (DEFAULT_ORIGINATOR).
//
// Drift protection: if a future careless edit drops a scope or extra
// param, this test fires before the user ever sees the broken
// authorize URL. The original "Lỗi xác thực" bug shipped without
// this assertion — it now exists specifically to prevent recurrence.
//
// When refreshing against newer Codex CLI upstream, update both
// halves of this test AND the registry entry together.
func TestOpenAI_PKCE_HasCodexCLIAuthShape(t *testing.T) {
	cfg := Providers["openai"]

	wantScopes := []string{
		"openid", "profile", "email", "offline_access",
		"api.connectors.read", "api.connectors.invoke",
	}
	if len(cfg.Scopes) != len(wantScopes) {
		t.Fatalf("Scopes count = %d, want %d (Codex CLI canonical): got=%v want=%v",
			len(cfg.Scopes), len(wantScopes), cfg.Scopes, wantScopes)
	}
	for i, want := range wantScopes {
		if cfg.Scopes[i] != want {
			t.Errorf("Scopes[%d] = %q, want %q", i, cfg.Scopes[i], want)
		}
	}

	wantExtra := map[string]string{
		"codex_cli_simplified_flow":  "true",
		"id_token_add_organizations": "true",
		"originator":                 "codex_cli_rs",
	}
	if len(cfg.ExtraAuthParams) != len(wantExtra) {
		t.Fatalf("ExtraAuthParams count = %d, want %d: got=%v want=%v",
			len(cfg.ExtraAuthParams), len(wantExtra), cfg.ExtraAuthParams, wantExtra)
	}
	for k, want := range wantExtra {
		if got := cfg.ExtraAuthParams[k]; got != want {
			t.Errorf("ExtraAuthParams[%q] = %q, want %q", k, got, want)
		}
	}
}
