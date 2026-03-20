package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

// DeriveKey produces a 32-byte AES-256 key from a password using SHA-256.
// For production workloads consider replacing this with Argon2id or scrypt.
func DeriveKey(password string) []byte {
	h := sha256.Sum256([]byte(password))
	return h[:]
}

// Encrypt encrypts plaintext with AES-256-GCM and returns a base64-encoded ciphertext
// that includes the random nonce prefix.
func Encrypt(plaintext string, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	// Seal appends the ciphertext to the nonce so we can extract it on decrypt.
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt decodes a base64 ciphertext produced by Encrypt and returns the plaintext.
func Decrypt(ciphertext string, key []byte) (string, error) {
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, raw := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, raw, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
