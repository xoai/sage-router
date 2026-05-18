// Package providers defines the static OAuth + import configuration for
// each upstream LLM provider sage-router supports for subscription auth.
//
// The values here are the public CLI client IDs and endpoints from the
// official Claude Code (Anthropic), Codex CLI (OpenAI), Copilot extension
// (GitHub), and Gemini CLI (Google) tools. Using these client IDs means
// sage-router can complete OAuth flows against the same redirect URIs
// those tools have registered — at the cost that any client-side change
// to those tools' OAuth configuration can break sage-router until updated.
//
// Update procedure: when a provider rotates a client ID or scope, edit
// this file and bump the entry's last-verified date in the comment.
// The registry is consulted at flow-start time so no caching layer needs
// to be invalidated.
package providers

import (
	"errors"
	"strings"
)

// FlowType is how a provider acquires fresh tokens.
type FlowType string

const (
	// FlowPKCE: browser-based authorize-and-callback. Used for OpenAI and
	// Anthropic where the provider's OAuth server trusts a localhost
	// redirect_uri registered against the public CLI client_id.
	FlowPKCE FlowType = "pkce"

	// FlowImportOnly: no in-band login flow. The user runs the provider's
	// official CLI tool (which performs the actual OAuth), then sage-router
	// reads the resulting credential file from disk. Used for GitHub
	// Copilot (its OAuth involves an editor-extension exchange we can't
	// replicate) and Gemini (pi-ai removed OAuth in v0.71.0).
	FlowImportOnly FlowType = "import_only"
)

// ProviderConfig holds everything the OAuth/import paths need to know
// about a provider. All fields are evaluated when a flow is initiated —
// no runtime mutation expected.
type ProviderConfig struct {
	ID          string
	DisplayName string
	FlowType    FlowType

	// PKCE fields (zero values for FlowImportOnly).
	AuthorizeURL    string
	TokenURL        string
	ClientID        string
	Scopes          []string
	ExtraAuthParams map[string]string
	RedirectPort    int    // 1455 for openai, 53692 for anthropic
	RedirectPath    string // "/auth/callback" for openai, "/callback" for anthropic
	AccountIDClaim  string // JWT claim path for account ID (OpenAI). "" if not applicable.

	// Import fields.
	ImportPath       string // default ~/.codex/auth.json etc.
	ImportPathEnvVar string // optional override env var name; "" if none.

	// Routing fields.
	SubscriptionAllowedModels []string // models this subscription can serve; checked by selector (M3.1).

	// Refresh-token endpoint. For Copilot this is the gh→copilot exchange.
	RefreshURL string

	// RequiresAPIKeyExchange field removed in cycle 20260517-provider-auth-variants
	// M2.6.3. The exchange chain was wrong-path (memory `f32bbc73`).

	// TokenRequestFormat selects the wire shape for the authorization-code
	// grant POST. RFC 6749 §4.1.3 default is "form" (application/x-www-form-
	// urlencoded). Anthropic's platform.claude.com/v1/oauth/token is JSON-only
	// — sending form-encoded returns 400 "Invalid request format". Verified
	// against opencode-anthropic-auth plugin source (src/auth.ts::exchangeCode
	// uses Content-Type: application/json). See M0.8 of cycle
	// 20260517-provider-auth-variants for the diagnostic that surfaced this.
	//
	// Values:
	//   ""     — equivalent to "form" (default, no migration needed for openai/etc.)
	//   "form" — application/x-www-form-urlencoded (RFC 6749 default)
	//   "json" — application/json (Anthropic's platform.claude.com)
	TokenRequestFormat string

	// RefreshTokenFormat controls the body shape for the REFRESH grant
	// path specifically. Distinct from TokenRequestFormat (initial PKCE
	// Exchange) because some providers split shapes between phases:
	// OpenAI's codex CLI uses form-encoded for Exchange but JSON for
	// Refresh (per codex-rs `login/src/auth/manager.rs`). Empty value
	// falls back to TokenRequestFormat for backward compatibility with
	// providers (Anthropic) whose endpoint accepts the same shape for
	// both phases. Cycle 20260517-provider-auth-variants M2 e2e fold.
	RefreshTokenFormat string
}

