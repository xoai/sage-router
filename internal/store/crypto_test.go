package store

import (
	"strings"
	"testing"
)

func TestDeriveKeyLength(t *testing.T) {
	key := DeriveKey("my-secret-password")
	if len(key) != 32 {
		t.Fatalf("DeriveKey: expected 32 bytes, got %d", len(key))
	}
}

func TestDeriveKeyDeterministic(t *testing.T) {
	k1 := DeriveKey("same-password")
	k2 := DeriveKey("same-password")

	if len(k1) != len(k2) {
		t.Fatalf("DeriveKey: lengths differ: %d vs %d", len(k1), len(k2))
	}
	for i := range k1 {
		if k1[i] != k2[i] {
			t.Fatalf("DeriveKey: byte %d differs: %02x vs %02x", i, k1[i], k2[i])
		}
	}
}

func TestDeriveKeyDifferentPasswords(t *testing.T) {
	k1 := DeriveKey("password-one")
	k2 := DeriveKey("password-two")

	same := true
	for i := range k1 {
		if k1[i] != k2[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("DeriveKey: different passwords produced the same key")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := DeriveKey("test-password")

	tests := []struct {
		name      string
		plaintext string
	}{
		{name: "empty string", plaintext: ""},
		{name: "hello", plaintext: "hello"},
		{name: "api key", plaintext: "sk-real-api-key-123"},
		{name: "4KB string", plaintext: strings.Repeat("A", 4096)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ciphertext, err := Encrypt(tt.plaintext, key)
			if err != nil {
				t.Fatalf("Encrypt failed: %v", err)
			}

			got, err := Decrypt(ciphertext, key)
			if err != nil {
				t.Fatalf("Decrypt failed: %v", err)
			}

			if got != tt.plaintext {
				t.Errorf("round-trip mismatch: got %q, want %q", got, tt.plaintext)
			}
		})
	}
}

func TestEncryptDecryptEmpty(t *testing.T) {
	key := DeriveKey("empty-test-password")

	ciphertext, err := Encrypt("", key)
	if err != nil {
		t.Fatalf("Encrypt empty string failed: %v", err)
	}

	got, err := Decrypt(ciphertext, key)
	if err != nil {
		t.Fatalf("Decrypt empty string failed: %v", err)
	}

	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestEncryptDecryptLong(t *testing.T) {
	key := DeriveKey("long-test-password")
	plaintext := strings.Repeat("B", 4096) // 4KB

	ciphertext, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt 4KB string failed: %v", err)
	}

	got, err := Decrypt(ciphertext, key)
	if err != nil {
		t.Fatalf("Decrypt 4KB string failed: %v", err)
	}

	if got != plaintext {
		t.Errorf("round-trip mismatch: got length %d, want length %d", len(got), len(plaintext))
	}
}

func TestDecryptWrongKey(t *testing.T) {
	key1 := DeriveKey("correct-password")
	key2 := DeriveKey("wrong-password")

	ciphertext, err := Encrypt("secret-data", key1)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	_, err = Decrypt(ciphertext, key2)
	if err == nil {
		t.Fatal("Decrypt with wrong key: expected error, got nil")
	}
}

func TestEncryptNonDeterministic(t *testing.T) {
	key := DeriveKey("nonce-test-password")
	plaintext := "same-value"

	c1, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("first Encrypt failed: %v", err)
	}

	c2, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("second Encrypt failed: %v", err)
	}

	if c1 == c2 {
		t.Error("Encrypt produced identical ciphertexts for the same plaintext; expected different nonces")
	}

	// Both must still decrypt correctly.
	d1, err := Decrypt(c1, key)
	if err != nil {
		t.Fatalf("Decrypt c1 failed: %v", err)
	}
	d2, err := Decrypt(c2, key)
	if err != nil {
		t.Fatalf("Decrypt c2 failed: %v", err)
	}
	if d1 != plaintext || d2 != plaintext {
		t.Errorf("decrypted values mismatch: d1=%q, d2=%q, want=%q", d1, d2, plaintext)
	}
}
