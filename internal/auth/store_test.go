package auth

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeConnectionStore is an in-memory ConnectionStore for unit tests.
type fakeConnectionStore struct {
	mu              sync.Mutex
	row             *ConnRow
	updateCalls     []map[string]any
	refreshFailures int
	bumpErr         error
	getErr          error
}

func (f *fakeConnectionStore) GetConnection(id string) (*ConnRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.row == nil || f.row.ID != id {
		return nil, errors.New("not found")
	}
	cp := *f.row
	return &cp, nil
}

func (f *fakeConnectionStore) UpdateConnection(id string, updates map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Capture for assertions.
	cp := make(map[string]any, len(updates))
	for k, v := range updates {
		cp[k] = v
	}
	f.updateCalls = append(f.updateCalls, cp)
	// Mirror the updates into the row so subsequent Get sees them.
	if f.row != nil && f.row.ID == id {
		if v, ok := updates["access_token"]; ok {
			f.row.AccessToken = v.(string)
		}
		if v, ok := updates["refresh_token"]; ok {
			f.row.RefreshToken = v.(string)
		}
		if v, ok := updates["provider_data"]; ok {
			f.row.ProviderData = []byte(v.(string))
		}
		if v, ok := updates["expires_at"]; ok {
			t, _ := time.Parse("2006-01-02T15:04:05Z", v.(string))
			f.row.ExpiresAt = &t
		}
		if v, ok := updates["refresh_failures"]; ok {
			f.refreshFailures = v.(int)
		}
	}
	return nil
}

func (f *fakeConnectionStore) BumpConnectionRefreshFailures(id string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bumpErr != nil {
		return 0, f.bumpErr
	}
	f.refreshFailures++
	return f.refreshFailures, nil
}

func (f *fakeConnectionStore) GetConnectionRefreshFailures(id string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshFailures, nil
}

func TestAuthStore_GetCredential_AssemblesFromRow(t *testing.T) {
	expires := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	pd, _ := json.Marshal(providerData{AccountID: "acct-123", Extra: map[string]any{"scope": "user:inference"}})
	f := &fakeConnectionStore{row: &ConnRow{
		ID:           "c1",
		Provider:     "openai",
		AccessToken:  "atk",
		RefreshToken: "rtk",
		ExpiresAt:    &expires,
		ProviderData: pd,
	}}
	as := NewAuthStore(f)

	cred, err := as.GetCredential("c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cred == nil {
		t.Fatal("expected non-nil credential")
	}
	if cred.Provider != "openai" || cred.AccessToken != "atk" || cred.RefreshToken != "rtk" {
		t.Errorf("unexpected credential: %+v", cred)
	}
	if !cred.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %v, want %v", cred.ExpiresAt, expires)
	}
	if cred.AccountID != "acct-123" {
		t.Errorf("AccountID = %q, want acct-123", cred.AccountID)
	}
	if cred.ExtraData["scope"] != "user:inference" {
		t.Errorf("ExtraData.scope = %v, want user:inference", cred.ExtraData["scope"])
	}
}

func TestAuthStore_GetCredential_EmptyAccessTokenReturnsNil(t *testing.T) {
	// API-key connections have no access_token; AuthStore should return nil
	// (with nil err) rather than synthesizing a partial Credential.
	f := &fakeConnectionStore{row: &ConnRow{ID: "c1", Provider: "openai"}}
	cred, err := NewAuthStore(f).GetCredential("c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cred != nil {
		t.Errorf("got %+v, want nil", cred)
	}
}

func TestAuthStore_GetCredential_MalformedProviderData_IsNonFatal(t *testing.T) {
	f := &fakeConnectionStore{row: &ConnRow{
		ID:           "c1",
		Provider:     "openai",
		AccessToken:  "atk",
		ProviderData: []byte("not-json"),
	}}
	cred, err := NewAuthStore(f).GetCredential("c1")
	if err != nil {
		t.Fatalf("malformed provider_data should be non-fatal; got error: %v", err)
	}
	if cred == nil || cred.AccessToken != "atk" {
		t.Errorf("malformed provider_data dropped the whole credential: %+v", cred)
	}
	if cred.AccountID != "" {
		t.Errorf("AccountID should be empty when provider_data is malformed; got %q", cred.AccountID)
	}
}

