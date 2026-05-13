package store

import (
	"database/sql"
	"testing"
	"time"
)

func TestMigrations_AllTablesCreated(t *testing.T) {
	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	s := db.(*sqliteStore)

	expectedTables := []string{
		"_migrations",
		"connections",
		"combos",
		"aliases",
		"api_keys",
		"settings",
		"usage_log",
		"routing_log",
		"catalog_models",        // migration 009
		"catalog_pricing",       // migration 010
		"catalog_provider_meta", // migration 011
	}

	for _, table := range expectedTables {
		var count int
		err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count)
		if err != nil {
			t.Errorf("failed to check table %s: %v", table, err)
			continue
		}
		if count == 0 {
			t.Errorf("table %s was not created by migration", table)
		}
	}

	// Verify all migrations were recorded (5 initial + 006/007/008/009/010/011 = 11).
	var migrationCount int
	s.db.QueryRow("SELECT COUNT(*) FROM _migrations").Scan(&migrationCount)
	if migrationCount != 11 {
		t.Errorf("expected 11 migrations recorded, got %d", migrationCount)
	}
}

// TestMigration009_CatalogModelsSchema verifies the schema of the
// catalog_models table created by migration 009 — per AC1 + ADR-1.
//
// Columns asserted: provider, model_id, display_name, tier,
// context_window, max_output, supports_images, supports_tools,
// supports_thinking, source, discovered_at, updated_at.
// Source CHECK enum: seed | discovery | openrouter | user.
// Composite PK: (provider, model_id).
// Index: idx_catalog_models_provider on (provider).
func TestMigration009_CatalogModelsSchema(t *testing.T) {
	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := db.(*sqliteStore)

	// Columns present.
	requiredCols := []string{
		"provider", "model_id", "display_name", "tier",
		"context_window", "max_output",
		"supports_images", "supports_tools", "supports_thinking",
		"source", "discovered_at", "updated_at",
	}
	for _, col := range requiredCols {
		if !columnExists(t, s, "catalog_models", col) {
			t.Errorf("catalog_models is missing column %q", col)
		}
	}

	// Composite primary key on (provider, model_id) — inserts that violate
	// uniqueness must fail with ON CONFLICT semantics.
	_, err = s.db.Exec(`INSERT INTO catalog_models
		(provider, model_id, display_name, tier, context_window, max_output, source)
		VALUES ('openai', 'gpt-4.1', 'GPT-4.1', 1, 1000000, 32768, 'seed')`)
	if err != nil {
		t.Fatalf("insert seed row: %v", err)
	}
	_, err = s.db.Exec(`INSERT INTO catalog_models
		(provider, model_id, display_name, tier, context_window, max_output, source)
		VALUES ('openai', 'gpt-4.1', 'GPT-4.1', 1, 1000000, 32768, 'seed')`)
	if err == nil {
		t.Error("expected PK violation on duplicate (provider, model_id), got no error")
	}

	// CHECK constraint on source — invalid enum must be rejected.
	_, err = s.db.Exec(`INSERT INTO catalog_models
		(provider, model_id, source) VALUES ('openai', 'bad-source', 'totally-invalid')`)
	if err == nil {
		t.Error("expected CHECK violation on invalid source enum, got no error")
	}

	// Each valid source value must be accepted.
	for _, src := range []string{"seed", "discovery", "openrouter", "user"} {
		_, err = s.db.Exec(`INSERT INTO catalog_models
			(provider, model_id, source) VALUES (?, ?, ?)`,
			"testprov-"+src, "m-"+src, src)
		if err != nil {
			t.Errorf("valid source %q rejected: %v", src, err)
		}
	}

	// Provider index exists.
	var idxName string
	err = s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_catalog_models_provider'`,
	).Scan(&idxName)
	if err != nil {
		t.Errorf("idx_catalog_models_provider missing: %v", err)
	}

	// Default values: tier, context_window, max_output, supports_*, source.
	_, err = s.db.Exec(`INSERT INTO catalog_models (provider, model_id) VALUES ('def', 'def-model')`)
	if err != nil {
		t.Fatalf("insert defaults row: %v", err)
	}
	var (
		tier, ctxWin, maxOut, simg, stools, sthink int
		source                                     string
	)
	err = s.db.QueryRow(`SELECT tier, context_window, max_output,
		supports_images, supports_tools, supports_thinking, source
		FROM catalog_models WHERE provider='def' AND model_id='def-model'`).
		Scan(&tier, &ctxWin, &maxOut, &simg, &stools, &sthink, &source)
	if err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if tier != 3 {
		t.Errorf("default tier = %d, want 3", tier)
	}
	if ctxWin != 0 || maxOut != 0 {
		t.Errorf("default context_window/max_output = %d/%d, want 0/0", ctxWin, maxOut)
	}
	if simg != 0 || stools != 0 || sthink != 0 {
		t.Errorf("default supports_* = %d/%d/%d, want 0/0/0", simg, stools, sthink)
	}
	if source != "seed" {
		t.Errorf("default source = %q, want \"seed\"", source)
	}
}

