package main

import (
	"context"
	"errors"
	"testing"

	"sage-router/internal/auth"
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

// fakeAuthStore stubs the credentialLoader interface. Keyed by connID;
// tests that don't care about ExtraHeaders can pass &fakeAuthStore{}.
type fakeAuthStore struct {
	byConnID map[string]*auth.Credential
}

func (f *fakeAuthStore) GetCredential(connID string) (*auth.Credential, error) {
	if f.byConnID == nil {
		return nil, nil
	}
	return f.byConnID[connID], nil
}

// TestBuildCredsLookup_ReturnsFalseForUnknownProvider — when the store
// has no connections for the queried provider, the lookup reports
// (zero, false). The 24h discovery ticker uses the bool to skip the
// provider for the cycle without logging spam.
func TestBuildCredsLookup_ReturnsFalseForUnknownProvider(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{}}
	lookup := buildCredsLookup(lister, &fakeAuthStore{})

	creds, _, ok := lookup("openai")
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
	lookup := buildCredsLookup(lister, &fakeAuthStore{})

	_, _, ok := lookup("anthropic")
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
	lookup := buildCredsLookup(lister, &fakeAuthStore{})

	creds, _, ok := lookup("openai")
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
	lookup := buildCredsLookup(lister, &fakeAuthStore{})

	creds, _, ok := lookup("anthropic")
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
	lookup := buildCredsLookup(lister, &fakeAuthStore{})

	if _, _, ok := lookup("openai"); ok {
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
	lookup := buildCredsLookup(lister, &fakeAuthStore{})

	creds, _, ok := lookup("openai")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if creds.APIKey != "sk-first" {
		t.Errorf("APIKey = %q, want sk-first (lowest-ID c-openai-1)", creds.APIKey)
	}
}

// TestBuildCredsLookup_PopulatesExtraHeadersForSubscription — for an
// OpenAI subscription connection with a known AccountID, the 24h
// ticker's credential lookup must include ChatGPT-Account-ID in
// ExtraHeaders so the discovery lister can authenticate. Without this,
// the periodic refresh has the same silent-fail mode the on-create
// path used to have (the user-reported bug).
func TestBuildCredsLookup_PopulatesExtraHeadersForSubscription(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{
		"openai": {{
			ID:          "c-openai-sub-1",
			Provider:    "openai",
			AuthType:    "subscription",
			AccessToken: "oauth-jwt-token",
			State:       "active",
		}},
	}}
	authStore := &fakeAuthStore{byConnID: map[string]*auth.Credential{
		"c-openai-sub-1": {
			Provider:    "openai",
			AccessToken: "oauth-jwt-token",
			AccountID:   "acct-456",
		},
	}}
	lookup := buildCredsLookup(lister, authStore)

	creds, _, ok := lookup("openai")
	if !ok {
		t.Fatalf("expected ok=true for enabled subscription connection")
	}
	if creds.ExtraHeaders == nil {
		t.Fatal("ExtraHeaders = nil, want populated map with ChatGPT-Account-ID")
	}
	if got := creds.ExtraHeaders["ChatGPT-Account-ID"]; got != "acct-456" {
		t.Errorf("ChatGPT-Account-ID = %q, want acct-456", got)
	}
}

// TestBuildCredsLookup_NoExtraHeadersForApikey — apikey connections
// don't need ExtraHeaders. The lookup must not query AuthStore (which
// would return nil anyway for apikey rows since AccessToken == "").
func TestBuildCredsLookup_NoExtraHeadersForApikey(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{
		"openai": {{
			ID:       "c-openai-apikey-1",
			Provider: "openai",
			AuthType: "apikey",
			APIKey:   "sk-classic",
			State:    "active",
		}},
	}}
	lookup := buildCredsLookup(lister, &fakeAuthStore{})

	creds, _, ok := lookup("openai")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if creds.ExtraHeaders != nil {
		t.Errorf("ExtraHeaders = %v, want nil for apikey auth", creds.ExtraHeaders)
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
	lookup := buildCredsLookup(lister, &fakeAuthStore{})

	if _, _, ok := lookup("made-up-provider"); ok {
		t.Errorf("expected ok=false for provider absent from config.KnownProviders")
	}
}

// TestBuildCredsLookup_ReturnsAuthTypeForSubscription — initiative
// 20260514-openrouter-fallback. The 24h ticker dispatches through
// catalog.DiscoveryListerKey using the auth_type of the picked
// connection. Verify the lookup returns "subscription" for a
// subscription connection so the dispatch resolves to the
// openrouter-mirror lister.
func TestBuildCredsLookup_ReturnsAuthTypeForSubscription(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{
		"openai": {{
			ID:          "c-sub-1",
			Provider:    "openai",
			AuthType:    "subscription",
			AccessToken: "jwt-token",
			State:       "active",
		}},
	}}
	lookup := buildCredsLookup(lister, &fakeAuthStore{})

	_, authType, ok := lookup("openai")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if authType != "subscription" {
		t.Errorf("authType = %q, want subscription", authType)
	}
}

