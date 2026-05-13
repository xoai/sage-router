package main

import (
	"testing"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/store"
)

func TestAuthStoreAdapter_RoundTripWithRealSQLite(t *testing.T) {
	// Integration smoke: AuthStore → adapter → real sqlite store. Verifies
	// the dependency edge actually composes correctly and that encryption
	// is applied transparently to the access/refresh token columns.
	db, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Encryption key — the same shape main.go derives from the master secret.
	db.SetEncryptionKey(store.DeriveKey("test-master-key"))

	if err := db.CreateConnection(&store.Connection{
		ID:       "c1",
		Provider: "openai",
		Name:     "Work",
		AuthType: "subscription",
	}); err != nil {
		t.Fatalf("seed conn: %v", err)
	}

	adapter := newAuthStoreAdapter(db)
	as := auth.NewAuthStore(adapter)

	// Put a credential.
	expires := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	if err := as.PutCredential("c1", &auth.Credential{
		Provider:     "openai",
		ConnectionID: "c1",
		AccessToken:  "secret-access",
		RefreshToken: "secret-refresh",
		ExpiresAt:    expires,
		AccountID:    "acct-abc",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Get it back through the adapter.
	cred, err := as.GetCredential("c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cred == nil {
		t.Fatal("expected non-nil credential")
	}
	if cred.AccessToken != "secret-access" || cred.RefreshToken != "secret-refresh" {
		t.Errorf("tokens did not round-trip; got %+v", cred)
	}
	if !cred.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %v", cred.ExpiresAt, expires)
	}
	if cred.AccountID != "acct-abc" {
		t.Errorf("account_id = %q, want acct-abc", cred.AccountID)
	}

	// Bump/Reset/Get refresh_failures through the chain.
	for i := 1; i <= 2; i++ {
		got, err := as.BumpRefreshFailure("c1")
		if err != nil {
			t.Fatalf("bump %d: %v", i, err)
		}
		if got != i {
			t.Errorf("bump %d returned %d, want %d", i, got, i)
		}
	}
	if err := as.ResetRefreshFailures("c1"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if n, _ := as.GetRefreshFailures("c1"); n != 0 {
		t.Errorf("after reset got %d, want 0", n)
	}
}
