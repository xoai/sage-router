package oauth

import (
	"strings"
	"testing"
)

func TestGenerateVerifier_LengthAndCharset(t *testing.T) {
	v, err := GenerateVerifier()
	if err != nil {
		t.Fatalf("GenerateVerifier: %v", err)
	}
	// 32 raw bytes → 43 chars base64url (no padding).
	if len(v) != 43 {
		t.Errorf("verifier len = %d, want 43", len(v))
	}
	// RFC 7636 Section 4.1: unreserved set = A-Z / a-z / 0-9 / "-" / "." / "_" / "~".
	// Base64url uses only A-Z, a-z, 0-9, "-", "_". No "=" padding allowed.
	for _, r := range v {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			// ok
		default:
			t.Errorf("verifier contains illegal char %q", r)
		}
	}
}

func TestGenerateVerifier_Uniqueness(t *testing.T) {
	// 100 calls should give 100 distinct verifiers.
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		v, err := GenerateVerifier()
		if err != nil {
			t.Fatalf("GenerateVerifier(%d): %v", i, err)
		}
		if seen[v] {
			t.Errorf("duplicate verifier seen at iteration %d: %s", i, v)
		}
		seen[v] = true
	}
}

// TestGenerateChallenge_RFC7636_TestVector asserts the SHA256-then-base64url
// chain matches the published example from RFC 7636 Appendix B.
func TestGenerateChallenge_RFC7636_TestVector(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	want := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	got := GenerateChallenge(verifier)
	if got != want {
		t.Errorf("GenerateChallenge() = %q, want %q (RFC 7636 Appendix B)", got, want)
	}
}

func TestGenerateChallenge_Deterministic(t *testing.T) {
	v, err := GenerateVerifier()
	if err != nil {
		t.Fatal(err)
	}
	c1 := GenerateChallenge(v)
	c2 := GenerateChallenge(v)
	if c1 != c2 {
		t.Errorf("challenge for same verifier differs: %q vs %q", c1, c2)
	}
	if c1 == v {
		t.Errorf("challenge should differ from verifier")
	}
}

func TestGenerateState_LengthAndCharset(t *testing.T) {
	s, err := GenerateState()
	if err != nil {
		t.Fatalf("GenerateState: %v", err)
	}
	// 16 raw bytes → 32 hex chars.
	if len(s) != 32 {
		t.Errorf("state len = %d, want 32", len(s))
	}
	if strings.ContainsAny(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Errorf("state should be lowercase hex; got %s", s)
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			// ok
		default:
			t.Errorf("state contains non-hex char %q", r)
		}
	}
}

func TestGenerateState_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		s, err := GenerateState()
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Errorf("duplicate state at iteration %d: %s", i, s)
		}
		seen[s] = true
	}
}
