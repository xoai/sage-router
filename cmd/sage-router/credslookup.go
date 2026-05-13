package main

import (
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
func buildCredsLookup(lister connectionLister) func(providerID string) (catalog.ListerCredentials, bool) {
	return func(providerID string) (catalog.ListerCredentials, bool) {
		conns, err := lister.ListConnections(store.ConnectionFilter{Provider: providerID})
		if err != nil || len(conns) == 0 {
			return catalog.ListerCredentials{}, false
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
			return catalog.ListerCredentials{}, false
		}
		provDef, ok := config.KnownProviders[providerID]
		if !ok {
			return catalog.ListerCredentials{}, false
		}
		return catalog.ListerCredentials{
			BaseURL:     provDef.BaseURL,
			APIKey:      picked.APIKey,
			AccessToken: picked.AccessToken,
		}, true
	}
}