func TestAuthStore_PutCredential_WritesCanonicalFields(t *testing.T) {
	f := &fakeConnectionStore{row: &ConnRow{ID: "c1", Provider: "openai"}}
	as := NewAuthStore(f)

	expires := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	cred := &Credential{
		Provider:     "openai",
		ConnectionID: "c1",
		AccessToken:  "new-atk",
		RefreshToken: "new-rtk",
		ExpiresAt:    expires,
		AccountID:    "acct-xyz",
		ExtraData:    map[string]any{"scope": "openid email"},
	}

	if err := as.PutCredential("c1", cred); err != nil {
		t.Fatalf("put: %v", err)
	}
	if len(f.updateCalls) != 1 {
		t.Fatalf("want 1 update call, got %d", len(f.updateCalls))
	}
	upd := f.updateCalls[0]

	wantFields := map[string]any{
		"access_token":  "new-atk",
		"refresh_token": "new-rtk",
	}
	for k, want := range wantFields {
		if upd[k] != want {
			t.Errorf("update[%s] = %v, want %v", k, upd[k], want)
		}
	}
	if upd["expires_at"] != expires.Format("2006-01-02T15:04:05Z") {
		t.Errorf("expires_at = %v, want formatted UTC", upd["expires_at"])
	}

	// provider_data is JSON-encoded.
	raw, ok := upd["provider_data"].(string)
	if !ok {
		t.Fatalf("provider_data should be string, got %T", upd["provider_data"])
	}
	var pd providerData
	if err := json.Unmarshal([]byte(raw), &pd); err != nil {
		t.Fatalf("provider_data unmarshal: %v", err)
	}
	if pd.AccountID != "acct-xyz" || pd.Extra["scope"] != "openid email" {
		t.Errorf("provider_data round-trip wrong: %+v", pd)
	}
}

func TestAuthStore_PutCredential_NilErrors(t *testing.T) {
	as := NewAuthStore(&fakeConnectionStore{})
	if err := as.PutCredential("c1", nil); err == nil {
		t.Error("PutCredential(nil) should error")
	}
}

func TestAuthStore_PutCredential_SkipsProviderDataWhenEmpty(t *testing.T) {
	// Without AccountID or ExtraData, we don't need to touch provider_data
	// (avoids clobbering whatever was there).
	f := &fakeConnectionStore{row: &ConnRow{ID: "c1", Provider: "openai"}}
	as := NewAuthStore(f)
	if err := as.PutCredential("c1", &Credential{
		Provider: "openai", AccessToken: "atk", RefreshToken: "rtk",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, ok := f.updateCalls[0]["provider_data"]; ok {
		t.Error("provider_data should be absent when AccountID and ExtraData are empty")
	}
}

func TestAuthStore_RefreshFailures_BumpResetGet(t *testing.T) {
	f := &fakeConnectionStore{row: &ConnRow{ID: "c1", Provider: "openai"}}
	as := NewAuthStore(f)

	for i := 1; i <= 3; i++ {
		got, err := as.BumpRefreshFailure("c1")
		if err != nil {
			t.Fatalf("bump %d: %v", i, err)
		}
		if got != i {
			t.Errorf("bump %d returned %d, want %d", i, got, i)
		}
	}

	n, err := as.GetRefreshFailures("c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n != 3 {
		t.Errorf("GetRefreshFailures = %d, want 3", n)
	}

	if err := as.ResetRefreshFailures("c1"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if n, _ := as.GetRefreshFailures("c1"); n != 0 {
		t.Errorf("after reset, got %d, want 0", n)
	}
}

func TestAuthStore_BumpRefreshFailure_PropagatesError(t *testing.T) {
	f := &fakeConnectionStore{bumpErr: errors.New("db gone")}
	as := NewAuthStore(f)
	if _, err := as.BumpRefreshFailure("c1"); err == nil {
		t.Error("expected error to propagate")
	}
}
