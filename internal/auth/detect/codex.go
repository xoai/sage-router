package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// CodexCredentials holds detected Codex CLI OAuth credentials. Parallel
// to ClaudeCredentials; the dashboard's Add Provider → OpenAI →
// "Connect with Codex CLI (auto-detect)" button reads this from
// `~/.codex/auth.json` once at create time, then the standard
// subscription-auth path takes over (encrypted DB storage + refresh
// loop). See `internal/server/routes_api.go:238-260` for the
// create-time conversion to `auth_type=subscription`.
type CodexCredentials struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        time.Time
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
		ExpiresAt    json.RawMessage `json:"expires_at,omitempty"`
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
func DetectCodex() (CodexResult, *CodexCredentials) {
	for _, path := range codexCredPaths() {
		result, creds := tryReadCodexCredentials(path)
		if result.Found {
			return result, creds
		}
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
