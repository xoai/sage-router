package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractIDTokenClaim base64url-decodes the JWT payload segment (no
// signature verification — the token came from the user's own OAuth flow
// and is trusted by definition) and returns the named claim as a string.
//
// Returns "" (without error) when the claim is missing. Returns a non-nil
// error when the JWT is malformed (wrong segment count, undecodable
// payload, non-JSON payload).
//
// Use case: OpenAI's id_token carries a chatgpt_account_id claim
// (configured in providers.ProviderConfig.AccountIDClaim) that we surface
// to the dashboard and inject as the ChatGPT-Account-ID request header.
func ExtractIDTokenClaim(token, claim string) (string, error) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return "", fmt.Errorf("jwt: expected 3 segments (header.payload.signature), got %d", len(segments))
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		// Tolerate tokens that pad with '=' (some providers do, RFC 7515
		// allows both). Fall through to padded decode.
		payloadRaw, err = base64.URLEncoding.DecodeString(segments[1])
		if err != nil {
			return "", fmt.Errorf("jwt: decode payload: %w", err)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		return "", fmt.Errorf("jwt: parse payload JSON: %w", err)
	}
	v, ok := payload[claim]
	if !ok {
		return "", nil
	}
	switch s := v.(type) {
	case string:
		return s, nil
	case float64:
		// JSON numbers come through as float64; render with %g for the
		// rare case a claim is numeric.
		return fmt.Sprintf("%g", s), nil
	default:
		// Arrays, objects, booleans — convert via fmt.
		return fmt.Sprintf("%v", v), nil
	}
}

