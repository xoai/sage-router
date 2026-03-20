package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter_AllowUnlimited(t *testing.T) {
	l := New()
	for i := 0; i < 1000; i++ {
		if !l.Allow("key1", 0) {
			t.Fatal("unlimited key should always be allowed")
		}
	}
}

func TestLimiter_EnforcesLimit(t *testing.T) {
	l := New()
	rpm := 5

	// First 5 should be allowed.
	for i := 0; i < rpm; i++ {
		if !l.Allow("key1", rpm) {
			t.Fatalf("request %d should be allowed", i)
		}
	}

	// 6th should be denied.
	if l.Allow("key1", rpm) {
		t.Fatal("request beyond RPM limit should be denied")
	}
}

func TestLimiter_SeparateKeys(t *testing.T) {
	l := New()

	// Fill key1's limit.
	for i := 0; i < 3; i++ {
		l.Allow("key1", 3)
	}
	if l.Allow("key1", 3) {
		t.Fatal("key1 should be rate limited")
	}

	// key2 should still be allowed.
	if !l.Allow("key2", 3) {
		t.Fatal("key2 should be allowed (separate window)")
	}
}

func TestLimiter_WindowExpiry(t *testing.T) {
	l := New()

	// Manually insert timestamps from > 1 minute ago.
	l.mu.Lock()
	past := time.Now().Add(-2 * time.Minute)
	l.windows["key1"] = []time.Time{past, past, past}
	l.mu.Unlock()

	// Should be allowed because old timestamps are pruned.
	if !l.Allow("key1", 3) {
		t.Fatal("should be allowed after window expiry")
	}
}

func TestLimiter_Cleanup(t *testing.T) {
	l := New()

	// Add old entries.
	l.mu.Lock()
	past := time.Now().Add(-2 * time.Minute)
	l.windows["stale"] = []time.Time{past}
	l.windows["active"] = []time.Time{time.Now()}
	l.mu.Unlock()

	l.Cleanup()

	l.mu.Lock()
	_, staleExists := l.windows["stale"]
	_, activeExists := l.windows["active"]
	l.mu.Unlock()

	if staleExists {
		t.Fatal("stale key should be cleaned up")
	}
	if !activeExists {
		t.Fatal("active key should remain")
	}
}
