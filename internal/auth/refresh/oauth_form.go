package refresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"sage-router/internal/auth"
)

// formTokenResponse is the canonical shape returned by OpenAI, Anthropic,
// and Gemini's refresh-token endpoints. Each may include extra fields
// (id_token, scope, etc.) that we ignore.
type formTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`

	// Error fields from the OAuth 2.0 spec; populated on 4xx responses.
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// refreshViaOAuthForm is the shared implementation for OpenAI / Anthropic /
// Gemini. They all follow RFC 6749 §6 (refresh_token grant): form-encoded
// POST with grant_type, refresh_token, client_id.
//
// The result Credential preserves Provider and AccountID from the input
// and gets fresh tokens + expires_at. Note: providers MAY rotate the
// refresh_token (returning a new one), in which case we replace it; or
// they may omit it, in which case we keep the old one.
func refreshViaOAuthForm(ctx context.Context, cred *auth.Credential, tokenURL, clientID string) (*auth.Credential, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cred.RefreshToken)
	if clientID != "" {
		form.Set("client_id", clientID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("refresh %s: build request: %w", cred.Provider, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh %s: %w", cred.Provider, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// Parse JSON eagerly — providers return JSON for both success and 4xx
	// error bodies. If parse fails we fall back to a generic error using
	// the raw status code (truncated body).
	var parsed formTokenResponse
	parseErr := json.Unmarshal(body, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// invalid_grant means our refresh_token is dead — distinct sentinel
		// so callers can auto-disable rather than retry.
		if parsed.Error == "invalid_grant" || parsed.Error == "invalid_token" {
			return nil, fmt.Errorf("refresh %s (%s): %w",
				cred.Provider, parsed.ErrorDescription, ErrRefreshTokenRevoked)
		}
		msg := parsed.Error
		if msg == "" {
			msg = truncate(string(body), 500)
		}
		return nil, fmt.Errorf("refresh %s: status %d: %s", cred.Provider, resp.StatusCode, msg)
	}

	if parseErr != nil {
		return nil, fmt.Errorf("refresh %s: parse response: %w (body: %s)",
			cred.Provider, parseErr, truncate(string(body), 200))
	}
	if parsed.AccessToken == "" {
		return nil, errors.New("refresh: provider returned 200 but no access_token")
	}

	// Preserve the old refresh_token when the provider doesn't rotate.
	newRefreshToken := parsed.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = cred.RefreshToken
	}

	return &auth.Credential{
		Provider:     cred.Provider,
		ConnectionID: cred.ConnectionID,
		AccessToken:  parsed.AccessToken,
		RefreshToken: newRefreshToken,
		ExpiresAt:    computeExpiresAt(parsed.ExpiresIn),
		AccountID:    cred.AccountID,
		ExtraData:    cred.ExtraData,
	}, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}
