package auth

import (
	"strings"
	"testing"
	"time"
	"unicode"
)

// helper to create a Manager with a known password and test secrets.
func newTestManager(t *testing.T, password string) *Manager {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	return NewManager(hash, []byte("test-jwt-secret"), []byte("test-hmac-secret"))
}

// ── Password tests ──

func TestPasswordHashAndCheck(t *testing.T) {
	mgr := newTestManager(t, "correct-password")

	if !mgr.CheckPassword("correct-password") {
		t.Fatal("expected CheckPassword to return true for correct password")
	}
	if mgr.CheckPassword("wrong-password") {
		t.Fatal("expected CheckPassword to return false for wrong password")
	}
}

func TestGenerateRandomPassword(t *testing.T) {
	const charset = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

	for _, length := range []int{8, 16, 32, 64} {
		pw := GenerateRandomPassword(length)
		if len(pw) != length {
			t.Errorf("expected length %d, got %d for password %q", length, len(pw), pw)
		}
		for i, ch := range pw {
			if !unicode.IsLetter(ch) && !unicode.IsDigit(ch) {
				t.Errorf("character at index %d is not alphanumeric: %q", i, ch)
			}
			if !strings.ContainsRune(charset, ch) {
				t.Errorf("character %q at index %d is not in the expected charset", ch, i)
			}
		}
	}
}

// ── JWT tests ──

func TestJWTGenerateAndValidate(t *testing.T) {
	mgr := newTestManager(t, "pw")

	token, err := mgr.GenerateToken(1 * time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	valid, err := mgr.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken returned error: %v", err)
	}
	if !valid {
		t.Fatal("expected token to be valid")
	}
}

func TestJWTExpired(t *testing.T) {
	mgr := newTestManager(t, "pw")

	// Generate a token that is already expired (negative duration).
	token, err := mgr.GenerateToken(-1 * time.Second)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}

	valid, err := mgr.ValidateToken(token)
	if valid {
		t.Fatal("expected expired token to be invalid")
	}
	if err == nil {
		t.Fatal("expected an error for expired token")
	}
}

func TestJWTInvalidSignature(t *testing.T) {
	// Generate a token with one secret, then validate with a different secret.
	hash, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}

	signer := NewManager(hash, []byte("secret-one"), []byte("hmac"))
	verifier := NewManager(hash, []byte("secret-two"), []byte("hmac"))

	token, err := signer.GenerateToken(1 * time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}

	valid, err := verifier.ValidateToken(token)
	if valid {
		t.Fatal("expected token signed with different secret to be invalid")
	}
	if err == nil {
		t.Fatal("expected an error for mismatched signature")
	}
}

// ── API key tests ──

func TestAPIKeyGenerate(t *testing.T) {
	mgr := newTestManager(t, "pw")

	plainKey, keyHash, prefix, err := mgr.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey failed: %v", err)
	}
	if !strings.HasPrefix(plainKey, "sk-sage-") {
		t.Errorf("expected key to start with 'sk-sage-', got %q", plainKey)
	}
	if !ValidateAPIKeyFormat(plainKey) {
		t.Error("expected ValidateAPIKeyFormat to return true for generated key")
	}
	if keyHash == "" {
		t.Error("expected non-empty key hash")
	}
	if prefix == "" {
		t.Error("expected non-empty prefix")
	}
	if !strings.HasPrefix(plainKey, prefix) {
		t.Errorf("expected plain key %q to start with prefix %q", plainKey, prefix)
	}
}

func TestAPIKeyValidateHMAC(t *testing.T) {
	mgr := newTestManager(t, "pw")

	plainKey, keyHash, _, err := mgr.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey failed: %v", err)
	}

	// Re-hash the plain key and confirm it matches the original hash.
	recomputed := mgr.HashAPIKey(plainKey)
	if recomputed != keyHash {
		t.Errorf("expected HMAC hash to match: got %q, want %q", recomputed, keyHash)
	}
}

func TestAPIKeyInvalidHMAC(t *testing.T) {
	mgr := newTestManager(t, "pw")

	plainKey, _, _, err := mgr.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey failed: %v", err)
	}

	originalHash := mgr.HashAPIKey(plainKey)

	// Tamper with the key by flipping the last character.
	tampered := plainKey[:len(plainKey)-1] + "X"
	tamperedHash := mgr.HashAPIKey(tampered)

	if tamperedHash == originalHash {
		t.Error("expected HMAC of tampered key to differ from original")
	}
}
