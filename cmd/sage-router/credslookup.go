package main

import (
	"sage-router/internal/auth"
	"sage-router/internal/catalog"
	"sage-router/internal/config"
	"sage-router/internal/store"
)

// connectionLister is the narrow interface buildCredsLookup needs from the
// store. Extracted from store.Store so credslookup_test.go can drive it
// with a fake without standing up a real SQLite database.
type connectionLister interface {
	ListConnections(filter store.ConnectionFilter) ([]store.Connection, error)
}

// credentialLoader is the narrow interface buildCredsLookup needs from
// AuthStore to surface provider-specific ExtraHeaders (e.g., the
// ChatGPT-Account-ID header for OpenAI subscription connections).
// Mirrors *auth.AuthStore.GetCredential's contract — extracted as an
// interface so credslookup_test.go can pass a fake without spinning up
// a real auth store.
type credentialLoader interface {
	GetCredential(connID string) (*auth.Credential, error)
}

// buildCredsLookup returns the catalog.CredsLookup function the background
// discovery loop uses to obtain a credential for each provider. The closure
// walks store-side connections per provider, picks the lowest-ID
// non-disabled connection for deterministic selection (mirrors the
// sample-connection pattern in buildSmartCandidates), and assembles
// ListerCredentials from the connection's API key / access token plus
// the provider's static BaseURL.
//
// Returns (creds, false) when:
//   - the store has no connections for the provider, or the list errors;
//   - every connection for the provider is in the "disabled" state;
//   - the provider is not in config.KnownProviders (no BaseURL available).
//
// Lifted out of main.go (M2.13 wiring) per M3 polish item #34 so the
// CRITICAL-1 fix this adapter implements has unit-test coverage on the
// adapter itself, independent of the goroutine spawned by
// catalog.StartBackgroundRefresh.
//
// authStore is required (not nil-tolerant) so wiring bugs surface at
// startup rather than masking as silently-empty ExtraHeaders.
//
// Returns (creds, authType, ok). The authType is needed by the 24h
// ticker to dispatch through catalog.DiscoveryListerKey (e.g.,
// subscription openai → "openai@openrouter-mirror"). See fix
// 20260514-openrouter-fallback.
func buildCredsLookup(lister connectionLister, authStore credentialLoader) func(providerID string) (catalog.ListerCredentials, string, bool) {
	return func(providerID string) (catalog.ListerCredentials, string, bool) {
		conns, err := lister.ListConnections(store.ConnectionFilter{Provider: providerID})
		if err != nil || len(conns) == 0 {
			return catalog.ListerCredentials{}, "", false
		}
		var picked *store.Connection
		for i := range conns {
			c := &conns[i]
			if c.State == "disabled" {
				continue
			}
			if picked == nil || c.ID < picked.ID {
				picked = c
			}
		}
		if picked == nil {
			return catalog.ListerCredentials{}, "", false
		}
		provDef, ok := config.KnownProviders[providerID]
		if !ok {
			return catalog.ListerCredentials{}, "", false
		}
		creds := catalog.ListerCredentials{
			BaseURL:     provDef.BaseURL,
			APIKey:      picked.APIKey,
			AccessToken: picked.AccessToken,
		}
		// Thread provider-specific ExtraHeaders for subscription connections
		// (e.g., ChatGPT-Account-ID for openai). Mirrors routes_v1.go:1031-1035
		// and routes_api.go:buildListerCredentials so the 24h ticker sees the
		// same auth shape as on-create discovery + chat-completion requests.
		if picked.AuthType == auth.AuthTypeSubscription {
			if cred, gerr := authStore.GetCredential(picked.ID); gerr == nil && cred != nil {
				creds.ExtraHeaders = cred.ExtraHeaders()
			}
		}
		return creds, picked.AuthType, true
	}
}
