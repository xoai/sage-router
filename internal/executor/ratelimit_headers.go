package executor

import (
	"net/http"
	"strconv"
	"time"
)

// RateLimitInfo is the parsed rate-limit signal from an upstream response.
// M2 (cycle 20260520-m2-circuit-breaker) populates Reset — the cooldown the
// circuit breaker honors — and Known. M3 (cycle 20260521-m3-quota-tracking)
// added the remaining-quota fields the QuotaWindow consumes; the extension is
// additive — ParseRateLimitReset's signature is unchanged.
type RateLimitInfo struct {
	Reset          time.Duration // (M2) time until the rate limit resets; 0 when unknown
	Known          bool          // (M2) true when a recognized reset header was present
	Remaining      int           // (M3) requests left in the window; -1 when not reported
	RemainingKnown bool          // (M3) true when a recognized *-remaining-* header was parsed
}

// ParseRateLimitReset parses an upstream response's rate-limit headers into a
// RateLimitInfo: the reset delay the circuit breaker honors (Reset/Known — M2)
// and the remaining-request count the QuotaWindow consumes (Remaining/
// RemainingKnown — M3). The two are parsed independently from separate
// headers; neither ever errors — an absent or unparseable header degrades to
// the unknown defaults. now is the reference for absolute-timestamp header
// values; production callers pass time.Now().
func ParseRateLimitReset(provider string, h http.Header, now time.Time) RateLimitInfo {
	info := parseResetHeaders(provider, h, now)
	info.Remaining, info.RemainingKnown = parseRemainingHeaders(provider, h)
	return info
}

// parseResetHeaders extracts the retry delay from an upstream response's
// headers. It checks, in priority order:
//
//  1. the generic Retry-After header (RFC 7231 — delta-seconds or HTTP-date),
//  2. provider-specific reset headers — Anthropic RFC 3339 timestamps,
//     OpenAI Go-style duration strings,
//  3. a generic x-ratelimit-reset epoch-seconds header.
//
// An unrecognized or absent header yields {Reset: 0, Known: false} — never an
// error; the breaker then falls back to its computed per-kind cooldown. A
// recognized header whose reset time has already passed yields
// {Reset: 0, Known: true}. It populates only Reset and Known — the caller
// (ParseRateLimitReset) populates the Remaining fields.
func parseResetHeaders(provider string, h http.Header, now time.Time) RateLimitInfo {
	// 1. Generic Retry-After — RFC 7231: delta-seconds or an HTTP-date.
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return clampReset(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(v); err == nil {
			return clampReset(t.Sub(now))
		}
		// Unparseable Retry-After — fall through to the other sources.
	}

	// 2. Provider-specific reset headers.
	switch provider {
	case "anthropic":
		if reset, ok := latestRFC3339Reset(h,
			"anthropic-ratelimit-requests-reset",
			"anthropic-ratelimit-tokens-reset",
			"anthropic-ratelimit-unified-reset"); ok {
			return clampReset(reset.Sub(now))
		}
	case "openai", "openrouter":
		if d, ok := longestDurationHeader(h,
			"x-ratelimit-reset-requests",
			"x-ratelimit-reset-tokens"); ok {
			return clampReset(d)
		}
	}

	// 3. Generic x-ratelimit-reset — epoch seconds.
	if v := h.Get("X-Ratelimit-Reset"); v != "" {
		if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
			return clampReset(time.Unix(epoch, 0).Sub(now))
		}
	}

	return RateLimitInfo{}
}

// clampReset wraps a computed reset duration: a non-positive duration (the
// reset already passed) becomes Reset 0, but the result is still Known — a
// header WAS recognized.
func clampReset(d time.Duration) RateLimitInfo {
	if d < 0 {
		d = 0
	}
	return RateLimitInfo{Reset: d, Known: true}
}

// latestRFC3339Reset parses the named headers as RFC 3339 timestamps and
// returns the latest. Without knowing which window is binding, the latest
// reset is the safe choice — the connection is not clear until every
// constraining window has reset. Unparseable values are skipped.
func latestRFC3339Reset(h http.Header, keys ...string) (time.Time, bool) {
	var latest time.Time
	var found bool
	for _, k := range keys {
		v := h.Get(k)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			continue
		}
		if !found || t.After(latest) {
			latest, found = t, true
		}
	}
	return latest, found
}

// longestDurationHeader parses the named headers as Go-style duration strings
// (OpenAI's format, e.g. "1m30s", "88ms") and returns the longest. Unparseable
// values are skipped.
func longestDurationHeader(h http.Header, keys ...string) (time.Duration, bool) {
	var longest time.Duration
	var found bool
	for _, k := range keys {
		v := h.Get(k)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			continue
		}
		if !found || d > longest {
			longest, found = d, true
		}
	}
	return longest, found
}

// remainingHeaders maps a provider to its "remaining requests" rate-limit
// header. A provider not listed — and any listed provider whose header is
// absent or unparseable — falls back to the generic X-RateLimit-Remaining.
var remainingHeaders = map[string]string{
	"anthropic":  "anthropic-ratelimit-requests-remaining",
	"openai":     "x-ratelimit-remaining-requests",
	"openrouter": "x-ratelimit-remaining-requests",
}

// parseRemainingHeaders extracts the remaining-request count from an upstream
// response — the provider-specific header first, then the generic
// X-RateLimit-Remaining fallback. Returns (-1, false) when no recognized
// header is present or its value does not parse to a non-negative integer;
// never an error. Zero is a real value (the window is exhausted) and is
// returned as (0, true).
func parseRemainingHeaders(provider string, h http.Header) (remaining int, known bool) {
	if name, ok := remainingHeaders[provider]; ok {
		if n, ok := parseNonNegInt(h.Get(name)); ok {
			return n, true
		}
	}
	if n, ok := parseNonNegInt(h.Get("X-RateLimit-Remaining")); ok {
		return n, true
	}
	return -1, false
}

// parseNonNegInt parses v as a non-negative integer. An empty, non-numeric, or
// negative value yields ok=false — a remaining-request count is never
// negative, so a negative value is treated as garbage.
func parseNonNegInt(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
