package executor

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestParseRateLimitReset pins the shared rate-limit header parser (M2 spec
// §5). Priority order: generic Retry-After → provider-specific reset headers
// → generic x-ratelimit-reset epoch. An unrecognized/absent header yields
// {0, false}; a recognized-but-already-passed reset yields {0, true}.
func TestParseRateLimitReset(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		provider  string
		headers   map[string]string
		wantReset time.Duration
		wantKnown bool
	}{
		{
			name:      "generic Retry-After delta-seconds",
			provider:  "gemini",
			headers:   map[string]string{"Retry-After": "120"},
			wantReset: 120 * time.Second,
			wantKnown: true,
		},
		{
			name:      "generic Retry-After HTTP-date",
			provider:  "gemini",
			headers:   map[string]string{"Retry-After": now.Add(2 * time.Minute).UTC().Format(http.TimeFormat)},
			wantReset: 2 * time.Minute,
			wantKnown: true,
		},
		{
			name:      "anthropic RFC3339 reset",
			provider:  "anthropic",
			headers:   map[string]string{"anthropic-ratelimit-requests-reset": now.Add(90 * time.Second).Format(time.RFC3339)},
			wantReset: 90 * time.Second,
			wantKnown: true,
		},
		{
			name:     "anthropic takes the latest of requests/tokens reset",
			provider: "anthropic",
			headers: map[string]string{
				"anthropic-ratelimit-requests-reset": now.Add(30 * time.Second).Format(time.RFC3339),
				"anthropic-ratelimit-tokens-reset":   now.Add(75 * time.Second).Format(time.RFC3339),
			},
			wantReset: 75 * time.Second,
			wantKnown: true,
		},
		{
			name:      "openai duration-string reset",
			provider:  "openai",
			headers:   map[string]string{"x-ratelimit-reset-requests": "1m30s"},
			wantReset: 90 * time.Second,
			wantKnown: true,
		},
		{
			name:      "generic x-ratelimit-reset epoch",
			provider:  "gemini",
			headers:   map[string]string{"x-ratelimit-reset": strconv.FormatInt(now.Add(45*time.Second).Unix(), 10)},
			wantReset: 45 * time.Second,
			wantKnown: true,
		},
		{
			name:     "Retry-After wins over x-ratelimit-reset",
			provider: "openai",
			headers: map[string]string{
				"Retry-After":       "10",
				"x-ratelimit-reset": strconv.FormatInt(now.Add(999*time.Second).Unix(), 10),
			},
			wantReset: 10 * time.Second,
			wantKnown: true,
		},
		{
			name:      "recognized header, reset already passed => 0 but Known",
			provider:  "anthropic",
			headers:   map[string]string{"anthropic-ratelimit-requests-reset": now.Add(-60 * time.Second).Format(time.RFC3339)},
			wantReset: 0,
			wantKnown: true,
		},
		{
			name:      "unrecognized header => not known",
			provider:  "openai",
			headers:   map[string]string{"X-Some-Other-Header": "whatever"},
			wantReset: 0,
			wantKnown: false,
		},
		{
			name:      "no headers => not known",
			provider:  "anthropic",
			headers:   map[string]string{},
			wantReset: 0,
			wantKnown: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			got := ParseRateLimitReset(tt.provider, h, now)
			if got.Reset != tt.wantReset {
				t.Errorf("Reset = %v, want %v", got.Reset, tt.wantReset)
			}
			if got.Known != tt.wantKnown {
				t.Errorf("Known = %v, want %v", got.Known, tt.wantKnown)
			}
		})
	}
}

// TestParseRateLimitReset_Remaining pins M3's remaining-quota extension to the
// shared parser (spec §2): the *-remaining-* headers for generic / Anthropic /
// OpenAI shapes, with a generic fallback. A missing or unparseable header
// yields {Remaining: -1, RemainingKnown: false}, never an error. Zero is a
// real value (window exhausted). The reset fields are exercised by
// TestParseRateLimitReset above; this test asserts only the M3 fields.
func TestParseRateLimitReset_Remaining(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		provider      string
		headers       map[string]string
		wantRemaining int
		wantKnown     bool
	}{
		{
			name:          "generic X-RateLimit-Remaining",
			provider:      "gemini",
			headers:       map[string]string{"X-RateLimit-Remaining": "42"},
			wantRemaining: 42,
			wantKnown:     true,
		},
		{
			name:          "anthropic remaining header",
			provider:      "anthropic",
			headers:       map[string]string{"anthropic-ratelimit-requests-remaining": "1000"},
			wantRemaining: 1000,
			wantKnown:     true,
		},
		{
			name:          "openai remaining header",
			provider:      "openai",
			headers:       map[string]string{"x-ratelimit-remaining-requests": "7"},
			wantRemaining: 7,
			wantKnown:     true,
		},
		{
			name:          "openrouter shares the openai remaining header",
			provider:      "openrouter",
			headers:       map[string]string{"x-ratelimit-remaining-requests": "500"},
			wantRemaining: 500,
			wantKnown:     true,
		},
		{
			name:          "zero remaining is a real value (window exhausted)",
			provider:      "openai",
			headers:       map[string]string{"x-ratelimit-remaining-requests": "0"},
			wantRemaining: 0,
			wantKnown:     true,
		},
		{
			name:          "provider-specific absent, generic header is the fallback",
			provider:      "anthropic",
			headers:       map[string]string{"X-RateLimit-Remaining": "13"},
			wantRemaining: 13,
			wantKnown:     true,
		},
		{
			name:          "no remaining header => -1, not known",
			provider:      "openai",
			headers:       map[string]string{"x-ratelimit-reset-requests": "1m"},
			wantRemaining: -1,
			wantKnown:     false,
		},
		{
			name:          "garbage value => -1, not known",
			provider:      "openai",
			headers:       map[string]string{"x-ratelimit-remaining-requests": "lots"},
			wantRemaining: -1,
			wantKnown:     false,
		},
		{
			name:          "negative value rejected as garbage",
			provider:      "gemini",
			headers:       map[string]string{"X-RateLimit-Remaining": "-5"},
			wantRemaining: -1,
			wantKnown:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			got := ParseRateLimitReset(tt.provider, h, now)
			if got.Remaining != tt.wantRemaining {
				t.Errorf("Remaining = %d, want %d", got.Remaining, tt.wantRemaining)
			}
			if got.RemainingKnown != tt.wantKnown {
				t.Errorf("RemainingKnown = %v, want %v", got.RemainingKnown, tt.wantKnown)
			}
		})
	}
}
