package refresh

import (
	"context"

	"sage-router/internal/auth"
	"sage-router/internal/auth/providers"
)

// refreshOpenAI runs the standard RFC 6749 §6 refresh_token grant against
// OpenAI's token endpoint and returns the rotated credential.
//
// Cycle 20260517-provider-auth-variants M2.6.2: the RFC 8693 token-exchange
// chain (Flow.ExchangeForAPIKey called on every refresh) was REMOVED.
// The exchange targeted api.openai.com/v1/responses which is unreachable
// for ChatGPT subscribers (memory `f32bbc73`); the working path now used
// by CodexSubscriptionExecutor at chatgpt.com/backend-api/codex/responses
// consumes the PKCE access_token directly, so no exchange step is needed.
func refreshOpenAI(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	cfg := providers.Providers["openai"]
	url := cfg.RefreshURL
	if override, ok := tokenURLOverrides["openai"]; ok {
		url = override
	}
	newCred, _, err := refreshViaOAuthForm(ctx, cred, url, cfg.ClientID)
	if err != nil {
		return nil, err
	}
	return newCred, nil
}
