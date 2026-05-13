package store

import (
	"errors"
	"strings"
	"testing"
)

// Settings package tests.
//
// The Store interface's settings contract (store.go:GetSetting /
// SetSetting / AllSettings) carries an important shape distinction
// that callers depend on: a missing row returns a wrapped
// ErrSettingNotFound sentinel, NOT an empty string with nil error.
// This lets callers distinguish "row absent" (e.g., non-bootstrapped
// DB, manually-edited rows) from "I/O failure" (e.g., disk corruption,
// connection lost). Pre-fix: GetSetting returned an unwrapped
// fmt.Errorf which made the distinction invisible to errors.Is, and
// the `cmd/sage-router/main.go` openrouter-refresh boot switch added
// in M3 polish item #10 carried a dead arm because it tried to test
// for the absent case via `value == ""` while every missing-row
// return path forced a non-nil error.

// TestGetSetting_MissingKeyReturnsErrSettingNotFound — fix
// initiative 20260513-getsetting-sentinel AC-Fix-1. The wrapped
// ErrSettingNotFound is what cmd/sage-router/main.go's switch
// inspects via errors.Is to distinguish the absent case from a
// generic DB error. A future refactor that unwrapped the sentinel
// (e.g., reverting to fmt.Errorf without %w) would silently break
// the boot-time warning differentiation; this test fails loudly
// in that scenario.
func TestGetSetting_MissingKeyReturnsErrSettingNotFound(t *testing.T) {
	s := newTestStore(t)

	value, err := s.GetSetting("nonexistent_key")
	if err == nil {
		t.Fatalf("expected non-nil error for missing key; got value=%q err=nil", value)
	}
	if !errors.Is(err, ErrSettingNotFound) {
		t.Errorf("errors.Is(err, ErrSettingNotFound) = false; got err: %v", err)
	}
	if value != "" {
		t.Errorf("value = %q, want \"\" (no value should be returned for missing keys)", value)
	}
}

// TestGetSetting_PresentKeyReturnsValueNilError — baseline contract.
// A row that exists returns its value with nil error. SetSetting +
// GetSetting are the inverse of each other on the happy path.
func TestGetSetting_PresentKeyReturnsValueNilError(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSetting("greeting", "hello"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	value, err := s.GetSetting("greeting")
	if err != nil {
		t.Errorf("unexpected error for present key: %v", err)
	}
	if value != "hello" {
		t.Errorf("value = %q, want \"hello\"", value)
	}
}

// TestGetSetting_EmptyValueIsDistinctFromMissing — the load-bearing
// invariant the dead-arm bug exposed. An explicitly-stored empty
// value returns ("", nil); a missing row returns ("", wrapped
// ErrSettingNotFound). The two must be distinguishable so callers
// can branch (the openrouter refresh boot switch needs this exact
// distinction — empty value should be treated as a configured-off
// state, NOT as a missing-bootstrap state).
func TestGetSetting_EmptyValueIsDistinctFromMissing(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSetting("explicit_empty", ""); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	// Explicit empty: ("", nil)
	value, err := s.GetSetting("explicit_empty")
	if err != nil {
		t.Errorf("explicit empty: unexpected error: %v", err)
	}
	if value != "" {
		t.Errorf("explicit empty: value = %q, want \"\"", value)
	}

	// Missing: ("", wrapped ErrSettingNotFound)
	_, missingErr := s.GetSetting("never_set")
	if !errors.Is(missingErr, ErrSettingNotFound) {
		t.Errorf("missing: errors.Is(err, ErrSettingNotFound) = false; got: %v", missingErr)
	}
}

// TestGetSetting_MissingKeyErrorIncludesKeyName — fix initiative
// 20260513-getsetting-sentinel AC-Fix-4. Diagnostic preservation:
// the wrapped error message must still contain the key name so
// operators reading log output can identify which setting was
// absent. A regression that wrapped the sentinel without the key
// (e.g., a bare `fmt.Errorf("%w", ErrSettingNotFound)`) would lose
// the diagnostic value and this test catches it.
func TestGetSetting_MissingKeyErrorIncludesKeyName(t *testing.T) {
	s := newTestStore(t)

	const key = "openrouter_refresh_enabled"
	_, err := s.GetSetting(key)
	if err == nil {
		t.Fatalf("expected non-nil error for missing key")
	}
	if !strings.Contains(err.Error(), key) {
		t.Errorf("error message %q does not contain key name %q; operators will not be able to identify the missing setting from logs",
			err.Error(), key)
	}
}
