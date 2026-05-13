package auth

import (
	"testing"
	"time"
)

func TestCredentialExpiresWithin(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		expiresAt time.Time
		within    time.Duration
		want      bool
	}{
		{"far future", now.Add(1 * time.Hour), 5 * time.Minute, false},
		{"just outside window", now.Add(6 * time.Minute), 5 * time.Minute, false},
		{"just inside window", now.Add(4 * time.Minute), 5 * time.Minute, true},
		{"already expired", now.Add(-1 * time.Minute), 5 * time.Minute, true},
		{"zero time", time.Time{}, 5 * time.Minute, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Credential{ExpiresAt: tt.expiresAt}
			got := c.ExpiresWithin(tt.within)
			if got != tt.want {
				t.Errorf("ExpiresWithin(%v) = %v, want %v (expiresAt=%v, now=%v)",
					tt.within, got, tt.want, tt.expiresAt, now)
			}
		})
	}
}

func TestCredentialExtraHeaders(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		account  string
		want     map[string]string
	}{
		{"openai with account ID", "openai", "acct-abc-123", map[string]string{"ChatGPT-Account-ID": "acct-abc-123"}},
		{"openai without account ID", "openai", "", nil},
		{"anthropic ignores account ID", "anthropic", "acct-xyz", nil},
		{"gemini returns nil", "gemini", "anything", nil},
		{"github-copilot returns nil", "github-copilot", "anything", nil},
		{"unknown provider returns nil", "weird", "x", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Credential{Provider: tt.provider, AccountID: tt.account}
			got := c.ExtraHeaders()
			if len(got) != len(tt.want) {
				t.Fatalf("ExtraHeaders len = %d, want %d (got %v)", len(got), len(tt.want), got)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("ExtraHeaders[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestCredentialMask(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		accessToken string
		want        string
	}{
		{"long token", "openai", "sk-abc-very-long-token-xyz1", "openai:****xyz1"},
		{"exactly 4 chars", "anthropic", "abcd", "anthropic:****abcd"},
		{"3 chars masked", "gemini", "abc", "gemini:****"},
		{"empty token", "copilot", "", "copilot:****"},
		{"empty provider", "", "sometoken1234", ":****1234"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Credential{Provider: tt.provider, AccessToken: tt.accessToken}
			got := c.Mask()
			if got != tt.want {
				t.Errorf("Mask() = %q, want %q", got, tt.want)
			}
		})
	}
}
