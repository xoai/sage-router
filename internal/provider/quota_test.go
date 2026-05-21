package provider

import (
	"testing"
	"time"
)

// M3 T1 — QuotaWindow: the best-effort, in-memory per-connection rate-limit
// quota view (cycle 20260521-m3-quota-tracking, ADR-3).

func TestConnection_QuotaWindowDefaultsToZeroValue(t *testing.T) {
	c := NewConnection("c1", "openai", "test", 0, "apikey")
	qw := c.QuotaWindow()
	if qw.Known {
		t.Errorf("fresh connection: QuotaWindow.Known = true, want false")
	}
	if !qw.ResetAt.IsZero() {
		t.Errorf("fresh connection: QuotaWindow.ResetAt = %v, want zero", qw.ResetAt)
	}
}

// SetQuotaWindow is a whole-value replacement, never a merge — a later,
// header-poorer response must fully overwrite a richer earlier window so a
// connection that drops below the header-reporting tier is correctly
// downgraded rather than left stale.
func TestConnection_SetQuotaWindowWholeReplace(t *testing.T) {
	c := NewConnection("c1", "openai", "test", 0, "apikey")

	rich := QuotaWindow{Remaining: 500, ResetAt: time.Now().Add(time.Hour), Known: true}
	c.SetQuotaWindow(rich)
	if got := c.QuotaWindow(); got != rich {
		t.Fatalf("after SetQuotaWindow(rich): got %+v, want %+v", got, rich)
	}

	poor := QuotaWindow{Remaining: -1, ResetAt: time.Time{}, Known: false}
	c.SetQuotaWindow(poor)
	got := c.QuotaWindow()
	if got != poor {
		t.Errorf("after SetQuotaWindow(poor): got %+v, want %+v (whole replacement, no merge)", got, poor)
	}
	if got.Remaining == 500 || !got.ResetAt.IsZero() || got.Known {
		t.Errorf("a stale field survived the whole-value replacement: %+v", got)
	}
}
