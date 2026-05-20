package executor

import (
	"net/http"
	"strconv"
	"time"
)

// RateLimitInfo is the parsed rate-limit signal from an upstream response.
// M2 (cycle 20260520-m2-circuit-breaker) populates Reset — the cooldown the
// circuit breaker honors — and Known. M3's quota tracker extends this struct
// with the remaining-quota fields for the QuotaWindow, keeping
// ParseRateLimitReset's contract additive (no signature churn for M3).
type RateLimitInfo struct {
	Reset time.Duration // time until the rate limit resets; 0 when unknown
	Known bool          // true when a recognized rate-limit header was present
}

// ParseRateLimitReset extracts the retry delay from an upstream response's
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
// {Reset: 0, Known: true}. now is the reference for absolute-timestamp header
// values; production callers pass time.Now().
func ParseRateLimitReset(provider string, h http.Header, now time.Time) RateLimitInfo {
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