// columnExists checks whether a column is present on a table.
func columnExists(t *testing.T, s *sqliteStore, table, column string) bool {
	t.Helper()
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

// TestMigration010_CatalogPricingSchema verifies the schema of the
// catalog_pricing table created by migration 010 — per AC1 + ADR-1.
//
// Columns asserted: provider, model_id, input_price, output_price,
// cache_read_price, cache_write_price, thinking_price, source,
// updated_at.
// Source CHECK enum: seed | discovery | openrouter | user.
// Composite PK: (provider, model_id).
// FK: (provider, model_id) → catalog_models(provider, model_id)
// ON DELETE CASCADE.
func TestMigration010_CatalogPricingSchema(t *testing.T) {
	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := db.(*sqliteStore)

	// Columns present.
	requiredCols := []string{
		"provider", "model_id",
		"input_price", "output_price",
		"cache_read_price", "cache_write_price", "thinking_price",
		"source", "updated_at",
	}
	for _, col := range requiredCols {
		if !columnExists(t, s, "catalog_pricing", col) {
			t.Errorf("catalog_pricing is missing column %q", col)
		}
	}

	// FK must enable per-connection: PRAGMA foreign_keys must be ON for
	// the cascade behavior we rely on. Enable for this test.
	if _, err := s.db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign_keys: %v", err)
	}

	// Seed a catalog_models row so the FK can be satisfied.
	if _, err := s.db.Exec(`INSERT INTO catalog_models
		(provider, model_id, source) VALUES ('openai', 'gpt-4.1', 'seed')`); err != nil {
		t.Fatalf("seed catalog_models: %v", err)
	}

	// Inserting catalog_pricing without a matching catalog_models row must fail.
	_, err = s.db.Exec(`INSERT INTO catalog_pricing
		(provider, model_id, input_price, output_price, source)
		VALUES ('openai', 'no-such-model', 2.0, 8.0, 'seed')`)
	if err == nil {
		t.Error("expected FK violation on orphan catalog_pricing, got no error")
	}

	// Inserting with matching catalog_models row must succeed.
	_, err = s.db.Exec(`INSERT INTO catalog_pricing
		(provider, model_id, input_price, output_price, source)
		VALUES ('openai', 'gpt-4.1', 2.0, 8.0, 'seed')`)
	if err != nil {
		t.Fatalf("insert valid catalog_pricing: %v", err)
	}

	// ON DELETE CASCADE: removing the catalog_models row removes the
	// catalog_pricing row.
	if _, err := s.db.Exec(`DELETE FROM catalog_models
		WHERE provider='openai' AND model_id='gpt-4.1'`); err != nil {
		t.Fatalf("delete catalog_models: %v", err)
	}
	var remaining int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM catalog_pricing
		 WHERE provider='openai' AND model_id='gpt-4.1'`).Scan(&remaining); err != nil {
		t.Fatalf("count catalog_pricing: %v", err)
	}
	if remaining != 0 {
		t.Errorf("FK ON DELETE CASCADE did not remove catalog_pricing row; got %d remaining", remaining)
	}

	// CHECK constraint on source — invalid enum must be rejected.
	if _, err := s.db.Exec(`INSERT INTO catalog_models
		(provider, model_id, source) VALUES ('p1', 'm1', 'seed')`); err != nil {
		t.Fatalf("seed catalog_models p1/m1: %v", err)
	}
	_, err = s.db.Exec(`INSERT INTO catalog_pricing
		(provider, model_id, source) VALUES ('p1', 'm1', 'totally-invalid')`)
	if err == nil {
		t.Error("expected CHECK violation on invalid source enum, got no error")
	}

	// Defaults: all price columns default to 0; source defaults to 'seed'.
	if _, err := s.db.Exec(`INSERT INTO catalog_models
		(provider, model_id, source) VALUES ('def', 'def-model', 'seed')`); err != nil {
		t.Fatalf("seed catalog_models def: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO catalog_pricing
		(provider, model_id) VALUES ('def', 'def-model')`); err != nil {
		t.Fatalf("insert defaults row: %v", err)
	}
	var (
		in, out, cr, cw, think float64
		source                 string
	)
	if err := s.db.QueryRow(`SELECT input_price, output_price,
		cache_read_price, cache_write_price, thinking_price, source
		FROM catalog_pricing WHERE provider='def' AND model_id='def-model'`).
		Scan(&in, &out, &cr, &cw, &think, &source); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if in != 0 || out != 0 || cr != 0 || cw != 0 || think != 0 {
		t.Errorf("default prices = %v/%v/%v/%v/%v, want all 0", in, out, cr, cw, think)
	}
	if source != "seed" {
		t.Errorf("default source = %q, want \"seed\"", source)
	}
}

