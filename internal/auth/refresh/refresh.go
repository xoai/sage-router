// Package refresh implements per-provider OAuth-token refresh. It exposes
// a single dispatch entry point [Refresh] and a sentinel
// [ErrRefreshTokenRevoked] that callers (Connection.AcquireCredential in
// M2.5; the refresh loop in M2.9) can use to decide between transient
// retry and permanent auto-disable.
package refresh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"sage-router/internal/auth"
)

// expiryBuffer matches the value used by the OAuth flow code so a refreshed
// token reports the same "expires within 5 minutes" semantics as a freshly-
// minted one.
const expiryBuffer = 5 * time.Minute

// ErrRefreshTokenRevoked signals that the upstream rejected our refresh
// token as no longer valid (typically a 400 with `invalid_grant`). Callers
// MUST NOT retry on this — the user has to re-authenticate via auth login
// or auth import.
var ErrRefreshTokenRevoked = errors.New("refresh: refresh token revoked")

// ErrUnsupportedProvider is returned when Refresh is called for a provider
// that has no registered dispatcher.
var ErrUnsupportedProvider = errors.New("refresh: provider not supported")

// Func is the contract each per-provider implementation satisfies.
type Func func(ctx context.Context, cred *auth.Credential) (*auth.Credential, error)

// dispatchers is the registry of per-provider refresh implementations. The
// package init wires the default HTTP client; tests can override the
// individual dispatchers (or the httpClient package var) to point at mock
// servers without touching this map.
var dispatchers = map[string]Func{
	"openai":         refreshOpenAI,
	"anthropic":      refreshAnthropic,
	"gemini":         refreshGemini,
	"github-copilot": refreshCopilot,
}

// httpClient is package-level so tests can swap in a custom transport.
// Default timeout is generous because the refresh endpoint can be slow
// during provider incidents — better to wait than to give up early and
// thrash the refresh loop.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// Refresh dispatches the credential to its provider's refresh implementation.
// Returns a fresh *Credential with new tokens and an expires_at that
// includes the 5-minute safety buffer, OR an error.
//
// On ErrRefreshTokenRevoked, callers should:
//   - mark the connection as Errored
//   - increment refresh_failures (the auto-disable threshold lives at the
//     callsite, not here)
//   - surface a clear "please re-authenticate" message to the user
//
// On any other error (network, 5xx, parse), callers should retry via the
// usual executor retry path.
func Refresh(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	if cred == nil {
		return nil, fmt.Errorf("refresh: nil credential")
	}
	fn, ok := dispatchers[cred.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedProvider, cred.Provider)
	}
	if cred.RefreshToken == "" {
		return nil, fmt.Errorf("refresh: %s connection has no refresh_token", cred.Provider)
	}
	return fn(ctx, cred)
}

// computeExpiresAt applies the 5-minute safety buffer to a server-reported
// expires_in (seconds from now).
func computeExpiresAt(expiresInSeconds int) time.Time {
	return time.Now().Add(time.Duration(expiresInSeconds)*time.Second - expiryBuffer)
}
