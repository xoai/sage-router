package provider

import (
	"fmt"
	"strings"
)

// ComboEntry represents a single provider+model pair extracted from a combo string.
type ComboEntry struct {
	Provider string
	Model    string
}

// String returns the canonical "provider/model" form.
func (e ComboEntry) String() string {
	return e.Provider + "/" + e.Model
}

// ParseCombo parses a list of "provider/model" strings into ComboEntry values.
// Each element must contain exactly one slash separating provider from model.
// Returns an error if any entry is malformed.
func ParseCombo(models []string) ([]ComboEntry, error) {
	entries := make([]ComboEntry, 0, len(models))
	for _, raw := range models {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		idx := strings.Index(raw, "/")
		if idx <= 0 || idx == len(raw)-1 {
			return nil, fmt.Errorf("invalid combo entry %q: expected \"provider/model\"", raw)
		}
		entries = append(entries, ComboEntry{
			Provider: raw[:idx],
			Model:    raw[idx+1:],
		})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("empty combo list")
	}
	return entries, nil
}

// NextFallback returns the first ComboEntry whose String() representation does
// not appear in the tried set. Returns nil if all entries have been tried.
func NextFallback(entries []ComboEntry, tried []string) *ComboEntry {
	triedSet := make(map[string]bool, len(tried))
	for _, t := range tried {
		triedSet[t] = true
	}
	for i := range entries {
		if !triedSet[entries[i].String()] {
			return &entries[i]
		}
	}
	return nil
}
