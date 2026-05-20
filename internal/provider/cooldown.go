package provider

import "time"

const (
	// BaseCooldown is the minimum cooldown duration before retrying a connection.
	BaseCooldown = 1 * time.Second

	// MaxCooldown caps the exponential backoff.
	MaxCooldown = 2 * time.Minute

	// MaxBackoffLevel limits the exponent to prevent overflow.
	MaxBackoffLevel = 15
)

// CalculateCooldown returns the cooldown duration for the given backoff level.
// Formula: BaseCooldown * 2^level, capped at MaxCooldown.
// Level is clamped to [0, MaxBackoffLevel].
func CalculateCooldown(level int) time.Duration {
	if level < 0 {
		level = 0
	}
	if level > MaxBackoffLevel {
		level = MaxBackoffLevel
	}
	d := BaseCooldown << level // BaseCooldown * 2^level
	if d > MaxCooldown {
		d = MaxCooldown
	}
	return d
}

// Per-failure-kind cooldown tuning — M2 (cycle 20260520-m2-circuit-breaker,
// open question #1). A 429 clears on roughly a one-minute schedule, so its
// base is 60s and it escalates up to a 15-minute ceiling. A non-transient
// upstream error ("errored") is slower to clear than a 5xx blip, so its base
// is 5s (vs transient's 1s), under the shared 2-minute MaxCooldown ceiling.
// quota_exhausted has no useful exponential — it clears when the provider's
// quota window resets, hours away — so its fallback is a flat multi-hour
// value until M3's quota tracker supplies real reset times.
const (
	rateLimitBaseCooldown = 60 * time.Second
	rateLimitMaxCooldown  = 15 * time.Minute
	erroredBaseCooldown   = 5 * time.Second
	quotaFallbackCooldown = 1 * time.Hour
)

// CooldownFor returns the breaker cooldown for a failure of the given kind.
// When the upstream provided an explicit retry hint (retryAfter > 0), a
// rate-limit or quota failure honors it verbatim — the provider knows its own
// schedule better than any computed backoff. Transient and errored failures
// have no such authoritative signal, so they always use the computed
// exponential (spec §3.1).
func CooldownFor(kind FailureKind, backoffLevel int, retryAfter time.Duration) time.Duration {
	switch kind {
	case FailureRateLimit:
		if retryAfter > 0 {
			return retryAfter
		}
		return scaledCooldown(rateLimitBaseCooldown, backoffLevel, rateLimitMaxCooldown)
	case FailureQuota:
		if retryAfter > 0 {
			return retryAfter
		}
		return quotaFallbackCooldown
	case FailureErrored:
		return scaledCooldown(erroredBaseCooldown, backoffLevel, MaxCooldown)
	default: // FailureTransient and any unknown kind
		return CalculateCooldown(backoffLevel)
	}
}

// scaledCooldown returns base * 2^level, clamped to (0, max]. level is clamped
// to [0, MaxBackoffLevel] to bound the shift; the d <= 0 guard catches any
// shift overflow.
func scaledCooldown(base time.Duration, level int, max time.Duration) time.Duration {
	if level < 0 {
		level = 0
	}
	if level > MaxBackoffLevel {
		level = MaxBackoffLevel
	}
	d := base << level
	if d > max || d <= 0 {
		d = max
	}
	return d
}
