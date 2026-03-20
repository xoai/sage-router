package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Manager handles all authentication concerns.
type Manager struct {
	mu           sync.RWMutex
	passwordHash string
	jwtSecret    []byte
	hmacSecret   []byte

	// One-time setup token (in-memory only)
	setupToken   string
	setupTokenAt time.Time
}

const setupTokenTTL = 5 * time.Minute

// NewManager creates a new auth manager.
func NewManager(passwordHash string, jwtSecret, hmacSecret []byte) *Manager {
	return &Manager{
		passwordHash: passwordHash,
		jwtSecret:    jwtSecret,
		hmacSecret:   hmacSecret,
	}
}

// NeedsSetup returns true if no password has been set yet.
func (m *Manager) NeedsSetup() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.passwordHash == ""
}

// SetPasswordHash updates the stored password hash (used after first-run setup).
func (m *Manager) SetPasswordHash(hash string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.passwordHash = hash
}

// ── One-Time Setup Token ──

// GenerateSetupToken creates a one-time token for first-run authentication.
func (m *Manager) GenerateSetupToken() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setupToken = GenerateRandomPassword(32)
	m.setupTokenAt = time.Now()
	return m.setupToken
}

// ValidateSetupToken checks and consumes the one-time setup token.
func (m *Manager) ValidateSetupToken(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setupToken == "" || token == "" {
		return false
	}
	if time.Since(m.setupTokenAt) > setupTokenTTL {
		m.setupToken = ""
		return false
	}
	if !hmac.Equal([]byte(m.setupToken), []byte(token)) {
		return false
	}
	// Single use — clear after validation
	m.setupToken = ""
	return true
}

// ── Password Auth ──

// HashPassword hashes a password using bcrypt.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword verifies a password against the stored hash.
func (m *Manager) CheckPassword(password string) bool {
	m.mu.RLock()
	hash := m.passwordHash
	m.mu.RUnlock()
	if hash == "" {
		return false
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// GenerateRandomPassword creates a cryptographically random alphanumeric password.
func GenerateRandomPassword(length int) string {
	const charset = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, length)
	rand.Read(b)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// ── JWT Auth (manual HMAC-SHA256, no external library) ──

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtPayload struct {
	Exp   int64 `json:"exp"`
	Iat   int64 `json:"iat"`
	Setup bool  `json:"setup,omitempty"`
}

// GenerateToken creates a signed JWT token.
func (m *Manager) GenerateToken(expiry time.Duration) (string, error) {
	return m.generateTokenWithClaims(expiry, false)
}

// GenerateSetupJWT creates a JWT with the setup claim set to true.
func (m *Manager) GenerateSetupJWT(expiry time.Duration) (string, error) {
	return m.generateTokenWithClaims(expiry, true)
}

func (m *Manager) generateTokenWithClaims(expiry time.Duration, setup bool) (string, error) {
	header := jwtHeader{Alg: "HS256", Typ: "JWT"}
	payload := jwtPayload{
		Exp:   time.Now().Add(expiry).Unix(),
		Iat:   time.Now().Unix(),
		Setup: setup,
	}

	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(payload)

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	signingInput := headerB64 + "." + payloadB64
	mac := hmac.New(sha256.New, m.jwtSecret)
	mac.Write([]byte(signingInput))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return signingInput + "." + signature, nil
}

// TokenInfo holds the result of a token validation.
type TokenInfo struct {
	Valid bool
	Setup bool // true if this is a setup-only session
}

// ValidateToken verifies a JWT token's signature and expiry.
func (m *Manager) ValidateToken(tokenStr string) (bool, error) {
	info, err := m.ValidateTokenInfo(tokenStr)
	return info.Valid, err
}

// ValidateTokenInfo verifies a JWT token and returns extended info.
func (m *Manager) ValidateTokenInfo(tokenStr string) (TokenInfo, error) {
	parts := strings.SplitN(tokenStr, ".", 3)
	if len(parts) != 3 {
		return TokenInfo{}, fmt.Errorf("invalid token format")
	}

	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, m.jwtSecret)
	mac.Write([]byte(signingInput))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return TokenInfo{}, fmt.Errorf("invalid signature")
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenInfo{}, fmt.Errorf("invalid payload encoding")
	}

	var payload jwtPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return TokenInfo{}, fmt.Errorf("invalid payload")
	}

	if time.Now().Unix() > payload.Exp {
		return TokenInfo{}, fmt.Errorf("token expired")
	}

	return TokenInfo{Valid: true, Setup: payload.Setup}, nil
}

// SetAuthCookie sets the JWT auth cookie.
func (m *Manager) SetAuthCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "sage-auth",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400, // 24 hours
	})
}

// ClearAuthCookie clears the auth cookie.
func (m *Manager) ClearAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "sage-auth",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// ── API Key Auth ──

// GenerateAPIKey creates a new API key with HMAC-SHA256 hash.
// Returns: plainKey, keyHash, prefix
func (m *Manager) GenerateAPIKey() (string, string, string, error) {
	// Generate 16 random bytes
	randBytes := make([]byte, 16)
	if _, err := rand.Read(randBytes); err != nil {
		return "", "", "", err
	}

	randomHex := hex.EncodeToString(randBytes)
	// Calculate CRC8
	crc := crc8([]byte(randomHex))
	plainKey := fmt.Sprintf("sk-sage-%s-%02x", randomHex, crc)

	// Hash the key
	keyHash := m.HashAPIKey(plainKey)
	prefix := plainKey[:12]

	return plainKey, keyHash, prefix, nil
}

// HashAPIKey creates an HMAC-SHA256 hash of an API key.
func (m *Manager) HashAPIKey(key string) string {
	mac := hmac.New(sha256.New, m.hmacSecret)
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidateAPIKeyFormat checks if a key matches the expected format.
func ValidateAPIKeyFormat(key string) bool {
	return strings.HasPrefix(key, "sk-sage-") && len(key) > 12
}

// crc8 computes a simple CRC-8 checksum.
func crc8(data []byte) byte {
	var crc byte
	for _, b := range data {
		crc ^= b
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = (crc << 1) ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
