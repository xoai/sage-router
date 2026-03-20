package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a per-key sliding window rate limiter.
// It tracks request timestamps per key and allows checking whether a new
// request should be permitted based on a requests-per-minute (RPM) limit.
type Limiter struct {
	mu      sync.Mutex
	windows map[string][]time.Time
}

// New creates a new Limiter.
func New() *Limiter {
	return &Limiter{
		windows: make(map[string][]time.Time),
	}
}

// Allow checks whether a request from the given key is allowed under the
// specified RPM limit. If allowed, it records the request and returns true.
// If rpm <= 0, the limiter always allows (unlimited).
func (l *Limiter) Allow(key string, rpm int) bool {
	if rpm <= 0 {
		return true // unlimited
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-time.Minute)

	// Prune expired entries.
	window := l.windows[key]
	start := 0
	for start < len(window) && window[start].Before(cutoff) {
		start++
	}
	window = window[start:]

	if len(window) >= rpm {
		l.windows[key] = window
		return false
	}

	l.windows[key] = append(window, now)
	return true
}

// Cleanup removes entries for keys that have no recent activity.
// Call periodically to prevent memory growth.
func (l *Limiter) Cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-time.Minute)
	for key, window := range l.windows {
		start := 0
		for start < len(window) && window[start].Before(cutoff) {
			start++
		}
		if start >= len(window) {
			delete(l.windows, key)
		} else {
			l.windows[key] = window[start:]
		}
	}
}
