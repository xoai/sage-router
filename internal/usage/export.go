package usage

import (
	"sage-router/internal/store"
	"time"
)

// DailySummary returns usage summary for a specific day.
func DailySummary(s store.Store, date time.Time) (*store.UsageSummary, error) {
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, date.Location())
	end := start.Add(24 * time.Hour)
	return s.UsageSummary(store.UsageFilter{
		From: start,
		To:   end,
	})
}

// MonthlySummary returns usage summary for the current month.
func MonthlySummary(s store.Store) (*store.UsageSummary, error) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return s.UsageSummary(store.UsageFilter{
		From: start,
		To:   now,
	})
}
