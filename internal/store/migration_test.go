package store

import (
	"testing"
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

	// Verify all migrations were recorded
	var migrationCount int
	s.db.QueryRow("SELECT COUNT(*) FROM _migrations").Scan(&migrationCount)
	if migrationCount != 5 {
		t.Errorf("expected 5 migrations recorded, got %d", migrationCount)
	}
}
