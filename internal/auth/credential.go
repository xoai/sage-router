package auth

import (
	"fmt"
	"time"
)

// Credential is the in-memory representation of a subscription token,
// decrypted from the connections table. The DB columns are encrypted at
// rest via the existing store-layer AES-256-GCM; this struct only ever
// holds plaintext.
type Credential struct {
	Provider     string
	ConnectionID string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	AccountID    string         // populated for providers that expose one (e.g., OpenAI's chatgpt_account_id JWT claim)
	ExtraData    map[string]any // future-proof; provider-specific fields from the token response
}

// ExpiresWithin reports whether the credential will expire within d from now.
// A zero ExpiresAt is treated as already expired.
func (c *Credential) ExpiresWithin(d time.Duration) bool {
	return time.Until(c.ExpiresAt) < d
}

// ExtraHeaders returns provider-specific request headers beyond the
// Authorization: Bearer header injected by the executor. Currently only
// OpenAI requires one: ChatGPT-Account-ID when an account ID is known.
func (c *Credential) ExtraHeaders() map[string]string {
	switch c.Provider {
	case "openai":
		if c.AccountID != "" {
			return map[string]string{"ChatGPT-Account-ID": c.AccountID}
		}
	}
	return nil
}

// Mask returns a redacted representation safe for log output:
// "<provider>:****<last4>" for tokens >= 4 characters, or
// "<provider>:****" for shorter/empty tokens. NEVER call fmt.Stringer
// indirectly with the raw struct — always use Mask().
func (c *Credential) Mask() string {
	if len(c.AccessToken) >= 4 {
		return fmt.Sprintf("%s:****%s", c.Provider, c.AccessToken[len(c.AccessToken)-4:])
	}
	return fmt.Sprintf("%s:****", c.Provider)
}
