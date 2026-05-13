package imports

import (
	"encoding/json"
	"fmt"
	"os"

	"sage-router/internal/auth"
)

// copilotFile is the (greatly simplified) shape of ~/.copilot/settings.json.
// We only care about the GitHub OAuth token under tokens["github.com"];
// the rest of the file is Copilot-specific settings sage-router doesn't
// touch.
type copilotFile struct {
	Tokens map[string]struct {
		OAuthToken string `json:"oauth_token"`
		User       string `json:"user,omitempty"`
	} `json:"tokens"`
}

// parseCopilotFile produces a Credential where:
//   - AccessToken is left empty (no short-lived Copilot bearer in the file).
//     The refresh loop or first request will mint one via the 2-step flow.
//   - RefreshToken is the long-lived GitHub OAuth token. The Copilot
//     refresh dispatcher (refresh/copilot.go) uses this in the
//     Authorization: token <gh_token> header.
//   - ExpiresAt is zero (already-expired), so AcquireCredential treats it
//     as needing a refresh on the first AcquireCredential call.
//
// User name (if present) flows into ExtraData for the dashboard.
//
// Caller (the import handler in M2.8) is expected to invoke
// refresh.Refresh after this returns, so the resulting connection has a
// usable AccessToken before it ever serves traffic. This matches AC17's
// promise that an imported-but-expired credential immediately gets the
// refresh loop's attention.
func parseCopilotFile(path string) (*auth.Credential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read copilot file: %w", err)
	}
	var f copilotFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse copilot file: %w", err)
	}
	gh, ok := f.Tokens["github.com"]
	if !ok || gh.OAuthToken == "" {
		return nil, ErrEmptyToken
	}

	cred := &auth.Credential{
		// AccessToken intentionally empty — see comment above.
		RefreshToken: gh.OAuthToken,
	}
	if gh.User != "" {
		cred.AccountID = gh.User
		cred.ExtraData = map[string]any{"github_user": gh.User}
	}
	return cred, nil
}
