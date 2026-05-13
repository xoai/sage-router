package imports

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"sage-router/internal/auth"
)

// claudeFile is the shape of ~/.claude/.credentials.json. expiresAt is
// always milliseconds since epoch in this file (unlike Codex's variable
// format). scopes and subscriptionType are advisory metadata we surface
// via ExtraData for the dashboard.
type claudeFile struct {
	ClaudeAiOauth struct {
		AccessToken      string   `json:"accessToken"`
		RefreshToken     string   `json:"refreshToken"`
		ExpiresAt        int64    `json:"expiresAt"` // unix milliseconds
		Scopes           []string `json:"scopes"`
		SubscriptionType string   `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

func parseClaudeFile(path string) (*auth.Credential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read claude file: %w", err)
	}
	var f claudeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse claude file: %w", err)
	}
	entry := f.ClaudeAiOauth
	if entry.AccessToken == "" {
		return nil, ErrEmptyToken
	}

	cred := &auth.Credential{
		AccessToken:  entry.AccessToken,
		RefreshToken: entry.RefreshToken,
	}
	if entry.ExpiresAt > 0 {
		cred.ExpiresAt = time.UnixMilli(entry.ExpiresAt)
	}
	// Surface scope/tier metadata for the dashboard. Useful when the user
	// has Max vs Pro and wants the badge to reflect that.
	if entry.SubscriptionType != "" || len(entry.Scopes) > 0 {
		cred.ExtraData = map[string]any{}
		if entry.SubscriptionType != "" {
			cred.ExtraData["subscription_type"] = entry.SubscriptionType
		}
		if len(entry.Scopes) > 0 {
			cred.ExtraData["scopes"] = entry.Scopes
		}
	}
	return cred, nil
}
