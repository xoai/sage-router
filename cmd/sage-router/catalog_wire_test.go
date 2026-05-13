package main

import (
	"context"
	"testing"

	"sage-router/internal/store"
)

// freshDB opens an in-memory SQLite store, runs migrations, and
// returns the Store (the catalog Wire helper consumes Store.DB()).
func freshDB(t *testing.T) store.Store {
	t.Helper()
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// TestWireCatalog_SeedsCatalogOnEmptyDB — Pm3 (plan-review m4) +
// M1.11 done criteria. wireCatalog populates catalog_models +
// catalog_pricing + catalog_provider_meta from the static config
// constants the first time it runs.
func TestWireCatalog_SeedsCatalogOnEmptyDB(t *testing.T) {
	st := freshDB(t)
	ctx := context.Background()

	reg, err := wireCatalog(ctx, st.DB())
	if err != nil {
		t.Fatalf("wireCatalog: %v", err)
	}
	if reg == nil {
		t.Fatal("wireCatalog returned nil registry")
	}

	// Verify catalog_models populated.
	var modelCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM catalog_models`).Scan(&modelCount); err != nil {
		t.Fatalf("count catalog_models: %v", err)
	}
	if modelCount == 0 {
		t.Error("catalog_models is empty after wireCatalog")
	}

	// Verify catalog_pricing populated.
	var pricingCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM catalog_pricing`).Scan(&pricingCount); err != nil {
		t.Fatalf("count catalog_pricing: %v", err)
	}
	if pricingCount == 0 {
		t.Error("catalog_pricing is empty after wireCatalog")
	}

	// Verify catalog_provider_meta populated (6 known providers).
	var metaCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM catalog_provider_meta`).Scan(&metaCount); err != nil {
		t.Fatalf("count catalog_provider_meta: %v", err)
	}
	if metaCount < 6 {
		t.Errorf("catalog_provider_meta has %d rows, want at least 6", metaCount)
	}

	// Verify openrouter_refresh_enabled setting written.
	var settingValue string
	err = st.DB().QueryRow(
		`SELECT value FROM settings WHERE key = ?`,
		"openrouter_refresh_enabled",
	).Scan(&settingValue)
	if err != nil {
		t.Fatalf("read openrouter_refresh_enabled: %v", err)
	}
	if settingValue != "true" {
		t.Errorf("openrouter_refresh_enabled = %q, want \"true\"", settingValue)
	}
}

// TestBootstrap_InsertsOpenRouterSettingIfMissing — discoverable alias
// for the assertion that wireCatalog inserts `openrouter_refresh_enabled`
// into the settings table. Delegated from M2.8 task done-criteria (the
// plan named this test by spec). The original assertion lives in
// TestWireCatalog_SeedsCatalogOnEmptyDB; this alias ensures a grep for
// "TestBootstrap_InsertsOpenRouterSettingIfMissing" finds it.
func TestBootstrap_InsertsOpenRouterSettingIfMissing(t *testing.T) {
	st := freshDB(t)
	ctx := context.Background()

	// Sanity-precondition: settings table starts empty for this key.
	var preCount int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM settings WHERE key = ?`,
		"openrouter_refresh_enabled",
	).Scan(&preCount); err != nil {
		t.Fatalf("count pre: %v", err)
	}
	if preCount != 0 {
		t.Fatalf("pre-wireCatalog: %d rows for openrouter_refresh_enabled, want 0", preCount)
	}

	if _, err := wireCatalog(ctx, st.DB()); err != nil {
		t.Fatalf("wireCatalog: %v", err)
	}

	var value string
	if err := st.DB().QueryRow(
		`SELECT value FROM settings WHERE key = ?`,
		"openrouter_refresh_enabled",
	).Scan(&value); err != nil {
		t.Fatalf("read setting: %v", err)
	}
	if value != "true" {
		t.Errorf("openrouter_refresh_enabled = %q, want %q", value, "true")
	}
}

// TestWireCatalog_SeedIsNoOpOnSecondRun — AC3 at the bootstrap layer.
// Running wireCatalog twice must not duplicate rows or clobber any
// runtime state set between calls.
func TestWireCatalog_SeedIsNoOpOnSecondRun(t *testing.T) {
	st := freshDB(t)
	ctx := context.Background()

	if _, err := wireCatalog(ctx, st.DB()); err != nil {
		t.Fatalf("wireCatalog 1: %v", err)
	}
	var before [3]int
	st.DB().QueryRow(`SELECT COUNT(*) FROM catalog_models`).Scan(&before[0])
	st.DB().QueryRow(`SELECT COUNT(*) FROM catalog_pricing`).Scan(&before[1])
	st.DB().QueryRow(`SELECT COUNT(*) FROM catalog_provider_meta`).Scan(&before[2])

	// Simulate a runtime write between bootstraps: bump the openai
	// row's backoff_step to 2. The second wireCatalog call must not
	// reset it (SeedProviderMeta is idempotent per M1.7).
	if _, err := st.DB().Exec(
		`UPDATE catalog_provider_meta SET backoff_step = 2 WHERE provider = 'openai'`,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	if _, err := wireCatalog(ctx, st.DB()); err != nil {
		t.Fatalf("wireCatalog 2: %v", err)
	}
	var after [3]int
	st.DB().QueryRow(`SELECT COUNT(*) FROM catalog_models`).Scan(&after[0])
	st.DB().QueryRow(`SELECT COUNT(*) FROM catalog_pricing`).Scan(&after[1])
	st.DB().QueryRow(`SELECT COUNT(*) FROM catalog_provider_meta`).Scan(&after[2])

	if before != after {
		t.Errorf("row counts drifted: before=%v after=%v", before, after)
	}

	// Runtime backoff_step value must survive the re-seed.
	var backoffStep int
	if err := st.DB().QueryRow(
		`SELECT backoff_step FROM catalog_provider_meta WHERE provider = 'openai'`,
	).Scan(&backoffStep); err != nil {
		t.Fatalf("read backoff_step: %v", err)
	}
	if backoffStep != 2 {
		t.Errorf("backoff_step clobbered by re-seed: got %d, want 2", backoffStep)
	}
}

// TestWireCatalog_OpenRouterSettingIsIdempotent — Pm2 contract. The
// settings INSERT must not clobber a user-toggled value from a prior
// boot. A user who set openrouter_refresh_enabled = 'false' must see
// 'false' preserved on subsequent restarts.
func TestWireCatalog_OpenRouterSettingIsIdempotent(t *testing.T) {
	st := freshDB(t)
	ctx := context.Background()

	// Simulate a prior boot's wireCatalog setting the row, then user
	// toggling it off.
	if _, err := wireCatalog(ctx, st.DB()); err != nil {
		t.Fatalf("wireCatalog 1: %v", err)
	}
	if _, err := st.DB().Exec(
		`UPDATE settings SET value = ? WHERE key = ?`,
		"false", "openrouter_refresh_enabled",
	); err != nil {
		t.Fatalf("update setting: %v", err)
	}

	// Re-wire (simulates next boot).
	if _, err := wireCatalog(ctx, st.DB()); err != nil {
		t.Fatalf("wireCatalog 2: %v", err)
	}

	var value string
	if err := st.DB().QueryRow(
		`SELECT value FROM settings WHERE key = ?`, "openrouter_refresh_enabled",
	).Scan(&value); err != nil {
		t.Fatalf("read setting: %v", err)
	}
	if value != "false" {
		t.Errorf("user-toggled setting was clobbered: got %q, want \"false\"", value)
	}
}
