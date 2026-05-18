package store

import (
	"strings"
	"testing"
	"time"
)

// TestBuildUsageFilter_MultiKey_IN pins the multi-key path: when
// APIKeyIDs is non-empty, the WHERE clause must emit an IN (?,...) form
// with one placeholder per id, and the params slice must carry the ids
// in order. Cycle 20260517-usage-page-filters T1.
func TestBuildUsageFilter_MultiKey_IN(t *testing.T) {
	f := UsageFilter{APIKeyIDs: []string{"a", "b", "c"}}
	w := buildUsageFilter(f)
	sql := w.sql()
	if !strings.Contains(sql, "api_key_id IN (?,?,?)") {
		t.Fatalf("want IN-clause for 3 keys; got: %q", sql)
	}
	if len(w.params) != 3 || w.params[0] != "a" || w.params[1] != "b" || w.params[2] != "c" {
		t.Fatalf("want params [a b c]; got: %v", w.params)
	}
}

// TestBuildUsageFilter_SingleKey_Backcompat pins the back-compat path:
// when APIKeyID is set and APIKeyIDs is nil, the single-key equality
// branch fires unchanged. Existing callers must not break.
func TestBuildUsageFilter_SingleKey_Backcompat(t *testing.T) {
	f := UsageFilter{APIKeyID: "x"}
	w := buildUsageFilter(f)
	sql := w.sql()
	if !strings.Contains(sql, "api_key_id = ?") {
		t.Fatalf("want equality clause; got: %q", sql)
	}
	if strings.Contains(sql, " IN (") {
		t.Fatalf("did not expect IN clause; got: %q", sql)
	}
	if len(w.params) != 1 || w.params[0] != "x" {
		t.Fatalf("want params [x]; got: %v", w.params)
	}
}

// TestBuildUsageFilter_MultiWinsOverSingle pins the collision rule from
// spec.md: when both are set, multi takes precedence. Single is ignored.
func TestBuildUsageFilter_MultiWinsOverSingle(t *testing.T) {
	f := UsageFilter{APIKeyID: "single", APIKeyIDs: []string{"multi-a", "multi-b"}}
	w := buildUsageFilter(f)
	sql := w.sql()
	if !strings.Contains(sql, "api_key_id IN (?,?)") {
		t.Fatalf("want IN-clause when multi non-empty; got: %q", sql)
	}
	for _, p := range w.params {
		if p == "single" {
			t.Fatalf("single-key value leaked into params: %v", w.params)
		}
	}
}

// TestBuildUsageFilter_EmptyBoth_NoKeyClause pins the no-filter case:
// neither APIKeyID nor APIKeyIDs set → no api_key_id clause at all.
func TestBuildUsageFilter_EmptyBoth_NoKeyClause(t *testing.T) {
	f := UsageFilter{}
	w := buildUsageFilter(f)
	sql := w.sql()
	if strings.Contains(sql, "api_key_id") {
		t.Fatalf("did not expect api_key_id clause; got: %q", sql)
	}
}

// TestBuildUsageFilter_TimeFormatMatchesTimeStr is the memory
// [82fa8ebabd] regression pin. SQLite compares TEXT timestamps
// lexicographically; the format string used by buildUsageFilter MUST
// match timeStr byte-for-byte, otherwise range queries silently miss.
func TestBuildUsageFilter_TimeFormatMatchesTimeStr(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 34, 56, 0, time.UTC)
	storage := timeStr(now)
	f := UsageFilter{From: now, To: now}
	w := buildUsageFilter(f)
	for _, p := range w.params {
		if ps, ok := p.(string); ok && ps != storage {
			t.Fatalf("filter time param %q does not match timeStr %q — lexicographic compare will silently fail", ps, storage)
		}
	}
}
