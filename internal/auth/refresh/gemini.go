package refresh

import (
	"context"

	"sage-router/internal/auth"
	"sage-router/internal/auth/providers"
)

// refreshGemini hits Google's OAuth token endpoint. The credential
// originated from gemini-cli (~/.gemini/oauth_creds.json) so the
// client_id in our registry is empty — Google's endpoint accepts
// refresh_token grant without a client_id when the token itself encodes
// the credential context. If a future Gemini change requires client_id,
// it can be added to the providers registry without code changes here.
func refreshGemini(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	cfg := providers.Providers["gemini"]
	url := cfg.RefreshURL
	if override, ok := tokenURLOverrides["gemini"]; ok {
		url = override
	}
	// Gemini's refresh sometimes wants the client_id from the original
	// gemini-cli registration; if Google's response rejects empty
	// client_id, the user should set it via ExtraData["client_id"] on
	// import. For now we pass whatever's in the registry (typically
	// empty for an import-only flow that obtained tokens externally).
	newCred, _, err := refreshViaOAuthForm(ctx, cred, url, cfg.ClientID)
	return newCred, err
}
