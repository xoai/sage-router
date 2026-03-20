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
