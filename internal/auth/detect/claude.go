package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ClaudeCredentials holds detected Claude Code OAuth credentials.
type ClaudeCredentials struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        time.Time
	SubscriptionType string
}

// ClaudeResult is the public-safe result of credential detection.
type ClaudeResult struct {
	Found            bool   `json:"found"`
	SubscriptionType string `json:"subscription_type,omitempty"`
	Expired          bool   `json:"expired,omitempty"`
}

// credentialsFile represents the on-disk JSON structure.
type credentialsFile struct {
	ClaudeAiOauth *oauthEntry `json:"claudeAiOauth"`
}

type oauthEntry struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // milliseconds since epoch
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType"`
}

// claudeCredPaths returns all candidate paths for Claude Code credentials.
// Tries multiple locations to handle Linux, macOS, Windows, and WSL scenarios.
func claudeCredPaths() []string {
	var paths []string
	home, _ := os.UserHomeDir()

	// Unix-style path (Linux, macOS, WSL)
	if home != "" {
		paths = append(paths, filepath.Join(home, ".claude", ".credentials.json"))
	}

	if runtime.GOOS == "windows" {
		// Windows native: %APPDATA%\claude\.credentials.json
		appdata := os.Getenv("APPDATA")
		if appdata != "" {
			paths = append(paths, filepath.Join(appdata, "claude", ".credentials.json"))
		}
		if home != "" {
			paths = append(paths, filepath.Join(home, "AppData", "Roaming", "claude", ".credentials.json"))
		}

		// WSL paths accessible from Windows (common distros)
		userProfile := os.Getenv("USERPROFILE")
		if userProfile == "" && home != "" {
			userProfile = home
		}
		if userProfile != "" {
			// Try common WSL user home paths via \\wsl$. Distro list
			// shared with codex.go via `wslDistros` in wsl.go; tests can
			// clear it via `DisableWSLForTesting`.
			for _, distro := range wslDistros {
				wslPath := filepath.Join(`\\wsl$`, distro, "home")
				entries, err := os.ReadDir(wslPath)
				if err == nil {
					for _, e := range entries {
						if e.IsDir() {
							paths = append(paths, filepath.Join(wslPath, e.Name(), ".claude", ".credentials.json"))
						}
					}
				}
			}
		}
	}

	return paths
}

// DetectClaude checks for Claude Code credentials on the local filesystem.
// Returns a safe result (never exposes tokens) and optionally the full credentials.
//
// Two-pass scan: prefer the first NON-expired file across all candidate
// paths. If every found file is expired, fall back to the first found
// so the caller can still surface the "expired — refresh" UX (per
// routes_api.go:253). Without this, on a Windows machine with stale
// native credentials AND fresh WSL credentials, the stale file would
// mask the fresh one. See fix 20260514-claude-oauth-and-detect.
// Mirrored at detect/codex.go DetectCodex — keep aligned.
//
// Caveat: when EVERY found file is expired, the fallback returns the
// FIRST in path-order. On Windows that's the native AppData path —
// the "restart Claude Code to refresh" message will point to the
// Windows install even if the user's actively-used Claude Code lives
// in WSL. Acceptable for now; a future refinement could include
// path source in the error message.
func DetectClaude() (ClaudeResult, *ClaudeCredentials) {
	var firstFound ClaudeResult
	var firstCreds *ClaudeCredentials
	haveFound := false

	for _, path := range claudeCredPaths() {
		result, creds := tryReadCredentials(path)
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
	return ClaudeResult{Found: false}, nil
}

func tryReadCredentials(path string) (ClaudeResult, *ClaudeCredentials) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ClaudeResult{Found: false}, nil
	}

	var creds credentialsFile
	if err := json.Unmarshal(data, &creds); err != nil {
		return ClaudeResult{Found: false}, nil
	}

	if creds.ClaudeAiOauth == nil || creds.ClaudeAiOauth.AccessToken == "" {
		return ClaudeResult{Found: false}, nil
	}

	oauth := creds.ClaudeAiOauth
	expiresAt := time.UnixMilli(oauth.ExpiresAt)
	expired := time.Now().After(expiresAt)

	result := ClaudeResult{
		Found:            true,
		SubscriptionType: oauth.SubscriptionType,
		Expired:          expired,
	}

	full := &ClaudeCredentials{
		AccessToken:      oauth.AccessToken,
		RefreshToken:     oauth.RefreshToken,
		ExpiresAt:        expiresAt,
		SubscriptionType: oauth.SubscriptionType,
	}

	return result, full
}
