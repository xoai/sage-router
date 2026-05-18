package store

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertRawConn(t *testing.T, s *sqliteStore, id, authType string) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO connections (id, provider, name, auth_type) VALUES (?, 'openai', ?, ?)`,
		id, id, authType,
	)
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func TestScanConnection_NormalizesLegacyAuthType(t *testing.T) {
	// AC1c: store reads canonicalize legacy vocabulary.
	st := newTestStore(t)
	s := st.(*sqliteStore)

	insertRawConn(t, s, "legacy-apikey", "api_key")
	insertRawConn(t, s, "legacy-oauth", "oauth")
	insertRawConn(t, s, "canonical-apikey", "apikey")

	want := map[string]string{
		"legacy-apikey":    "apikey",
		"legacy-oauth":     "subscription",
		"canonical-apikey": "apikey",
	}
	for id, expect := range want {
		conn, err := st.GetConnection(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if conn.AuthType != expect {
			t.Errorf("%s: AuthType = %q, want %q", id, conn.AuthType, expect)
		}
	}
}

func TestScanConnection_WarnsOnUnexpectedAuthType(t *testing.T) {
	// AC1c second branch: a typo'd row produces a WARN log line but doesn't
	// crash. The value passes through unchanged.
	st := newTestStore(t)
	s := st.(*sqliteStore)
	insertRawConn(t, s, "weird", "garbage")

	// Capture slog output by replacing the default logger.
	var buf bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })

	conn, err := st.GetConnection("weird")
	if err != nil {
		t.Fatalf("get weird: %v", err)
	}
	if conn.AuthType != "garbage" {
		t.Errorf("unknown value should pass through; got %q want %q", conn.AuthType, "garbage")
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "unexpected auth_type") {
		t.Errorf("expected WARN log about unexpected auth_type; got %q", logOutput)
	}
	if !strings.Contains(logOutput, "conn_id=weird") {
		t.Errorf("WARN log should include conn_id; got %q", logOutput)
	}
}

func TestCreateConnection_NormalizesAuthTypeOnWrite(t *testing.T) {
	// AC2b: write paths set canonical AuthType. Even if the caller passes
	// legacy vocabulary, the row hits disk in canonical form.
	st := newTestStore(t)
	s := st.(*sqliteStore)

	c := &Connection{
		ID:       "c1",
		Provider: "openai",
		Name:     "Test",
		AuthType: "api_key", // legacy input
	}
	if err := st.CreateConnection(c); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	// In-memory struct mutated to canonical form.
	if c.AuthType != "apikey" {
		t.Errorf("after CreateConnection: c.AuthType = %q, want apikey", c.AuthType)
	}

	// DB row stored as canonical (verified by reading the raw value).
	var raw string
	if err := s.db.QueryRow("SELECT auth_type FROM connections WHERE id = 'c1'").Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if raw != "apikey" {
		t.Errorf("raw DB value = %q, want apikey", raw)
	}
}

func TestUpdateConnection_NormalizesAuthTypeOnWrite(t *testing.T) {
	st := newTestStore(t)
	s := st.(*sqliteStore)

	if err := st.CreateConnection(&Connection{ID: "c1", Provider: "openai", Name: "T", AuthType: "apikey"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.UpdateConnection("c1", map[string]any{"auth_type": "oauth"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	var raw string
	if err := s.db.QueryRow("SELECT auth_type FROM connections WHERE id = 'c1'").Scan(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if raw != "subscription" {
		t.Errorf("raw DB value after update = %q, want subscription", raw)
	}
}

func TestRefreshFailures_Roundtrip(t *testing.T) {
	// AC7 prep: DB-canonical counter, atomic increment via RETURNING.
	st := newTestStore(t)

	if err := st.CreateConnection(&Connection{ID: "c1", Provider: "openai", Name: "T", AuthType: "subscription"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Initial value.
	n, err := st.GetConnectionRefreshFailures("c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n != 0 {
		t.Errorf("initial refresh_failures = %d, want 0", n)
	}

	// Bump three times.
	for i := 1; i <= 3; i++ {
		got, err := st.BumpConnectionRefreshFailures("c1")
		if err != nil {
			t.Fatalf("bump %d: %v", i, err)
		}
		if got != i {
			t.Errorf("bump %d returned %d, want %d", i, got, i)
		}
	}

	// Reset to 0 via UpdateConnection.
	if err := st.UpdateConnection("c1", map[string]any{"refresh_failures": 0}); err != nil {
		t.Fatalf("reset via update: %v", err)
	}
	n, err = st.GetConnectionRefreshFailures("c1")
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if n != 0 {
		t.Errorf("after reset, refresh_failures = %d, want 0", n)
	}
}

func TestRefreshFailures_ConcurrentBumps_Atomic(t *testing.T) {
	// Stress test: 20 goroutines each bump once → final value is exactly 20.
	st := newTestStore(t)
	if err := st.CreateConnection(&Connection{ID: "c1", Provider: "openai", Name: "T", AuthType: "subscription"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.BumpConnectionRefreshFailures("c1"); err != nil {
				t.Errorf("bump: %v", err)
			}
		}()
	}
	wg.Wait()

	n, err := st.GetConnectionRefreshFailures("c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n != 20 {
		t.Errorf("after 20 concurrent bumps, refresh_failures = %d, want 20", n)
	}
}

func TestRefreshFailures_NotFound(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.BumpConnectionRefreshFailures("missing"); err == nil {
		t.Error("expected error bumping nonexistent connection")
	}
	if _, err := st.GetConnectionRefreshFailures("missing"); err == nil {
		t.Error("expected error getting nonexistent connection")
	}
}

func TestConnection_RefreshFailuresColumnRoundTrip(t *testing.T) {
	// Ensure the new column flows through scanConnection.
	st := newTestStore(t)
	if err := st.CreateConnection(&Connection{ID: "c1", Provider: "openai", Name: "T", AuthType: "subscription"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.BumpConnectionRefreshFailures("c1"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	conn, err := st.GetConnection("c1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if conn.RefreshFailures != 1 {
		t.Errorf("RefreshFailures = %d, want 1", conn.RefreshFailures)
	}

	// JSON roundtrip exposes the field for API responses.
	b, err := json.Marshal(conn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"refresh_failures":1`)) {
		t.Errorf("JSON should include refresh_failures; got %s", b)
	}
}


// 3 ExchangedToken roundtrip + encryption tests deleted in cycle
// 20260517-provider-auth-variants M2.6.6 — migration 013 drops the
// exchanged_token column entirely (RFC 8693 chain was wrong-path).