// TestBuildCredsLookup_ReturnsAuthTypeForApikey — analogous coverage
// for apikey connections. Catches a future regression where the
// auth_type field silently drops to "" and breaks ticker dispatch.
func TestBuildCredsLookup_ReturnsAuthTypeForApikey(t *testing.T) {
	lister := &fakeConnLister{byProvider: map[string][]store.Connection{
		"openai": {{
			ID:       "c-apikey-1",
			Provider: "openai",
			AuthType: "apikey",
			APIKey:   "sk-test",
			State:    "active",
		}},
	}}
	lookup := buildCredsLookup(lister, &fakeAuthStore{})

	_, authType, ok := lookup("openai")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if authType != "apikey" {
		t.Errorf("authType = %q, want apikey", authType)
	}
}

// TestSelfHealV2_ClearsStaleSubscriptionScope403 — initiative
// 20260514-openrouter-fallback. Inject the exact 403 body the
// pre-fix binary produced, run wireCatalog, assert the row is
// cleared and the v2 gate is set.
func TestSelfHealV2_ClearsStaleSubscriptionScope403(t *testing.T) {
	st := freshDB(t)
	ctx := context.Background()

	if _, err := wireCatalog(ctx, st.DB()); err != nil {
		t.Fatalf("wireCatalog 1: %v", err)
	}
	// Clear the v2 gate so a second wireCatalog will actually scan.
	if _, err := st.DB().ExecContext(ctx,
		`DELETE FROM settings WHERE key = ?`, selfHealKeyV2,
	); err != nil {
		t.Fatalf("clear v2 gate: %v", err)
	}

	// Inject the captured 403 body verbatim (from the user's live DB).
	const staleErr = `lister openai: HTTP 403: {"error":"You have insufficient permissions for this operation. Missing scopes: api.model.read. Check that you have the correct role in your organization (Reader, Writer, Owner) and project (Viewer, Member, Owner), and if you're using a restricted API key,"}`
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE catalog_provider_meta
		 SET last_discovery_error = ?, backoff_step = 3
		 WHERE provider = 'openai'`, staleErr,
	); err != nil {
		t.Fatalf("inject stale: %v", err)
	}

	if _, err := wireCatalog(ctx, st.DB()); err != nil {
		t.Fatalf("wireCatalog 2: %v", err)
	}

	var errText string
	var backoff int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT last_discovery_error, backoff_step FROM catalog_provider_meta WHERE provider = 'openai'`,
	).Scan(&errText, &backoff); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if errText != "" {
		t.Errorf("last_discovery_error = %q, want '' (v2 self-heal didn't clear)", errText)
	}
	if backoff != 0 {
		t.Errorf("backoff_step = %d, want 0", backoff)
	}

	var gate string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, selfHealKeyV2,
	).Scan(&gate); err != nil {
		t.Fatalf("read v2 gate: %v", err)
	}
	if gate != "true" {
		t.Errorf("v2 gate = %q, want 'true'", gate)
	}
}

// TestSelfHealV2_DoesNotClearOtherErrors — narrow-match contract.
// A 403 from a DIFFERENT cause (no "Missing scopes: api.model.read"
// substring) must NOT be cleared. Catches a future regression where
// someone widens the SQL match to just "HTTP 403:".
func TestSelfHealV2_DoesNotClearOtherErrors(t *testing.T) {
	st := freshDB(t)
	ctx := context.Background()

	if _, err := wireCatalog(ctx, st.DB()); err != nil {
		t.Fatalf("wireCatalog 1: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`DELETE FROM settings WHERE key = ?`, selfHealKeyV2,
	); err != nil {
		t.Fatalf("clear v2 gate: %v", err)
	}

	const otherErr = `lister openai: HTTP 403: {"error":"API key revoked by administrator"}`
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE catalog_provider_meta
		 SET last_discovery_error = ?, backoff_step = 1
		 WHERE provider = 'openai'`, otherErr,
	); err != nil {
		t.Fatalf("inject other 403: %v", err)
	}

	if _, err := wireCatalog(ctx, st.DB()); err != nil {
		t.Fatalf("wireCatalog 2: %v", err)
	}

	var errText string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT last_discovery_error FROM catalog_provider_meta WHERE provider = 'openai'`,
	).Scan(&errText); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if errText != otherErr {
		t.Errorf("self-heal v2 incorrectly cleared non-scope 403: got %q, want %q", errText, otherErr)
	}
}
