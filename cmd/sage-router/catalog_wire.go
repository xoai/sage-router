package main

import (
	"context"
	"database/sql"
	"fmt"

	"sage-router/internal/catalog"
)

// CatalogWiring bundles the three catalog handles the server depends
// on so main.go can wire them all from one call:
//
//   - Registry: read-side fast path with in-memory cache (M1 surface)
//   - Store: raw store for writes (M2 — handleCreateConnection +
//     pricing override endpoints in M2.10)
//   - Discovery: dispatcher used by the on-connection-create hook
//     (M2.4) and the 24h background ticker (M2.5)
type CatalogWiring struct {
	Registry  catalog.Registry
	Store     catalog.Store
	Discovery *catalog.DiscoveryRunner
}

// wireCatalog constructs the catalog Store + Registry + DiscoveryRunner
// against the same *sql.DB the production store uses, seeds the static
// config constants into the catalog tables, and ensures the
// `openrouter_refresh_enabled` setting row exists.
//
// Idempotent: re-running on a populated DB is a no-op semantically
// (per AC3) — preserves runtime state (backoff_step, last_discovered_at).
//
// Models Discovery M1.11 (Registry) + M2.3/2.4 (DiscoveryRunner with
// BuiltinListers) — invoked from main.go after db.Migrate() and
// before server.New().
func wireCatalog(ctx context.Context, db *sql.DB) (*CatalogWiring, error) {
	cs := catalog.NewSQLiteStore(db)

	if err := catalog.SeedFromConstants(ctx, cs); err != nil {
		return nil, fmt.Errorf("seed catalog from constants: %w", err)
	}
	if err := catalog.SeedProviderMeta(ctx, cs); err != nil {
		return nil, fmt.Errorf("seed catalog provider meta: %w", err)
	}

	// Pm2 fix (plan-review): ensure the OpenRouter-refresh setting
	// row exists. M2's OpenRouterRefresher reads this row; without it,
	// the M2 background goroutine won't know whether to spawn. INSERT
	// is idempotent — ON CONFLICT preserves any user toggle from a
	// prior boot.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		"openrouter_refresh_enabled", "true",
	); err != nil {
		return nil, fmt.Errorf("seed openrouter_refresh_enabled setting: %w", err)
	}

	return &CatalogWiring{
		Registry:  catalog.NewRegistry(cs),
		Store:     cs,
		Discovery: catalog.NewDiscoveryRunner(cs, catalog.BuiltinListers()),
	}, nil
}
