// Package oauth implements OAuth flows for provider authentication.
package oauth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OpenAI OAuth constants (public Codex CLI credentials).
const (
	openAIDeviceAuthURL = "https://auth.openai.com/oauth/device/code"
	openAITokenURL      = "https://auth.openai.com/oauth/token"
	openAIClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAIScope         = "openid profile email offline_access"
	openAIAudience      = "https://api.openai.com/v1"
)

// DeviceCodeResponse is returned when initiating the device code flow.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// TokenResponse is returned after successful token exchange.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// PendingFlow tracks an in-progress device code flow.
type PendingFlow struct {
	DeviceCode string
	UserCode   string
	ExpiresAt  time.Time
	Interval   time.Duration
}

// OpenAIAuth manages OpenAI OAuth device code flows.
type OpenAIAuth struct {
	mu      sync.Mutex
	pending map[string]*PendingFlow // keyed by user_code
	client  *http.Client
}

// NewOpenAIAuth creates a new OpenAI OAuth handler.
func NewOpenAIAuth() *OpenAIAuth {
	return &OpenAIAuth{
		pending: make(map[string]*PendingFlow),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// StartDeviceFlow initiates the device code flow with OpenAI.
// Returns the user code and verification URI for the user to complete in browser.
func (a *OpenAIAuth) StartDeviceFlow() (*DeviceCodeResponse, error) {
	data := url.Values{
		"client_id": {openAIClientID},
		"scope":     {openAIScope},
		"audience":  {openAIAudience},
	}

	resp, err := a.client.PostForm(openAIDeviceAuthURL, data)
	if err != nil {
		return nil, fmt.Errorf("device code request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code request returned %d: %s", resp.StatusCode, string(body))
	}

	var dcResp DeviceCodeResponse
	if err := json.Unmarshal(body, &dcResp); err != nil {
		return nil, fmt.Errorf("parse device code response: %w", err)
	}

	if dcResp.Interval == 0 {
		dcResp.Interval = 5
	}

	// Store pending flow
	a.mu.Lock()
	a.pending[dcResp.UserCode] = &PendingFlow{
		DeviceCode: dcResp.DeviceCode,
		UserCode:   dcResp.UserCode,
		ExpiresAt:  time.Now().Add(time.Duration(dcResp.ExpiresIn) * time.Second),
		Interval:   time.Duration(dcResp.Interval) * time.Second,
	}
	a.mu.Unlock()

	return &dcResp, nil
}

// PollForToken polls OpenAI's token endpoint until the user completes
// authorization or the flow expires. Returns the token response.
func (a *OpenAIAuth) PollForToken(userCode string) (*TokenResponse, error) {
	a.mu.Lock()
	flow, ok := a.pending[userCode]
	a.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("no pending flow for user code %q", userCode)
	}

	for {
		if time.Now().After(flow.ExpiresAt) {
			a.mu.Lock()
			delete(a.pending, userCode)
			a.mu.Unlock()
			return nil, fmt.Errorf("device code expired")
		}

		time.Sleep(flow.Interval)

		token, err := a.exchangeDeviceCode(flow.DeviceCode)
		if err != nil {
			if strings.Contains(err.Error(), "authorization_pending") {
				continue
			}
			if strings.Contains(err.Error(), "slow_down") {
				flow.Interval += 1 * time.Second
				continue
			}
			a.mu.Lock()
			delete(a.pending, userCode)
			a.mu.Unlock()
			return nil, err
		}

		// Success — clean up pending flow
		a.mu.Lock()
		delete(a.pending, userCode)
		a.mu.Unlock()

		return token, nil
	}
}

func (a *OpenAIAuth) exchangeDeviceCode(deviceCode string) (*TokenResponse, error) {
	data := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {openAIClientID},
	}

	resp, err := a.client.PostForm(openAITokenURL, data)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Check for pending/slow_down errors
		var errResp struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &errResp)
		if errResp.Error != "" {
			return nil, fmt.Errorf("%s", errResp.Error)
		}
		return nil, fmt.Errorf("token request returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	return &tokenResp, nil
}
