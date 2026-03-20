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
			// Try common WSL user home paths via \\wsl$
			for _, distro := range []string{"Ubuntu", "Ubuntu-22.04", "Ubuntu-24.04", "Debian", "kali-linux"} {
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
func DetectClaude() (ClaudeResult, *ClaudeCredentials) {
	for _, path := range claudeCredPaths() {
		result, creds := tryReadCredentials(path)
		if result.Found {
			return result, creds
		}
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
