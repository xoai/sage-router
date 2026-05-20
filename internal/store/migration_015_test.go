package store

import "testing"

// TestMigration015_NormalizesConnectionState pins migration 015: the
// connections.state column is normalized to the persistable Lifecycle/Auth
// vocabulary {idle, disabled, auth_expired}. Transient-health values
// (active/rate_limited/cooldown/errored/refreshing) are in-memory-only under
// the M2 three-facet circuit-breaker model and must not persist — they
// collapse to 'idle'. The migration is idempotent: after it runs, no row
// matches the transient set, so re-applying it changes zero rows.
//
// Cycle 20260520-m2-circuit-breaker, T1.
func TestMigration015_NormalizesConnectionState(t *testing.T) {
	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := db.(*sqliteStore)

	// Seed one connection per old-enum state value, writing the raw column
	// directly so the migration body is what is under test (not the store's
	// connection methods).
	seed := []struct{ id, state string }{
		{"c-active", "active"},
		{"c-ratelimited", "rate_limited"},
		{"c-cooldown", "cooldown"},
		{"c-errored", "errored"},
		{"c-refreshing", "refreshing"},
		{"c-idle", "idle"},
		{"c-disabled", "disabled"},
		{"c-authexpired", "auth_expired"},
	}
	for _, r := range seed {
		if _, err := s.db.Exec(
			`INSERT INTO connections (id, provider, name, state) VALUES (?, 'openai', ?, ?)`,
			r.id, r.id, r.state,
		); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}

	// Re-apply migration 015 directly against the freshly-seeded transient
	// rows. (Migrate() already ran it once at open, against an empty table.)
	sql, err := migrationsFS.ReadFile("migrations/015_normalize_connection_state.sql")
	if err != nil {
		t.Fatalf("read migration 015: %v", err)
	}
	if _, err := s.db.Exec(string(sql)); err != nil {
		t.Fatalf("apply migration 015: %v", err)
	}

	// Transient values collapse to idle; persistable values are untouched.
	want := map[string]string{
		"c-active":      "idle",
		"c-ratelimited": "idle",
		"c-cooldown":    "idle",
		"c-errored":     "idle",
		"c-refreshing":  "idle",
		"c-idle":        "idle",
		"c-disabled":    "disabled",
		"c-authexpired": "auth_expired",
	}
	for id, wantState := range want {
		var got string
		if err := s.db.QueryRow(`SELECT state FROM connections WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if got != wantState {
			t.Errorf("connection %s: state = %q, want %q", id, got, wantState)
		}
	}

	// Idempotency: no row matches the transient set after the migration, so a
	// re-run changes zero rows.
	var transientRemaining int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM connections
		 WHERE state IN ('active', 'rate_limited', 'cooldown', 'errored', 'refreshing')`,
	).Scan(&transientRemaining); err != nil {
		t.Fatalf("count transient rows: %v", err)
	}
	if transientRemaining != 0 {
		t.Errorf("%d transient state rows remain after migration 015 — not fully normalized",
			transientRemaining)
	}
}
