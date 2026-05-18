package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"sage-router/internal/auth/oauth"
	"sage-router/internal/auth/providers"
)

// CodexCredentials holds detected Codex CLI OAuth credentials. Parallel
// to ClaudeCredentials; the dashboard's Add Provider → OpenAI →
// "Connect with Codex CLI (auto-detect)" button reads this from
// `~/.codex/auth.json` once at create time, then the standard
// subscription-auth path takes over (encrypted DB storage + refresh
// loop). See `internal/server/routes_api.go:238-260` for the
// create-time conversion to `auth_type=subscription`.
type CodexCredentials struct {
	AccessToken  string
	RefreshToken string
	// IDToken field removed in cycle 20260517-provider-auth-variants M2.6.3.
	// The RFC 8693 token-exchange chain that consumed it was wrong-path
	// (memory `f32bbc73`); CodexSubscriptionExecutor uses AccessToken directly.
	ExpiresAt        time.Time
	AccountID        string // chatgpt_account_id JWT claim from id_token; required for the ChatGPT-Account-ID header on /v1/* subscription calls. Mirrors internal/auth/imports/codex.go:50-55.
	SubscriptionType string // best-effort; usually empty — Codex auth.json has no tier field today
}

// CodexResult is the public-safe result of credential detection;
// never carries the raw token. Returned by `GET /api/detect/codex`
// and consumed by the dashboard's `detectCodex()` API wrapper.
type CodexResult struct {
	Found            bool   `json:"found"`
	SubscriptionType string `json:"subscription_type,omitempty"`
	Expired          bool   `json:"expired,omitempty"`
}

// codexFile mirrors the JSON shape that `internal/auth/imports/codex.go`
// parses for the manual-import path. Re-parsed here (detect package
// has no public types from imports) for ownership isolation; if drift
// becomes a maintenance burden, the type can move to a shared
// `auth/tokenfile` sub-package later.
type codexFile struct {
	Tokens struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		IDToken      string          `json:"id_token,omitempty"`
		// AccountID is the chatgpt_account_id used by chatgpt.com/backend-api/codex/responses
		// for the ChatGPT-Account-ID header. M2.6.3 prefers this when present
		// over the JWT-claim extraction path (codex auth.json carries it
		// directly — verified at M0.8). Older auth.json formats may have
		// it only in the id_token; we fall back to extraction in that case.
		AccountID string          `json:"account_id,omitempty"`
		ExpiresAt json.RawMessage `json:"expires_at,omitempty"`
	} `json:"tokens"`
}

// codexCredPaths returns candidate paths for Codex CLI credentials in
// preference order:
//
//  1. `$CODEX_HOME/auth.json` (env override; matches the import path's
//     `ImportPathEnvVar` at internal/auth/providers/registry.go).
//  2. `~/.codex/auth.json` (Unix-style — Linux, macOS, WSL).
//  3. Windows-native: `%APPDATA%\codex\auth.json` and
//     `$USERPROFILE\AppData\Roaming\codex\auth.json`.
//  4. WSL-from-Windows distro paths under `\\wsl$\<distro>\home\...`
//     (gated by `wslDistros` — tests can disable this group).
//
// Mirrors detect/claude.go's path-search strategy.
func codexCredPaths() []string {
	var paths []string

	if env := os.Getenv("CODEX_HOME"); env != "" {
		paths = append(paths, filepath.Join(env, "auth.json"))
	}

	home, _ := os.UserHomeDir()

	if home != "" {
		paths = append(paths, filepath.Join(home, ".codex", "auth.json"))
	}

	if runtime.GOOS == "windows" {
		appdata := os.Getenv("APPDATA")
		if appdata != "" {
			paths = append(paths, filepath.Join(appdata, "codex", "auth.json"))
		}
		if home != "" {
			paths = append(paths, filepath.Join(home, "AppData", "Roaming", "codex", "auth.json"))
		}

		userProfile := os.Getenv("USERPROFILE")
		if userProfile == "" && home != "" {
			userProfile = home
		}
		if userProfile != "" && len(wslDistros) > 0 {
			for _, distro := range wslDistros {
				wslPath := filepath.Join(`\\wsl$`, distro, "home")
				entries, err := os.ReadDir(wslPath)
				if err == nil {
					for _, e := range entries {
						if e.IsDir() {
							paths = append(paths, filepath.Join(wslPath, e.Name(), ".codex", "auth.json"))
						}
					}
				}
			}
		}
	}

	return paths
}

