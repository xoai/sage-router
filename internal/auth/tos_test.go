package auth

import (
	"errors"
	"sync"
	"testing"
)

// fakeSettingsStore is the narrowest interface TOS helpers need from a store —
// just GetSetting / SetSetting on the existing settings table.
type fakeSettingsStore struct {
	mu      sync.Mutex
	values  map[string]string
	getErr  error
	setErr  error
}

func newFakeSettingsStore() *fakeSettingsStore {
	return &fakeSettingsStore{values: map[string]string{}}
}

func (f *fakeSettingsStore) GetSetting(key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return "", f.getErr
	}
	v, ok := f.values[key]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (f *fakeSettingsStore) SetSetting(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.values[key] = value
	return nil
}

func TestIsTOSAcknowledged_FalseWhenUnset(t *testing.T) {
	s := newFakeSettingsStore()
	if IsTOSAcknowledged(s) {
		t.Error("IsTOSAcknowledged should be false when setting is not present")
	}
}

func TestIsTOSAcknowledged_TrueAfterAcknowledge(t *testing.T) {
	s := newFakeSettingsStore()
	if err := AcknowledgeTOS(s); err != nil {
		t.Fatalf("AcknowledgeTOS: %v", err)
	}
	if !IsTOSAcknowledged(s) {
		t.Error("IsTOSAcknowledged should be true after AcknowledgeTOS")
	}
}

func TestAcknowledgeTOS_Idempotent(t *testing.T) {
	s := newFakeSettingsStore()
	if err := AcknowledgeTOS(s); err != nil {
		t.Fatalf("first ack: %v", err)
	}
	if err := AcknowledgeTOS(s); err != nil {
		t.Fatalf("second ack: %v", err)
	}
	if !IsTOSAcknowledged(s) {
		t.Error("after two acks, IsTOSAcknowledged should be true")
	}
}

func TestIsTOSAcknowledged_FalseForUnexpectedValue(t *testing.T) {
	// Some future code path might set the setting to something other than
	// "1" (e.g., a "0" rollback). Only "1" is truthy.
	s := newFakeSettingsStore()
	s.values[settingSubscriptionTOSAcknowledged] = "0"
	if IsTOSAcknowledged(s) {
		t.Error("setting value '0' should not be considered acknowledged")
	}

	s.values[settingSubscriptionTOSAcknowledged] = ""
	if IsTOSAcknowledged(s) {
		t.Error("empty setting value should not be considered acknowledged")
	}
}

func TestAcknowledgeTOS_PropagatesError(t *testing.T) {
	s := newFakeSettingsStore()
	s.setErr = errors.New("disk full")
	if err := AcknowledgeTOS(s); err == nil {
		t.Error("expected error to propagate")
	}
}

func TestTOSText_NonEmpty(t *testing.T) {
	// The displayed text is part of the user-facing contract. It MUST
	// mention the TOS-changes risk so users have informed consent.
	if TOSText == "" {
		t.Fatal("TOSText must not be empty")
	}
	required := []string{"subscription", "Terms of Service", "API key"}
	for _, frag := range required {
		if !containsCaseInsensitive(TOSText, frag) {
			t.Errorf("TOSText missing required phrase %q; current text:\n%s", frag, TOSText)
		}
	}
}

func containsCaseInsensitive(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	hl := lower(haystack)
	nl := lower(needle)
	for i := 0; i+len(nl) <= len(hl); i++ {
		if hl[i:i+len(nl)] == nl {
			return true
		}
	}
	return false
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
