package main

import (
	"errors"
	"testing"

	"sage-router/internal/config"
	"sage-router/internal/store"
)

// fakeConnLister stubs the connectionLister interface for buildCredsLookup
// unit tests. Keyed by provider; returns the configured slice (or error).
type fakeConnLister struct {
	byProvider map[string][]store.Connection
	listErr    error
}

func (f *fakeConnLister) ListConnections(filter store.ConnectionFilter) ([]store.Connection, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byProvider[filter.Provider], nil
}

// TestBuildCredsLookup_ReturnsFalseForUnknownProvider — when the store
// has no connections for the queried provider, the lookup reports
// (zero, false). The 24h discovery ticker uses the bool to skip the
// provider for the cycle without logging spam.
func TestBuildCredsLookup_ReturnsFalseForUnknownProvider(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{}}
	lookup := buildCredsLookup(lister)

	creds, ok := lookup("openai")
	if ok {
		t.Errorf("expected ok=false for provider with no connections; got creds=%+v", creds)
	}
}

// TestBuildCredsLookup_ListErrorReportsFalse — store-level errors
// behave the same as missing rows: lookup reports false rather than
// propagating the error, since the discovery ticker has no error
// channel and the next cycle (24h) will retry.
func TestBuildCredsLookup_ListErrorReportsFalse(t *testing.T) {
	lister := &fakeConnLister{listErr: errors.New("db: connection refused")}
	lookup := buildCredsLookup(lister)

	_, ok := lookup("anthropic")
	if ok {
		t.Errorf("expected ok=false on store error; lookup must not propagate")
	}
}

// TestBuildCredsLookup_PicksApikeyConnection — for a provider with one
// enabled apikey connection, the lookup builds ListerCredentials with
// the connection's APIKey populated and the provider's static BaseURL.
func TestBuildCredsLookup_PicksApikeyConnection(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{
		"openai": {{
			ID:       "c-openai-1",
			Provider: "openai",
			AuthType: "apikey",
			APIKey:   "sk-test-12345",
			State:    "active",
		}},
	}}
	lookup := buildCredsLookup(lister)

	creds, ok := lookup("openai")
	if !ok {
		t.Fatalf("expected ok=true for enabled apikey connection")
	}
	if creds.APIKey != "sk-test-12345" {
		t.Errorf("APIKey = %q, want sk-test-12345", creds.APIKey)
	}
	if creds.BaseURL != config.KnownProviders["openai"].BaseURL {
		t.Errorf("BaseURL = %q, want %q from config.KnownProviders",
			creds.BaseURL, config.KnownProviders["openai"].BaseURL)
	}
}

// TestBuildCredsLookup_PicksSubscriptionConnection — for a provider
// with one enabled subscription connection (AccessToken instead of
// APIKey), the lookup builds ListerCredentials with AccessToken
// populated. The discovery lister decides which credential field to
// use; this adapter just forwards both.
func TestBuildCredsLookup_PicksSubscriptionConnection(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{
		"anthropic": {{
			ID:          "c-anthropic-1",
			Provider:    "anthropic",
			AuthType:    "subscription",
			AccessToken: "oauth-access-token",
			State:       "active",
		}},
	}}
	lookup := buildCredsLookup(lister)

	creds, ok := lookup("anthropic")
	if !ok {
		t.Fatalf("expected ok=true for enabled subscription connection")
	}
	if creds.AccessToken != "oauth-access-token" {
		t.Errorf("AccessToken = %q, want oauth-access-token", creds.AccessToken)
	}
	if creds.APIKey != "" {
		t.Errorf("APIKey = %q on subscription connection; want empty", creds.APIKey)
	}
	if creds.BaseURL != config.KnownProviders["anthropic"].BaseURL {
		t.Errorf("BaseURL = %q, want %q from config.KnownProviders",
			creds.BaseURL, config.KnownProviders["anthropic"].BaseURL)
	}
}

// TestBuildCredsLookup_SkipsDisabledConnections — disabled connections
// are not picked. With every connection disabled the lookup reports
// false, so the discovery ticker treats the provider as if it had no
// connections at all.
func TestBuildCredsLookup_SkipsDisabledConnections(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{
		"openai": {{
			ID:       "c-1",
			Provider: "openai",
			APIKey:   "sk-disabled",
			State:    "disabled",
		}},
	}}
	lookup := buildCredsLookup(lister)

	if _, ok := lookup("openai"); ok {
		t.Errorf("expected ok=false when every connection is disabled")
	}
}

// TestBuildCredsLookup_PicksLowestIDForDeterminism — when multiple
// non-disabled connections exist for the same provider, the lookup
// picks the lowest-ID one. This matches buildSmartCandidates'
// sample-connection pattern so the discovery cycle is deterministic
// across processes / restarts with the same DB state.
func TestBuildCredsLookup_PicksLowestIDForDeterminism(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{
		"openai": {
			{ID: "c-openai-3", Provider: "openai", APIKey: "sk-third", State: "active"},
			{ID: "c-openai-1", Provider: "openai", APIKey: "sk-first", State: "active"},
			{ID: "c-openai-2", Provider: "openai", APIKey: "sk-second", State: "active"},
		},
	}}
	lookup := buildCredsLookup(lister)

	creds, ok := lookup("openai")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if creds.APIKey != "sk-first" {
		t.Errorf("APIKey = %q, want sk-first (lowest-ID c-openai-1)", creds.APIKey)
	}
}

// TestBuildCredsLookup_UnknownProviderInConfig — even with a valid
// connection in the store, if the provider isn't in config.KnownProviders
// (no BaseURL), the lookup reports false. A real Lister would have no
// idea where to dispatch.
func TestBuildCredsLookup_UnknownProviderInConfig(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{
		"made-up-provider": {{
			ID:       "c-1",
			Provider: "made-up-provider",
			APIKey:   "sk-test",
			State:    "active",
		}},
	}}
	lookup := buildCredsLookup(lister)

	if _, ok := lookup("made-up-provider"); ok {
		t.Errorf("expected ok=false for provider absent from config.KnownProviders")
	}
}