// Providers is the static registry, keyed by canonical provider ID.
//
// Provider constants last verified: 2026-05-11
//
// Sources:
//   - OpenAI client_id: Codex CLI repository, oauth/constants.go (or
//     equivalent — public on GitHub).
//   - Anthropic client_id: Claude Code repository, oauth.ts.
//   - GitHub Copilot client_id: VS Code Copilot extension, OAuth init.
//   - Gemini: no client_id needed (import-only — reads gemini-cli output).
var Providers = map[string]ProviderConfig{
	"openai": {
		ID:           "openai",
		DisplayName:  "OpenAI (ChatGPT Plus/Pro)",
		FlowType:     FlowPKCE,
		AuthorizeURL: "https://auth.openai.com/oauth/authorize",
		TokenURL:     "https://auth.openai.com/oauth/token",
		ClientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
		// Scopes mirror codex-rs/login/src/server.rs:495 verbatim:
		//   "openid profile email offline_access api.connectors.read api.connectors.invoke"
		// api.connectors.* are required by OpenAI's authorization server
		// for this client_id; omitting them caused authorize-time
		// rejection (visible to the user as "Lỗi xác thực" / auth error).
		Scopes: []string{"openid", "profile", "email", "offline_access", "api.connectors.read", "api.connectors.invoke"},
		// ExtraAuthParams mirror codex-rs/login/src/server.rs:504-508:
		//   codex_cli_simplified_flow=true
		//   id_token_add_organizations=true   (load-bearing for the
		//     AccountIDClaim path at line 92 below — without it, the
		//     id_token lacks the chatgpt_account_id claim)
		//   originator=codex_cli_rs           (the upstream
		//     DEFAULT_ORIGINATOR const from codex-rs/login/src/auth/
		//     default_client.rs:36; required to identify the client
		//     to OpenAI's authorization server)
		ExtraAuthParams: map[string]string{
			"codex_cli_simplified_flow":    "true",
			"id_token_add_organizations":   "true",
			"originator":                   "codex_cli_rs",
		},
		RedirectPort:     1455,
		RedirectPath:     "/auth/callback",
		AccountIDClaim:   "https://api.openai.com/auth.chatgpt_account_id",
		ImportPath:       "~/.codex/auth.json",
		ImportPathEnvVar: "CODEX_HOME",
		RefreshURL: "https://auth.openai.com/oauth/token",
		// Codex CLI uses DIFFERENT body shapes per endpoint phase:
		//   - Initial PKCE Exchange (codex-rs `login/src/server.rs::exchange_code_for_tokens`):
		//     form-encoded. sage-router default — TokenRequestFormat unset.
		//   - Refresh grant (codex-rs `login/src/auth/manager.rs`): JSON.
		// So OpenAI has TokenRequestFormat unset (form) AND
		// RefreshTokenFormat="json". Anthropic uses JSON for BOTH endpoints
		// (same platform.claude.com/v1/oauth/token URL — see registry entry below).
		// Cycle 20260517-provider-auth-variants M2 e2e fold — surfaced
		// when the user asked "are you sure refresh works?". Error envelope
		// is also non-RFC (chat-completions wrapper with codes
		// refresh_token_expired|reused|invalidated); handled by
		// refresh/oauth_form.go::normalizeError per memory `bf614108`
		// public-client OAuth parity rule.
		RefreshTokenFormat: "json",
		// Cycle 20260517-provider-auth-variants M2.6.3: RequiresAPIKeyExchange
		// removed. CodexSubscriptionExecutor uses PKCE access_token directly
		// against chatgpt.com/backend-api/codex/responses.
		//
		// SubscriptionAllowedModels removed (M5.10 pulled forward to M2 —
		// M0.8 E2E test showed the static list rejected gpt-5.4 which the
		// codex backend DOES accept). The per-account whitelist is now
		// determined at backend gate time; sage-router passes through.
		// Catalog-side hinting comes from openai@codex-subscription lister.
		// SubscriptionAllowedModels: <nil> — ModelInAllowlist permits any model.
	},
	"anthropic": {
		ID:             "anthropic",
		DisplayName:    "Anthropic (Claude Pro/Max)",
		FlowType:       FlowPKCE,
		AuthorizeURL:   "https://claude.ai/oauth/authorize",
		TokenURL:       "https://platform.claude.com/v1/oauth/token",
		ClientID:       "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		// Mirror what upstream Claude Code requests today. Verified
		// 2026-05-14 against a fresh ~/.claude/.credentials.json grant
		// (subscriptionType=max). Drift from this slice will cause
		// Anthropic's authorization server to reject the authorize URL
		// (or grant a narrower token). Order is alphabetical to match
		// the observed grant. See fix 20260514-claude-oauth-and-detect.
		Scopes: []string{
			"user:file_upload",
			"user:inference",
			"user:mcp_servers",
			"user:profile",
			"user:sessions:claude_code",
		},
		RedirectPort:   53692,
		RedirectPath:   "/callback",
		ImportPath:     "~/.claude/.credentials.json",
		RefreshURL:     "https://platform.claude.com/v1/oauth/token",
		// Anthropic's platform.claude.com/v1/oauth/token requires JSON.
		// Form-encoded → 400 "Invalid request format" (M0.8 diagnostic
		// 2026-05-17, request_id req_011Cb86cWhWMtdUsmAK4cLRc).
		TokenRequestFormat: "json",
		SubscriptionAllowedModels: []string{
			"claude-sonnet-4-x",
			"claude-opus-4-x",
			"claude-haiku-4-x",
			"claude-3-5-sonnet-latest",
			"claude-3-5-haiku-latest",
		},
	},
	"github-copilot": {
		ID:               "github-copilot",
		DisplayName:      "GitHub Copilot",
		FlowType:         FlowImportOnly,
		ImportPath:       "~/.copilot/settings.json",
		ImportPathEnvVar: "COPILOT_HOME",
		// Copilot refresh exchanges a GitHub OAuth token for a Copilot
		// bearer token. Implementation in internal/auth/refresh/copilot.go.
		RefreshURL: "https://api.github.com/copilot_internal/v2/token",
		SubscriptionAllowedModels: []string{
			"gpt-4o", "gpt-4o-mini",
			"claude-3-5-sonnet", "claude-3-7-sonnet",
			"o1", "o3-mini",
		},
	},
	"gemini": {
		ID:          "gemini",
		DisplayName: "Google Gemini",
		FlowType:    FlowImportOnly,
		ImportPath:  "~/.gemini/oauth_creds.json",
		RefreshURL:  "https://oauth2.googleapis.com/token",
		SubscriptionAllowedModels: []string{
			"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite",
		},
	},
}

// aliases maps user-typed names to canonical provider IDs.
var aliases = map[string]string{
	"openai":         "openai",
	"anthropic":      "anthropic",
	"claude":         "anthropic",
	"copilot":        "github-copilot",
	"github-copilot": "github-copilot",
	"gemini":         "gemini",
	"google":         "gemini",
}

// ErrUnknownProvider is returned when Resolve can't map the input.
var ErrUnknownProvider = errors.New("unknown provider")

// Resolve maps a possibly-aliased provider name (case-insensitive, trimmed)
// to its canonical ID. Returns ErrUnknownProvider for inputs that don't
// match any alias.
func Resolve(name string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	id, ok := aliases[key]
	if !ok {
		return "", ErrUnknownProvider
	}
	return id, nil
}
