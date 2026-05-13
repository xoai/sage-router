package main

import (
	"sage-router/internal/auth"
	"sage-router/internal/store"
)

// authStoreAdapter bridges store.Store → auth.ConnectionStore. It exists only
// to satisfy the narrow interface auth.AuthStore depends on — auth cannot
// import store directly because store already imports auth for the
// NormalizeAuthType vocabulary helper. The adapter is intentionally
// minimal: it converts store.Connection to auth.ConnRow and forwards the
// other methods unchanged.
type authStoreAdapter struct {
	s store.Store
}

func newAuthStoreAdapter(s store.Store) *authStoreAdapter {
	return &authStoreAdapter{s: s}
}

func (a *authStoreAdapter) GetConnection(id string) (*auth.ConnRow, error) {
	c, err := a.s.GetConnection(id)
	if err != nil {
		return nil, err
	}
	return &auth.ConnRow{
		ID:           c.ID,
		Provider:     c.Provider,
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		ExpiresAt:    c.ExpiresAt,
		ProviderData: []byte(c.ProviderData),
	}, nil
}

func (a *authStoreAdapter) UpdateConnection(id string, updates map[string]any) error {
	return a.s.UpdateConnection(id, updates)
}

func (a *authStoreAdapter) BumpConnectionRefreshFailures(id string) (int, error) {
	return a.s.BumpConnectionRefreshFailures(id)
}

func (a *authStoreAdapter) GetConnectionRefreshFailures(id string) (int, error) {
	return a.s.GetConnectionRefreshFailures(id)
}
