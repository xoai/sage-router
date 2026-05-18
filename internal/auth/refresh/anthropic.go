package refresh

import (
	"context"

	"sage-router/internal/auth"
	"sage-router/internal/auth/providers"
)

func refreshAnthropic(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	cfg := providers.Providers["anthropic"]
	url := cfg.RefreshURL
	if override, ok := tokenURLOverrides["anthropic"]; ok {
		url = override
	}
	newCred, _, err := refreshViaOAuthForm(ctx, cred, url, cfg.ClientID)
	return newCred, err
}
