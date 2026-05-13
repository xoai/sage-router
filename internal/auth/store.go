package auth

import (
	"encoding/json"
	"fmt"
	"time"
)

// ConnRow is the subset of store.Connection that AuthStore reads. It lives
// here (not in internal/store) to keep the dependency edge running
// store → auth instead of auth → store. A small adapter at the composition
// point (cmd/sage-router) maps store.Connection ↔ auth.ConnRow.
type ConnRow struct {
	ID           string
	Provider     string
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
	ProviderData []byte
}

// ConnectionStore is the narrow interface AuthStore depends on. The concrete
// implementation in internal/store satisfies a tiny adapter that implements
// this interface — see cmd/sage-router for the wiring.
type ConnectionStore interface {
	GetConnection(id string) (*ConnRow, error)
	UpdateConnection(id string, updates map[string]any) error
	BumpConnectionRefreshFailures(id string) (int, error)
	GetConnectionRefreshFailures(id string) (int, error)
}

// AuthStore is the credential-and-counter facade over the underlying SQLite
// store. It is the only place application code should construct Credentials
// from DB rows.
//
// Concurrency: AuthStore methods are safe for concurrent use, delegating to
// the underlying ConnectionStore which provides its own atomicity (e.g.,
// BumpConnectionRefreshFailures uses UPDATE … RETURNING).
//
// In-memory cache leads DB after a successful refresh + failed PutCredential:
// the in-memory token IS valid and gets used, but the DB still has the
// expired one. On process restart, the cold-start path pays one extra
// refresh against the stale-on-disk token. Acceptable; not silently
// corrupting state (auto-review M1-CO3).
type AuthStore struct {
	store ConnectionStore
}

// NewAuthStore wires AuthStore to a ConnectionStore implementation.
func NewAuthStore(s ConnectionStore) *AuthStore {
	return &AuthStore{store: s}
}

// providerData is the on-disk shape of Credential.ExtraData + AccountID,
// JSON-encoded into the connections.provider_data column.
type providerData struct {
	AccountID string         `json:"account_id,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// GetCredential reads the credential for the connection and assembles a
// *Credential. Returns a nil Credential and a nil error when the row exists
// but no access_token is set (e.g., an API-key connection) — callers can
// distinguish "no subscription cred" from "DB error" by checking nil.
func (a *AuthStore) GetCredential(connID string) (*Credential, error) {
	row, err := a.store.GetConnection(connID)
	if err != nil {
		return nil, fmt.Errorf("auth store: get connection %s: %w", connID, err)
	}
	if row.AccessToken == "" {
		return nil, nil
	}

	cred := &Credential{
		Provider:     row.Provider,
		ConnectionID: row.ID,
		AccessToken:  row.AccessToken,
		RefreshToken: row.RefreshToken,
	}
	if row.ExpiresAt != nil {
		cred.ExpiresAt = *row.ExpiresAt
	}

	if len(row.ProviderData) > 0 {
		var pd providerData
		if err := json.Unmarshal(row.ProviderData, &pd); err == nil {
			cred.AccountID = pd.AccountID
			cred.ExtraData = pd.Extra
		}
		// Malformed provider_data is non-fatal; the credential still works
		// without AccountID. Callers that need the JWT account_id can
		// re-extract it on the next refresh.
	}
	return cred, nil
}

// PutCredential writes the credential back to the connection row. Encryption
// of the secret columns is the underlying store's responsibility — AuthStore
// only marshals provider_data and dispatches the UPDATE.
func (a *AuthStore) PutCredential(connID string, cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("auth store: PutCredential(%s): nil credential", connID)
	}

	updates := map[string]any{
		"access_token":  cred.AccessToken,
		"refresh_token": cred.RefreshToken,
	}
	if !cred.ExpiresAt.IsZero() {
		updates["expires_at"] = cred.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
	}

	if cred.AccountID != "" || len(cred.ExtraData) > 0 {
		pd := providerData{
			AccountID: cred.AccountID,
			Extra:     cred.ExtraData,
		}
		raw, err := json.Marshal(pd)
		if err != nil {
			return fmt.Errorf("auth store: marshal provider_data: %w", err)
		}
		updates["provider_data"] = string(raw)
	}

	if err := a.store.UpdateConnection(connID, updates); err != nil {
		return fmt.Errorf("auth store: update connection %s: %w", connID, err)
	}
	return nil
}

// BumpRefreshFailure atomically increments the connection's
// refresh_failures counter and returns the new value. Callers use the
// returned count to drive the auto-disable threshold.
func (a *AuthStore) BumpRefreshFailure(connID string) (int, error) {
	n, err := a.store.BumpConnectionRefreshFailures(connID)
	if err != nil {
		return 0, fmt.Errorf("auth store: bump refresh_failures %s: %w", connID, err)
	}
	return n, nil
}

// ResetRefreshFailures sets the counter back to 0. Called on a successful
// refresh.
func (a *AuthStore) ResetRefreshFailures(connID string) error {
	if err := a.store.UpdateConnection(connID, map[string]any{"refresh_failures": 0}); err != nil {
		return fmt.Errorf("auth store: reset refresh_failures %s: %w", connID, err)
	}
	return nil
}

// GetRefreshFailures reads the current counter value.
func (a *AuthStore) GetRefreshFailures(connID string) (int, error) {
	n, err := a.store.GetConnectionRefreshFailures(connID)
	if err != nil {
		return 0, fmt.Errorf("auth store: get refresh_failures %s: %w", connID, err)
	}
	return n, nil
}
