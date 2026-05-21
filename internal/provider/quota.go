package provider

import "time"

// QuotaWindow is a best-effort, in-memory view of a connection's upstream
// rate-limit quota (cycle 20260521-m3-quota-tracking, ADR-3). It is populated
// from provider rate-limit response headers and consumed by the reset-aware
// routing strategy.
//
// It is NOT a health facet — it does not affect Selectable; it only informs
// strategy ordering. In-memory only: a connection loads the zero value
// (Known=false) on restart, exactly like the breaker facet.
type QuotaWindow struct {
	Remaining int       // requests left in the window; -1 when the provider did not report it
	ResetAt   time.Time // when the window rolls over; zero when not reported
	Known     bool      // true once a recognized rate-limit header populated this window
}

// QuotaWindow returns a copy of the connection's current quota view. Safe for
// concurrent use; the QuotaWindow value type makes the returned copy
// independent of the connection.
func (c *Connection) QuotaWindow() QuotaWindow {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.quotaWindow
}

// SetQuotaWindow replaces the connection's quota view wholesale. It is a
// whole-value replacement, never a merge: each upstream response carries the
// provider's current view, so a later response that reports fewer headers must
// be able to downgrade the window (e.g. back to Known=false) rather than leave
// stale richer data in place. Safe for concurrent use.
func (c *Connection) SetQuotaWindow(qw QuotaWindow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quotaWindow = qw
}
