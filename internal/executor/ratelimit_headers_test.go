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