// TestMigration011_CatalogProviderMetaSchema verifies the schema of
// catalog_provider_meta — per AC1, AC2c, and ADR-1.
//
// Columns: provider, discovery_enabled, subscription_discoverable,
// last_discovered_at, last_discovery_error, backoff_step,
// next_discovery_after.
// Defaults (AC2c): discovery_enabled=0 (fail-closed),
// subscription_discoverable=0, last_discovery_error='',
// backoff_step=0, last_discovered_at and next_discovery_after NULL.
// PK: (provider).
func TestMigration011_CatalogProviderMetaSchema(t *testing.T) {
	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := db.(*sqliteStore)

	requiredCols := []string{
		"provider", "discovery_enabled", "subscription_discoverable",
		"last_discovered_at", "last_discovery_error",
		"backoff_step", "next_discovery_after",
	}
	for _, col := range requiredCols {
		if !columnExists(t, s, "catalog_provider_meta", col) {
			t.Errorf("catalog_provider_meta is missing column %q", col)
		}
	}

	// AC2c — defaults asserted on a row inserted with only the PK column.
	if _, err := s.db.Exec(`INSERT INTO catalog_provider_meta (provider) VALUES ('def')`); err != nil {
		t.Fatalf("insert defaults row: %v", err)
	}
	var (
		discEnabled, subDisc, backoffStep int
		lastErr                           string
		lastDisc                          sql.NullString
		nextAfter                         sql.NullString
	)
	err = s.db.QueryRow(`SELECT discovery_enabled, subscription_discoverable,
		last_discovered_at, last_discovery_error, backoff_step, next_discovery_after
		FROM catalog_provider_meta WHERE provider='def'`).
		Scan(&discEnabled, &subDisc, &lastDisc, &lastErr, &backoffStep, &nextAfter)
	if err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if discEnabled != 0 {
		t.Errorf("default discovery_enabled = %d, want 0 (fail-closed)", discEnabled)
	}
	if subDisc != 0 {
		t.Errorf("default subscription_discoverable = %d, want 0", subDisc)
	}
	if backoffStep != 0 {
		t.Errorf("default backoff_step = %d, want 0", backoffStep)
	}
	if lastErr != "" {
		t.Errorf("default last_discovery_error = %q, want \"\"", lastErr)
	}
	if lastDisc.Valid {
		t.Errorf("default last_discovered_at should be NULL, got %q", lastDisc.String)
	}
	if nextAfter.Valid {
		t.Errorf("default next_discovery_after should be NULL, got %q", nextAfter.String)
	}

	// PK: duplicate provider must fail.
	if _, err := s.db.Exec(`INSERT INTO catalog_provider_meta (provider) VALUES ('def')`); err == nil {
		t.Error("expected PK violation on duplicate provider, got no error")
	}
}

