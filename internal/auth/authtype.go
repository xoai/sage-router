package auth

import "strings"

// Canonical AuthType values stored on connections.
//
// The Connection.AuthType column historically used "api_key" and "oauth"
// alongside the canonical forms. NormalizeAuthType maps any of those
// inputs to the canonical vocabulary so that downstream code only needs
// to switch on one set of literals.
const (
	AuthTypeAPIKey       = "apikey"
	AuthTypeSubscription = "subscription"
	AuthTypeAutoDetect   = "auto_detect"
	AuthTypeNone         = "none"
)

// NormalizeAuthType returns the canonical form of an AuthType string.
// Empty input maps to "none". Unknown values pass through unchanged so
// callers (e.g., the executor switch statement) can reject them with a
// clear error rather than silently masking misconfigurations.
func NormalizeAuthType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "api_key", "apikey":
		return AuthTypeAPIKey
	case "oauth", "subscription":
		return AuthTypeSubscription
	case "auto_detect":
		return AuthTypeAutoDetect
	case "", "none":
		return AuthTypeNone
	default:
		return s
	}
}

// IsCanonicalAuthType reports whether s is already in canonical form.
// Useful for "did this come from a normalized read path?" checks.
func IsCanonicalAuthType(s string) bool {
	switch s {
	case AuthTypeAPIKey, AuthTypeSubscription, AuthTypeAutoDetect, AuthTypeNone:
		return true
	default:
		return false
	}
}
