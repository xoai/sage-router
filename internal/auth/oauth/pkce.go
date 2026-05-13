package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// PKCE constants (RFC 7636 Section 4.1).
const (
	verifierBytes = 32 // 43 chars after base64url-no-padding
	stateBytes    = 16 // 32 chars hex
)

// GenerateVerifier returns a fresh PKCE code_verifier per RFC 7636 §4.1:
// 32 random bytes encoded as base64url with no padding (43 chars).
func GenerateVerifier() (string, error) {
	buf := make([]byte, verifierBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// GenerateChallenge derives the PKCE code_challenge from a verifier per
// RFC 7636 §4.2 using S256:
// challenge = base64url-no-padding(SHA256(verifier-as-ascii-bytes)).
//
// Verifier matches the RFC's "code_verifier" charset — base64url chars
// are all ASCII, so SHA256-ing the string's bytes is identical to SHA256-ing
// the ASCII representation.
func GenerateChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// GenerateState returns a fresh CSRF state token: 16 random bytes encoded
// as lowercase hex (32 chars). Hex encoding (rather than base64url) is
// chosen because state is sometimes copy-pasted into URLs by humans during
// the headless paste-redirect fallback flow, and hex is harder to confuse
// (no case sensitivity, no special chars).
func GenerateState() (string, error) {
	buf := make([]byte, stateBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
