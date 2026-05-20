package provider

import (
	"log/slog"
	"sync"
	"time"
)

// HealthChecker runs a background loop that monitors connection states and
// performs passive recovery: expired cooldowns → Idle, expired model locks
// cleaned up, errored connections retried after a grace period.
type HealthChecker struct {
	selector *Selector
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewHealthChecker creates a checker that ticks at the given interval.
func NewHealthChecker(sel *Selector, interval time.Duration) *HealthChecker {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &HealthChecker{
		selector: sel,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the background health check loop.
func (h *HealthChecker) Start() {
	h.wg.Add(1)
	go h.loop()
	slog.Info("health checker started", "interval", h.interval)
}

// Stop signals the loop to exit and waits for it to finish.
func (h *HealthChecker) Stop() {
	close(h.stopCh)
	h.wg.Wait()
}

func (h *HealthChecker) loop() {
	defer h.wg.Done()
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.check()
		}
	}
}

func (h *HealthChecker) check() {
	h.selector.mu.RLock()
	var allConns []*Connection
	for _, conns := range h.selector.conns {
		allConns = append(allConns, conns...)
	}
	h.selector.mu.RUnlock()

	now := time.Now()
	for _, c := range allConns {
		h.checkConnection(c, now)
	}
}

// checkConnection performs the health checker's per-connection work: the
// timer-driven OPEN→HALF_OPEN breaker recovery, plus model-lock GC. Once an
// OPEN breaker's cooldown has elapsed the connection is promoted to HALF_OPEN
// so the Selector can run a single trial request (M2, ADR-1 / spec §6). The
// checker acts only on OPEN breakers — a CLOSED or HALF_OPEN connection is
// left untouched, so a slow in-flight trial is not disturbed. A failed
// credential refresh is no longer auto-recovered here: it stays
// Auth=AuthExpired and is re-driven by the refresh loop.
func (h *HealthChecker) checkConnection(c *Connection, now time.Time) {
	c.mu.Lock()
	breaker := c.breaker
	cooldownElapsed := !c.cooldownUntil.IsZero() && now.After(c.cooldownUntil)
	// Garbage-collect expired model-scoped locks, regardless of breaker state.
	for model, expiry := range c.modelLocks {
		if now.After(expiry) {
			delete(c.modelLocks, model)
		}
	}
	c.mu.Unlock()

	// Timer-driven OPEN→HALF_OPEN. ToHalfOpen acquires c.mu itself, so it is
	// called only after the lock above is released (the lock-discipline rule).
	// ToHalfOpen's own transition check makes a concurrent breaker change
	// between the read and the call harmless.
	if breaker == BreakerOpen && cooldownElapsed {
		if err := c.ToHalfOpen(); err == nil {
			slog.Info("health: breaker cooldown elapsed, connection half-open",
				"connection", c.ID, "provider", c.Provider)
		}
	}
}
