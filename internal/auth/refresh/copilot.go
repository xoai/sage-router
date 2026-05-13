package refresh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/providers"
)

// copilotTokenResponse is the shape of api.github.com/copilot_internal/v2/token.
// Notable difference vs. the OAuth-form providers: there's no
// `expires_in` (seconds-from-now); instead `expires_at` is a unix
// timestamp the server already computed.
type copilotTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// refreshCopilot performs the GitHub Copilot 2-step refresh:
//   1. Use the user's long-lived GitHub OAuth token (stored as
//      cred.RefreshToken because we don't get a "Copilot refresh token";
//      we use the GH token to mint fresh Copilot bearers) as the
//      `Authorization: token <gh_token>` header.
//   2. GET copilot_internal/v2/token returns a short-lived Copilot
//      bearer in `.token` and an absolute expiry in `.expires_at`.
//
// Outcome:
//   - cred.AccessToken ← parsed.Token (short-lived Copilot bearer)
//   - cred.RefreshToken ← unchanged (GH token is long-lived)
//   - cred.ExpiresAt   ← parsed.ExpiresAt minus the 5-min safety buffer
//
// Failure modes:
//   - 401 from GitHub means the GH token is dead → ErrRefreshTokenRevoked
//   - 5xx / network → wrapped error, retryable
func refreshCopilot(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	cfg := providers.Providers["github-copilot"]
	url := cfg.RefreshURL
	if override, ok := tokenURLOverrides["github-copilot"]; ok {
		url = override
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("refresh copilot: build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+cred.RefreshToken)
	req.Header.Set("Accept", "application/json")
	// Copilot's endpoint expects a User-Agent that identifies the client.
	// Anything non-empty works; we use a sage-router-specific UA so
	// GitHub's logs can correlate.
	req.Header.Set("User-Agent", "sage-router/subscription-auth")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh copilot: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("refresh copilot: github rejected token (%d): %w",
			resp.StatusCode, ErrRefreshTokenRevoked)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("refresh copilot: status %d: %s",
			resp.StatusCode, truncate(string(body), 500))
	}

	var parsed copilotTokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("refresh copilot: parse response: %w", err)
	}
	if parsed.Token == "" {
		return nil, fmt.Errorf("refresh copilot: empty token in response")
	}

	expiresAt := time.Unix(parsed.ExpiresAt, 0).Add(-expiryBuffer)
	return &auth.Credential{
		Provider:     cred.Provider,
		ConnectionID: cred.ConnectionID,
		AccessToken:  parsed.Token,
		RefreshToken: cred.RefreshToken, // GH token is long-lived; don't replace
		ExpiresAt:    expiresAt,
		AccountID:    cred.AccountID,
		ExtraData:    cred.ExtraData,
	}, nil
}