// DetectCodex checks for Codex CLI credentials on the local filesystem.
// Returns a safe result (never exposes tokens) and optionally the full
// credentials. Mirrors detect.DetectClaude's contract.
//
// Two-pass scan: prefer the first NON-expired file across all candidate
// paths. If every found file is expired, fall back to the first found
// so the caller can still surface the "expired — refresh" UX. Same
// pattern as DetectClaude in claude.go; keep aligned. See fix
// 20260514-claude-oauth-and-detect.
func DetectCodex() (CodexResult, *CodexCredentials) {
	var firstFound CodexResult
	var firstCreds *CodexCredentials
	haveFound := false

	for _, path := range codexCredPaths() {
		result, creds := tryReadCodexCredentials(path)
		if !result.Found {
			continue
		}
		if !result.Expired {
			return result, creds // fresh file wins immediately
		}
		if !haveFound {
			firstFound = result
			firstCreds = creds
			haveFound = true
		}
	}
	if haveFound {
		return firstFound, firstCreds // all expired; return the first
	}
	return CodexResult{Found: false}, nil
}

func tryReadCodexCredentials(path string) (CodexResult, *CodexCredentials) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CodexResult{Found: false}, nil
	}
	var f codexFile
	if err := json.Unmarshal(data, &f); err != nil {
		return CodexResult{Found: false}, nil
	}
	if f.Tokens.AccessToken == "" {
		return CodexResult{Found: false}, nil
	}

	expiresAt := parseCodexExpiry(f.Tokens.ExpiresAt)
	expired := !expiresAt.IsZero() && time.Now().After(expiresAt)

	result := CodexResult{
		Found:   true,
		Expired: expired,
		// SubscriptionType stays empty — codex auth.json doesn't carry a
		// tier field today. The dashboard's render block handles empty
		// via `&& subscription_type` short-circuit, same as Claude's
		// no-badge case.
	}
	full := &CodexCredentials{
		AccessToken:  f.Tokens.AccessToken,
		RefreshToken: f.Tokens.RefreshToken,
		ExpiresAt:    expiresAt,
	}
	// Best-effort AccountID extraction — the ChatGPT-Account-ID header
	// downstream (executor + listers) needs this for subscription calls.
	// Codex CLI's auth.json carries `tokens.account_id` directly (verified
	// M0.8 — see m0-baseline.md Finding 4 evidence), so prefer that over
	// JWT claim extraction. Cycle 20260517-provider-auth-variants M2.6.3
	// simplifies the path: no id_token field in CodexCredentials anymore.
	if f.Tokens.AccountID != "" {
		full.AccountID = f.Tokens.AccountID
	} else if f.Tokens.IDToken != "" {
		// JWT-claim fallback for older Codex auth.json shapes that didn't
		// include the account_id field directly.
		if def, ok := providers.Providers["openai"]; ok && def.AccountIDClaim != "" {
			if id, err := oauth.ExtractIDTokenClaim(f.Tokens.IDToken, def.AccountIDClaim); err == nil {
				full.AccountID = id
			}
		}
	}
	return result, full
}

// parseCodexExpiry mirrors `internal/auth/imports/codex.go:parseFlexibleExpiry`.
// Codex CLI has emitted `expires_at` in three formats across versions:
// unix seconds (int), unix milliseconds (int), and ISO-8601 string.
// Heuristic: values > 1e12 are milliseconds (year 2286 in seconds is
// ~10^10; 10^12 is firmly in ms territory). Returns zero time on parse
// failure (caller treats as "expiry unknown" — never marks expired).
func parseCodexExpiry(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
