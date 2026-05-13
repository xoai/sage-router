package refresh

import (
	"context"

	"sage-router/internal/auth"
	"sage-router/internal/auth/providers"
)

// refreshOpenAI runs the standard RFC 6749 §6 refresh_token grant against
// OpenAI's token endpoint. Reads URL + client_id from the provider registry
// (test override via package-level httpClient + a custom URL is fine, but
// for symmetry with the other providers we go through the registry — tests
// override via tokenURLOverride in registry_override.go).
func refreshOpenAI(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	cfg := providers.Providers["openai"]
	url := cfg.RefreshURL
	if override, ok := tokenURLOverrides["openai"]; ok {
		url = override
	}
	return refreshViaOAuthForm(ctx, cred, url, cfg.ClientID)
}
