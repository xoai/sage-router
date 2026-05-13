package imports

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/oauth"
	"sage-router/internal/auth/providers"
)

// codexFile is the on-disk shape of ~/.codex/auth.json. The OPENAI_API_KEY
// at the top level is intentionally ignored — that's the API-key fallback
// path, separate from subscription auth.
type codexFile struct {
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token,omitempty"`
		// ExpiresAt may be a unix timestamp (seconds OR milliseconds) or
		// an ISO-8601 string depending on which Codex version wrote the
		// file. We try seconds first, then ms, then ISO.
		ExpiresAt json.RawMessage `json:"expires_at,omitempty"`
	} `json:"tokens"`
}

func parseCodexFile(path string) (*auth.Credential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read codex file: %w", err)
	}
	var f codexFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse codex file: %w", err)
	}
	if f.Tokens.AccessToken == "" {
		return nil, ErrEmptyToken
	}

	cred := &auth.Credential{
		AccessToken:  f.Tokens.AccessToken,
		RefreshToken: f.Tokens.RefreshToken,
	}
	if expires := parseFlexibleExpiry(f.Tokens.ExpiresAt); !expires.IsZero() {
		cred.ExpiresAt = expires
	}

	// Extract account_id from the id_token JWT claim if present.
	if f.Tokens.IDToken != "" {
		claim := providers.Providers["openai"].AccountIDClaim
		if id, err := oauth.ExtractIDTokenClaim(f.Tokens.IDToken, claim); err == nil {
			cred.AccountID = id
		}
		// Decode errors are non-fatal — the credential still works.
	}
	return cred, nil
}

// parseFlexibleExpiry handles the variants of expires_at Codex has used
// across versions: unix seconds (int), unix milliseconds (int), and
// ISO-8601 string. Returns zero time on parse failure (caller treats as
// "expiry unknown" and lets the refresh loop sort it out).
func parseFlexibleExpiry(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	// Try as integer first.
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		// Heuristic: if the value is > 10^12, it's milliseconds.
		// (Year 2286 in seconds is ~10^10; 10^12 is firmly in ms territory.)
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	// Try as ISO string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
