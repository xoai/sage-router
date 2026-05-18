package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"sage-router/internal/catalog"
)

// selfHealKey gates the one-shot self-heal of stale URL-doubling
// discovery errors. When the row in `settings` is "true", the
// self-heal has already run on this DB and won't repeat. See
// initiative 20260514-discovery-url-doubling.
//
// Versioning convention: future one-shot self-heals should use
// `_v2_done`, `_v3_done`, etc. — never reuse `_v1_done`, even if the
// underlying bug class is similar. Each version gates an independent
// scan with its own match criteria; rows written by `_v1_done` should
// not be interpreted by future versions.
const selfHealKey = "discovery_404_self_heal_v1_done"

// selfHealKeyV2 gates the one-shot self-heal of stale 403-scope-mismatch
// errors. When ChatGPT subscription users tried discovery against
// api.openai.com/v1/models, the call returned 403 with
// "Missing scopes: api.model.read". After fix 20260514-openrouter-fallback,
// discovery routes subscription openai through OpenRouter and the 403
// goes away — but the stale row remains in catalog_provider_meta.
// This v2 self-heal clears that one specific row.
const selfHealKeyV2 = "discovery_403_self_heal_v2_done"

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

	// One-shot self-heal for the URL-doubling regression
	// (20260514-discovery-url-doubling). Pre-fix listers appended a
	// duplicate version segment producing /v1/v1/models → 404. Any DB
	// that ran a pre-fix binary has stale catalog_provider_meta rows
	// with backoff_step > 0 and a "HTTP 404:" error. Clear them once
	// so the UI badge disappears and discovery re-arms.
	if err := selfHealStaleDiscoveryErrors(ctx, db); err != nil {
		// Non-fatal: log + continue. The badge will clear on the next
		// successful discovery sweep regardless (~5 min after restart
		// via StartBackgroundRefresh + defaultSettleDelay).
		slog.Warn("self-heal stale discovery errors failed; continuing", "err", err)
	}

	// One-shot self-heal v2 for the OpenAI subscription scope-mismatch
	// (20260514-openrouter-fallback). After v1 fixed the URL, ChatGPT
	// subscription discovery 403'd with "Missing scopes: api.model.read".
	// This fix routes subscription openai through OpenRouter, but the
	// stale 403 row needs clearing.
	if err := selfHealStaleScopeErrors(ctx, db); err != nil {
		slog.Warn("self-heal v2 (scope errors) failed; continuing", "err", err)
	}

	return &CatalogWiring{
		Registry:  catalog.NewRegistry(cs),
		Store:     cs,
		Discovery: catalog.NewDiscoveryRunner(cs, catalog.BuiltinListers()),
	}, nil
}

// selfHealStaleDiscoveryErrors clears catalog_provider_meta rows whose
// last_discovery_error came from the URL-doubling regression
// (20260514-discovery-url-doubling). Gated by a one-shot settings row
// so future legitimate 404s are NOT auto-cleared.
//
// Narrow match: error text MUST contain "HTTP 404:" — the exact
// pre-fix signature from listerStatusError at internal/catalog/listers.go:104
// ("lister %s: HTTP %d: %s"). Limited to the 5 affected providers
// (the only ones with a discovery lister today).
//
// `next_discovery_after = NULL` matches the nullTimestamp zero-value
// contract at internal/catalog/sqlite.go:445-450 (NOT literal 0,
// which would be ambiguous TEXT in the RFC3339 column).
//
// Idempotent on fresh installs: UPDATE matches 0 rows, gate INSERTs,
// subsequent boots short-circuit at the gate check.
func selfHealStaleDiscoveryErrors(ctx context.Context, db *sql.DB) error {
	// One-shot gate: if already run on this DB, skip.
	var done string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, selfHealKey,
	).Scan(&done)
	if err == nil && done == "true" {
		return nil // already self-healed
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("settings query: %w", err)
	}

	// Clear stale URL-doubling errors for all 5 affected providers.
	affected := []string{"openai", "anthropic", "openrouter", "gemini", "ollama"}
	placeholders := strings.Repeat("?,", len(affected))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(affected))
	for i, p := range affected {
		args[i] = p
	}
	res, err := db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE catalog_provider_meta
		SET last_discovery_error = '',
		    backoff_step = 0,
		    next_discovery_after = NULL
		WHERE provider IN (%s)
		  AND last_discovery_error LIKE '%%HTTP 404:%%'
	`, placeholders), args...)
	if err != nil {
		return fmt.Errorf("clear stale errors: %w", err)
	}
	cleared, _ := res.RowsAffected()
	if cleared > 0 {
		slog.Info("self-heal: cleared stale discovery 404 errors", "rows", cleared)
	}

	// Record one-shot completion.
	_, err = db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES(?, 'true')
		 ON CONFLICT(key) DO UPDATE SET value = 'true'`, selfHealKey,
	)
	if err != nil {
		return fmt.Errorf("set self-heal gate: %w", err)
	}
	return nil
}

// selfHealStaleScopeErrors clears the one catalog_provider_meta row
// (provider='openai') whose last_discovery_error came from the
// pre-OpenRouter-fallback 403-scope-mismatch. After the fix at
// 20260514-openrouter-fallback, discovery for subscription openai
// uses OpenRouter — the stale 403 row needs clearing so the UI badge
// disappears.
//
// Narrow match: error text MUST contain BOTH "HTTP 403:" AND
// "Missing scopes: api.model.read" — the literal scope-error string
// from OpenAI's response body. This prevents auto-clearing legit
// non-scope 403s (e.g., API key revoked) that may surface in the
// future. Limited to provider='openai' — anthropic + others use
// different upstream endpoints.
//
// Idempotent on fresh installs: UPDATE matches 0 rows, gate INSERTs,
// subsequent boots short-circuit at the gate check.
func selfHealStaleScopeErrors(ctx context.Context, db *sql.DB) error {
	// One-shot gate.
	var done string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, selfHealKeyV2,
	).Scan(&done)
	if err == nil && done == "true" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("settings query: %w", err)
	}

	res, err := db.ExecContext(ctx, `
		UPDATE catalog_provider_meta
		SET last_discovery_error = '',
		    backoff_step = 0,
		    next_discovery_after = NULL
		WHERE provider = 'openai'
		  AND last_discovery_error LIKE '%HTTP 403:%'
		  AND last_discovery_error LIKE '%Missing scopes: api.model.read%'
	`)
	if err != nil {
		return fmt.Errorf("clear stale scope errors: %w", err)
	}
	cleared, _ := res.RowsAffected()
	if cleared > 0 {
		slog.Info("self-heal v2: cleared stale subscription scope 403", "provider", "openai", "rows", cleared)
	}

	_, err = db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES(?, 'true')
		 ON CONFLICT(key) DO UPDATE SET value = 'true'`, selfHealKeyV2,
	)
	if err != nil {
		return fmt.Errorf("set self-heal v2 gate: %w", err)
	}
	return nil
}
