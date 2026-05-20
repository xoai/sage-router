package provider

import (
	"testing"
	"time"
)

func TestCalculateCooldownTableDriven(t *testing.T) {
	tests := []struct {
		name  string
		level int
		want  time.Duration
	}{
		{
			name:  "level 0 => 1s",
			level: 0,
			want:  1 * time.Second,
		},
		{
			name:  "level 1 => 2s",
			level: 1,
			want:  2 * time.Second,
		},
		{
			name:  "level 2 => 4s",
			level: 2,
			want:  4 * time.Second,
		},
		{
			name:  "level 3 => 8s",
			level: 3,
			want:  8 * time.Second,
		},
		{
			name:  "level 6 => 64s",
			level: 6,
			want:  64 * time.Second,
		},
		{
			name:  "level 7 => 128s capped to MaxCooldown (120s)",
			level: 7,
			want:  MaxCooldown,
		},
		{
			name:  "level 10 => capped at MaxCooldown",
			level: 10,
			want:  MaxCooldown,
		},
		{
			name:  "level 15 (MaxBackoffLevel) => capped at MaxCooldown",
			level: 15,
			want:  MaxCooldown,
		},
		{
			name:  "negative level => clamped to 0 => 1s",
			level: -1,
			want:  1 * time.Second,
		},
		{
			name:  "very negative level => clamped to 0 => 1s",
			level: -100,
			want:  1 * time.Second,
		},
		{
			name:  "level > MaxBackoffLevel => clamped and capped",
			level: 20,
			want:  MaxCooldown,
		},
		{
			name:  "level 100 => clamped and capped",
			level: 100,
			want:  MaxCooldown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCooldown(tt.level)
			if got != tt.want {
				t.Errorf("CalculateCooldown(%d) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestCalculateCooldownNeverExceedsMax(t *testing.T) {
	for level := -5; level <= 30; level++ {
		d := CalculateCooldown(level)
		if d > MaxCooldown {
			t.Errorf("CalculateCooldown(%d) = %v exceeds MaxCooldown %v", level, d, MaxCooldown)
		}
		if d < BaseCooldown {
			t.Errorf("CalculateCooldown(%d) = %v is less than BaseCooldown %v", level, d, BaseCooldown)
		}
	}
}

func TestCooldownConstants(t *testing.T) {
	if BaseCooldown != 1*time.Second {
		t.Errorf("BaseCooldown = %v, want 1s", BaseCooldown)
	}
	if MaxCooldown != 2*time.Minute {
		t.Errorf("MaxCooldown = %v, want 2m", MaxCooldown)
	}
	if MaxBackoffLevel != 15 {
		t.Errorf("MaxBackoffLevel = %d, want 15", MaxBackoffLevel)
	}
}

// TestCooldownDuration verifies exponential backoff: Level 0 -> 1s, Level 1 -> 2s, Level 2 -> 4s.
func TestCooldownDuration(t *testing.T) {
	tests := []struct {
		level int
		want  time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 64 * time.Second},
	}

	for _, tt := range tests {
		got := CalculateCooldown(tt.level)
		if got != tt.want {
			t.Errorf("CalculateCooldown(%d) = %v, want %v", tt.level, got, tt.want)
		}
	}
}

// TestCooldownMaxClamp verifies that high backoff levels are clamped at MaxCooldown.
func TestCooldownMaxClamp(t *testing.T) {
	highLevels := []int{7, 8, 10, 15, 20, 50, 100}
	for _, level := range highLevels {
		got := CalculateCooldown(level)
		if got != MaxCooldown {
			t.Errorf("CalculateCooldown(%d) = %v, want MaxCooldown (%v)", level, got, MaxCooldown)
		}
	}
}

// TestCooldownNegativeLevel verifies that negative levels are clamped to 0 (BaseCooldown).
func TestCooldownNegativeLevel(t *testing.T) {
	negativeLevels := []int{-1, -5, -100, -999}
	for _, level := range negativeLevels {
		got := CalculateCooldown(level)
		if got != BaseCooldown {
			t.Errorf("CalculateCooldown(%d) = %v, want BaseCooldown (%v)", level, got, BaseCooldown)
		}
	}
}

// TestCooldownFor pins the per-failure-kind cooldown (M2, spec §3.1). A
// rate-limit or quota failure honors an explicit upstream Retry-After hint;
// transient and errored failures always use the computed exponential.
func TestCooldownFor(t *testing.T) {
	tests := []struct {
		name       string
		kind       FailureKind
		level      int
		retryAfter time.Duration
		want       time.Duration
	}{
		// rate_limit — Retry-After honored verbatim when present.
		{"rate_limit honors Retry-After", FailureRateLimit, 0, 90 * time.Second, 90 * time.Second},
		{"rate_limit honors Retry-After over backoff", FailureRateLimit, 5, 30 * time.Second, 30 * time.Second},
		// rate_limit — no header: 60s base, escalating, 15m ceiling.
		{"rate_limit level 0 => 60s base", FailureRateLimit, 0, 0, 60 * time.Second},
		{"rate_limit level 2 => 4m", FailureRateLimit, 2, 0, 4 * time.Minute},
		{"rate_limit high level => clamped to 15m", FailureRateLimit, 20, 0, 15 * time.Minute},
		// quota — Retry-After honored; else a flat multi-hour fallback (no escalation).
		{"quota honors Retry-After", FailureQuota, 3, 30 * time.Minute, 30 * time.Minute},
		{"quota no header => 1h flat", FailureQuota, 0, 0, 1 * time.Hour},
		{"quota ignores backoff level", FailureQuota, 9, 0, 1 * time.Hour},
		// transient — the existing CalculateCooldown; Retry-After NOT honored (spec §3.1).
		{"transient level 3 => 8s", FailureTransient, 3, 0, 8 * time.Second},
		{"transient ignores Retry-After", FailureTransient, 0, 90 * time.Second, 1 * time.Second},
		{"transient high level => capped at MaxCooldown", FailureTransient, 10, 0, MaxCooldown},
		// errored — 5s base (slower than a 5xx blip), escalating, 2m ceiling.
		{"errored level 0 => 5s base", FailureErrored, 0, 0, 5 * time.Second},
		{"errored level 2 => 20s", FailureErrored, 2, 0, 20 * time.Second},
		{"errored high level => capped at MaxCooldown", FailureErrored, 15, 0, MaxCooldown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CooldownFor(tt.kind, tt.level, tt.retryAfter)
			if got != tt.want {
				t.Errorf("CooldownFor(%s, %d, %v) = %v, want %v",
					tt.kind, tt.level, tt.retryAfter, got, tt.want)
			}
		})
	}
}