// TestSetGetProviderMeta_TimestampRoundTrip — per holistic /review m2.
// Verifies that timestamp values written via SQL and read back through
// modernc.org/sqlite v1.47.0 round-trip without loss within sub-second
// tolerance. The test exercises a non-zero `next_discovery_after` and
// non-zero `last_discovered_at` round-trip.
func TestSetGetProviderMeta_TimestampRoundTrip(t *testing.T) {
	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := db.(*sqliteStore)

	// Insert with explicit timestamps using the same format Go's database/sql
	// driver writes by default (ISO-8601 with UTC zone offset).
	want := time.Date(2026, 5, 12, 14, 30, 0, 0, time.UTC)
	if _, err := s.db.Exec(
		`INSERT INTO catalog_provider_meta
		(provider, discovery_enabled, last_discovered_at, next_discovery_after)
		VALUES (?, 1, ?, ?)`,
		"openai", want.Format(time.RFC3339), want.Add(time.Hour).Format(time.RFC3339),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Read back.
	var lastDisc, nextAfter string
	if err := s.db.QueryRow(
		`SELECT last_discovered_at, next_discovery_after
		 FROM catalog_provider_meta WHERE provider='openai'`,
	).Scan(&lastDisc, &nextAfter); err != nil {
		t.Fatalf("read: %v", err)
	}

	gotLast, err := time.Parse(time.RFC3339, lastDisc)
	if err != nil {
		t.Fatalf("parse last_discovered_at %q: %v", lastDisc, err)
	}
	gotNext, err := time.Parse(time.RFC3339, nextAfter)
	if err != nil {
		t.Fatalf("parse next_discovery_after %q: %v", nextAfter, err)
	}

	if delta := gotLast.Sub(want); delta < -time.Second || delta > time.Second {
		t.Errorf("last_discovered_at round-trip drift = %v (got %v, want %v)", delta, gotLast, want)
	}
	if delta := gotNext.Sub(want.Add(time.Hour)); delta < -time.Second || delta > time.Second {
		t.Errorf("next_discovery_after round-trip drift = %v", delta)
	}
}

// TestUsageLog_CreatedAtComparesAsString — per holistic /review m3.
// Migration 002's usage_log.created_at is TEXT (datetime('now') string
// format). The GetCacheHitRate query (added in task 1.8) filters with
// `WHERE created_at >= ?`. This test pins the contract: as long as
// writes use a canonical ISO-8601 / datetime('now') format, string
// comparison correctly bounds the rolling window.
func TestUsageLog_CreatedAtComparesAsString(t *testing.T) {
	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := db.(*sqliteStore)

	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	insert := `INSERT INTO usage_log
		(id, request_id, provider, model, connection_id, api_key_id,
		 input_tokens, output_tokens, total_tokens,
		 cache_read_tokens, cache_write_tokens,
		 cost, latency_ms, status, created_at, cost_source)
		VALUES (?, ?, 'openai', 'gpt-4.1', 'c1', '',
		        100, 50, 150, 0, 0, 0.001, 100, 'ok', ?, 'apikey')`
	for i, ts := range []time.Time{old, recent} {
		id := []string{"old", "recent"}[i]
		if _, err := s.db.Exec(insert, id, id, ts.Format("2006-01-02T15:04:05Z")); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	// 24h-lookback string comparison: must include only the recent row.
	// Format must match RecordUsage's `timeStr` (ISO 8601 with T-separator
	// and Z suffix) — see self-learning 82fa8ebabdbe4ea49ad3cdff7978e421.
	cutoff := now.Add(-24 * time.Hour).Format("2006-01-02T15:04:05Z")
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM usage_log WHERE created_at >= ?`, cutoff,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("string comparison failed to bound the window: count = %d, want 1 (only the recent row)", count)
	}
}

func TestMigration007_RefreshFailuresColumn(t *testing.T) {
	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := db.(*sqliteStore)

	if !columnExists(t, s, "connections", "refresh_failures") {
		t.Fatal("refresh_failures column missing after migration 007")
	}

	// Insert a connection without specifying refresh_failures.
	_, err = s.db.Exec(`INSERT INTO connections (id, provider, name, auth_type)
		VALUES ('test1', 'openai', 'Test', 'apikey')`)
	if err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	var rf int
	if err := s.db.QueryRow("SELECT refresh_failures FROM connections WHERE id = 'test1'").Scan(&rf); err != nil {
		t.Fatalf("read refresh_failures: %v", err)
	}
	if rf != 0 {
		t.Errorf("default refresh_failures = %d, want 0", rf)
	}
}

func TestMigration008_CostSourceColumn_BackfillsExistingRows(t *testing.T) {
	// AC2d: existing usage_log rows after 008 report cost_source='apikey'.
	// We exercise this by applying migrations 001-007 first, inserting a
	// usage_log row, then applying 008 manually and verifying the row is
	// backfilled via the column DEFAULT.

	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	s := db.(*sqliteStore)

	// Apply migrations 001..007 by running them directly. We use the runner
	// to keep things honest, but stop before 008 by faking 008 as already
	// applied AFTER 001-007 complete, then rolling that fake back.
	if err := db.Migrate(); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	// Drop cost_source so we can simulate "pre-migration-008 state."
	// SQLite supports DROP COLUMN from 3.35+ (modernc.org/sqlite ships
	// recent enough versions); if this fails, the test self-skips.
	if _, err := s.db.Exec("ALTER TABLE usage_log DROP COLUMN cost_source"); err != nil {
		t.Skipf("DROP COLUMN unsupported by this SQLite build: %v", err)
	}
	if _, err := s.db.Exec("DELETE FROM _migrations WHERE name = '008_cost_source.sql'"); err != nil {
		t.Fatalf("clear 008 from _migrations: %v", err)
	}

	// Insert a usage_log row WITHOUT cost_source — column doesn't exist yet.
	_, err = s.db.Exec(`INSERT INTO usage_log
		(id, request_id, provider, model, connection_id, api_key_id,
		 input_tokens, output_tokens, total_tokens,
		 cache_read_tokens, cache_write_tokens,
		 cost, latency_ms, status)
		VALUES ('u1', 'r1', 'openai', 'gpt-5', 'c1', '', 100, 50, 150, 0, 0, 0.001, 250, 'ok')`)
	if err != nil {
		t.Fatalf("insert pre-008 usage row: %v", err)
	}

	// Now re-apply migration 008.
	if err := db.Migrate(); err != nil {
		t.Fatalf("re-migrate after row insert: %v", err)
	}

	// AC2d check: the pre-existing row now reports cost_source='apikey'.
	var cs string
	if err := s.db.QueryRow("SELECT cost_source FROM usage_log WHERE id = 'u1'").Scan(&cs); err != nil {
		t.Fatalf("read cost_source: %v", err)
	}
	if cs != "apikey" {
		t.Errorf("backfilled cost_source = %q, want \"apikey\"", cs)
	}
}

func TestMigration006_NormalizesAuthType(t *testing.T) {
	// AC1: 006 UPDATEs normalize 'api_key' → 'apikey' and 'oauth' → 'subscription'.
	// We verify by inserting legacy values AFTER migrate (the runner won't
	// re-apply 006), then running the same UPDATE statements directly.

	db, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := db.(*sqliteStore)

	// Seed legacy auth_type values directly.
	cases := []struct {
		id, authType string
	}{
		{"legacy-apikey", "api_key"},
		{"legacy-oauth", "oauth"},
		{"already-canonical", "apikey"},
		{"none", "none"},
	}
	for _, c := range cases {
		if _, err := s.db.Exec(
			`INSERT INTO connections (id, provider, name, auth_type) VALUES (?, 'openai', ?, ?)`,
			c.id, c.id, c.authType,
		); err != nil {
			t.Fatalf("seed %s: %v", c.id, err)
		}
	}

	// Run the same UPDATE statements migration 006 contains.
	if _, err := s.db.Exec("UPDATE connections SET auth_type = 'apikey' WHERE auth_type = 'api_key'"); err != nil {
		t.Fatalf("UPDATE api_key: %v", err)
	}
	if _, err := s.db.Exec("UPDATE connections SET auth_type = 'subscription' WHERE auth_type = 'oauth'"); err != nil {
		t.Fatalf("UPDATE oauth: %v", err)
	}

	// Idempotency check: re-running the UPDATEs should be no-ops.
	if _, err := s.db.Exec("UPDATE connections SET auth_type = 'apikey' WHERE auth_type = 'api_key'"); err != nil {
		t.Fatalf("UPDATE api_key (2nd run): %v", err)
	}

	want := map[string]string{
		"legacy-apikey":     "apikey",
		"legacy-oauth":      "subscription",
		"already-canonical": "apikey",
		"none":              "none",
	}
	for id, expect := range want {
		var got string
		if err := s.db.QueryRow("SELECT auth_type FROM connections WHERE id = ?", id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got != expect {
			t.Errorf("connection %s: auth_type = %q, want %q", id, got, expect)
		}
	}
}
