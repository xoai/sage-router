package imports

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"sage-router/internal/auth"
)

// geminiFile is the shape of ~/.gemini/oauth_creds.json. expiry_date is
// always unix milliseconds.
type geminiFile struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiryDate   int64  `json:"expiry_date,omitempty"` // unix milliseconds
}

func parseGeminiFile(path string) (*auth.Credential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gemini file: %w", err)
	}
	var f geminiFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse gemini file: %w", err)
	}
	if f.AccessToken == "" {
		return nil, ErrEmptyToken
	}

	cred := &auth.Credential{
		AccessToken:  f.AccessToken,
		RefreshToken: f.RefreshToken,
	}
	if f.ExpiryDate > 0 {
		cred.ExpiresAt = time.UnixMilli(f.ExpiryDate)
	}
	if f.Scope != "" {
		cred.ExtraData = map[string]any{"scope": f.Scope}
	}
	return cred, nil
}
