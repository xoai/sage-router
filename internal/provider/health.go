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

func (h *HealthChecker) checkConnection(c *Connection, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.state {
	case StateCooldown:
		// If cooldown has expired, transition back to Idle.
		if !c.cooldownUntil.IsZero() && now.After(c.cooldownUntil) {
			if err := c.transition(StateIdle); err == nil {
				c.cooldownUntil = time.Time{}
				slog.Info("health: cooldown expired, connection restored",
					"connection", c.ID, "provider", c.Provider)
			}
		}

	case StateErrored:
		// Auto-recover errored connections after a grace period (5 minutes).
		grace := 5 * time.Minute
		if !c.lastUsedAt.IsZero() && now.Sub(c.lastUsedAt) > grace {
			if err := c.transition(StateIdle); err == nil {
				c.backoffLevel = 0
				c.lastError = nil
				slog.Info("health: errored connection auto-recovered",
					"connection", c.ID, "provider", c.Provider)
			}
		}
	}

	// Garbage-collect expired model locks regardless of state.
	for model, expiry := range c.modelLocks {
		if now.After(expiry) {
			delete(c.modelLocks, model)
		}
	}
}
