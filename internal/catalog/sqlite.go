package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// sqliteCatalog implements Store against the same *sql.DB that
// internal/store.sqliteStore uses. The shared connection is needed
// for FK consistency between catalog_models and catalog_pricing
// (FK→009 with ON DELETE CASCADE) and to avoid double-opening
// SQLite. See ADR-1 + ADR-2 for the schema and precedence rules.
type sqliteCatalog struct {
	db *sql.DB
}

// NewSQLiteStore returns a catalog Store backed by the given DB.
// The caller is responsible for running migrations (via the
// production store.Store.Migrate). Callers MUST enable
// PRAGMA foreign_keys = ON at session start for the FK between
// catalog_pricing and catalog_models to enforce cascade behavior.
func NewSQLiteStore(db *sql.DB) Store {
	return &sqliteCatalog{db: db}
}

// timestampLayout is the canonical TEXT format used for all catalog
// timestamps. Matches modernc.org/sqlite's RFC3339 round-trip
// contract verified by TestSetGetProviderMeta_TimestampRoundTrip.
const timestampLayout = time.RFC3339

// pricingSourcePrecedenceWhere is the WHERE-clause body shared by
// UpsertPricing and BulkUpsertPricing. It enforces ADR-2 §Conflict
// resolution for catalog_pricing: user beats everything, seed loses
// to anything non-seed, and discovery cannot clobber openrouter (the
// extra rule that distinguishes pricing precedence from models
// precedence). Twelve-cell behavior is pinned by
// TestPrecedenceMatrix_Pricing in precedence_matrix_test.go.
//
// UpsertModel uses a similar but shorter WHERE (no openrouter rule);
// it is the only catalog_models writer, so the fragment stays inline
// at the call site rather than warranting its own constant.
const pricingSourcePrecedenceWhere = `
	NOT (catalog_pricing.source = 'user' AND excluded.source != 'user')
	AND NOT (catalog_pricing.source != 'seed' AND excluded.source = 'seed')
	AND NOT (catalog_pricing.source = 'openrouter' AND excluded.source = 'discovery')`

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// UpsertModel implements the source-precedence rules from ADR-2
// §Conflict resolution for the catalog_models table:
//
//   - user can be overwritten only by user
//   - seed cannot overwrite anything that isn't itself seed
//   - all other transitions (e.g., discovery → discovery, openrouter →
//     discovery, seed → discovery) overwrite
//
// The rules are enforced in the ON CONFLICT WHERE clause, NOT in Go,
// so concurrent writes from different goroutines retain correctness.
func (s *sqliteCatalog) UpsertModel(ctx context.Context, m Model) error {
	if !IsValidSource(m.Source) {
		return fmt.Errorf("catalog: invalid source %q", m.Source)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO catalog_models
			(provider, model_id, display_name, tier, context_window, max_output,
			 supports_images, supports_tools, supports_thinking, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, model_id) DO UPDATE SET
			display_name      = excluded.display_name,
			tier              = excluded.tier,
			context_window    = excluded.context_window,
			max_output        = excluded.max_output,
			supports_images   = excluded.supports_images,
			supports_tools    = excluded.supports_tools,
			supports_thinking = excluded.supports_thinking,
			source            = excluded.source,
			updated_at        = CURRENT_TIMESTAMP
		WHERE
			NOT (catalog_models.source = 'user' AND excluded.source != 'user')
			AND NOT (catalog_models.source != 'seed' AND excluded.source = 'seed')
	`,
		m.Provider, m.ModelID, m.DisplayName, m.Tier, m.ContextWindow, m.MaxOutput,
		boolToInt(m.Caps.SupportsImages), boolToInt(m.Caps.SupportsTools), boolToInt(m.Caps.SupportsThinking),
		m.Source,
	)
	if err != nil {
		return fmt.Errorf("catalog: upsert model %s/%s: %w", m.Provider, m.ModelID, err)
	}
	return nil
}

// GetModel reads one model row joined with its pricing row. Returns
// nil, nil when no row exists. Pricing fields are zero when no
// pricing row exists yet (e.g., immediately after a discovery write
// that didn't include pricing).
func (s *sqliteCatalog) GetModel(ctx context.Context, provider, modelID string) (*Model, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT m.provider, m.model_id, m.display_name, m.tier,
		       m.context_window, m.max_output,
		       m.supports_images, m.supports_tools, m.supports_thinking,
		       m.source, m.discovered_at, m.updated_at,
		       COALESCE(p.input_price, 0), COALESCE(p.output_price, 0),
		       COALESCE(p.cache_read_price, 0), COALESCE(p.cache_write_price, 0),
		       COALESCE(p.thinking_price, 0), COALESCE(p.source, '')
		FROM catalog_models m
		LEFT JOIN catalog_pricing p
		  ON p.provider = m.provider AND p.model_id = m.model_id
		WHERE m.provider = ? AND m.model_id = ?
	`, provider, modelID)
	m, err := scanModelRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("catalog: get model %s/%s: %w", provider, modelID, err)
	}
	return m, nil
}

// ListModels returns models ordered by (provider, model_id) ASC.
// AC11b: this deterministic ordering keeps the 1.9-baseline golden
// snapshot stable across runs (Go map iteration is randomized).
func (s *sqliteCatalog) ListModels(ctx context.Context, provider string) ([]Model, error) {
	q := `
		SELECT m.provider, m.model_id, m.display_name, m.tier,
		       m.context_window, m.max_output,
		       m.supports_images, m.supports_tools, m.supports_thinking,
		       m.source, m.discovered_at, m.updated_at,
		       COALESCE(p.input_price, 0), COALESCE(p.output_price, 0),
		       COALESCE(p.cache_read_price, 0), COALESCE(p.cache_write_price, 0),
		       COALESCE(p.thinking_price, 0), COALESCE(p.source, '')
		FROM catalog_models m
		LEFT JOIN catalog_pricing p
		  ON p.provider = m.provider AND p.model_id = m.model_id`
	var args []any
	if provider != "" {
		q += " WHERE m.provider = ?"
		args = append(args, provider)
	}
	q += " ORDER BY m.provider, m.model_id"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: list models: %w", err)
	}
	defer rows.Close()

	var out []Model
	for rows.Next() {
		m, err := scanModelRow(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog: scan model: %w", err)
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate models: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Pricing
// ---------------------------------------------------------------------------

// UpsertPricing enforces ADR-2 §Conflict resolution rules for the
// catalog_pricing table. Beyond the model-table rules (user wins,
// seed loses to non-seed), pricing has one extra rule: **discovery
// may NOT clobber openrouter pricing.** OpenRouter is a richer
// pricing oracle than per-provider discovery; once an openrouter row
// exists, only openrouter or user may overwrite it.
func (s *sqliteCatalog) UpsertPricing(ctx context.Context, provider, modelID string, p Pricing) error {
	if !IsValidSource(p.Source) {
		return fmt.Errorf("catalog: invalid pricing source %q", p.Source)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO catalog_pricing
			(provider, model_id, input_price, output_price,
			 cache_read_price, cache_write_price, thinking_price, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, model_id) DO UPDATE SET
			input_price       = excluded.input_price,
			output_price      = excluded.output_price,
			cache_read_price  = excluded.cache_read_price,
			cache_write_price = excluded.cache_write_price,
			thinking_price    = excluded.thinking_price,
			source            = excluded.source,
			updated_at        = CURRENT_TIMESTAMP
		WHERE`+pricingSourcePrecedenceWhere,
		provider, modelID,
		p.Input, p.Output, p.CacheRead, p.CacheWrite, p.Thinking, p.Source,
	)
	if err != nil {
		return fmt.Errorf("catalog: upsert pricing %s/%s: %w", provider, modelID, err)
	}
	return nil
}

// GetPricing returns just the pricing row for (provider, modelID).
// Returns nil, nil when no row exists.
func (s *sqliteCatalog) GetPricing(ctx context.Context, provider, modelID string) (*Pricing, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT input_price, output_price,
		       cache_read_price, cache_write_price, thinking_price, source
		FROM catalog_pricing
		WHERE provider = ? AND model_id = ?
	`, provider, modelID)
	var p Pricing
	err := row.Scan(&p.Input, &p.Output, &p.CacheRead, &p.CacheWrite, &p.Thinking, &p.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("catalog: get pricing %s/%s: %w", provider, modelID, err)
	}
	return &p, nil
}

// DeletePricing removes the catalog_pricing row for (provider, modelID).
// Idempotent: nil error when no row exists. Does NOT touch
// catalog_models — the model row stays so subsequent discovery /
// OpenRouter refresh can re-create pricing without an FK violation.
// (Schema's FK is catalog_pricing → catalog_models ON DELETE CASCADE,
// not the reverse, so deleting pricing alone is safe.)
func (s *sqliteCatalog) DeletePricing(ctx context.Context, provider, modelID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM catalog_pricing WHERE provider = ? AND model_id = ?`,
		provider, modelID,
	)
	if err != nil {
		return fmt.Errorf("catalog: delete pricing %s/%s: %w", provider, modelID, err)
	}
	return nil
}

// BulkUpsertPricing applies many pricing updates in one transaction
// for write amortization (used by the OpenRouter refresher in M2
// and the seeder in M1.7). Each row goes through the same WHERE-
// clause precedence as UpsertPricing.
func (s *sqliteCatalog) BulkUpsertPricing(ctx context.Context, updates []PricingUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalog: begin bulk pricing tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO catalog_pricing
			(provider, model_id, input_price, output_price,
			 cache_read_price, cache_write_price, thinking_price, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, model_id) DO UPDATE SET
			input_price       = excluded.input_price,
			output_price      = excluded.output_price,
			cache_read_price  = excluded.cache_read_price,
			cache_write_price = excluded.cache_write_price,
			thinking_price    = excluded.thinking_price,
			source            = excluded.source,
			updated_at        = CURRENT_TIMESTAMP
		WHERE`+pricingSourcePrecedenceWhere)
	if err != nil {
		return fmt.Errorf("catalog: prepare bulk pricing: %w", err)
	}
	defer stmt.Close()

	for _, u := range updates {
		if !IsValidSource(u.Pricing.Source) {
			return fmt.Errorf("catalog: invalid pricing source %q in bulk update", u.Pricing.Source)
		}
		_, err := stmt.ExecContext(ctx,
			u.Provider, u.ModelID,
			u.Pricing.Input, u.Pricing.Output,
			u.Pricing.CacheRead, u.Pricing.CacheWrite, u.Pricing.Thinking,
			u.Pricing.Source,
		)
		if err != nil {
			return fmt.Errorf("catalog: bulk upsert %s/%s: %w", u.Provider, u.ModelID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: commit bulk pricing: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Provider meta
// ---------------------------------------------------------------------------

// SetProviderMeta upserts the catalog_provider_meta row. Used by the
// seeder (M1.7) and the discovery loop's recordResult (M2). No
// source-precedence semantics — operator/seeder/loop writes are all
// equally authoritative; the table is mutable state, not data.
func (s *sqliteCatalog) SetProviderMeta(ctx context.Context, provider string, m ProviderMeta) error {
	lastDiscovered := nullTimestamp(m.LastDiscoveredAt)
	nextAfter := nullTimestamp(m.NextDiscoveryAfter)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO catalog_provider_meta
			(provider, discovery_enabled, subscription_discoverable,
			 last_discovered_at, last_discovery_error,
			 backoff_step, next_discovery_after)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider) DO UPDATE SET
			discovery_enabled         = excluded.discovery_enabled,
			subscription_discoverable = excluded.subscription_discoverable,
			last_discovered_at        = excluded.last_discovered_at,
			last_discovery_error      = excluded.last_discovery_error,
			backoff_step              = excluded.backoff_step,
			next_discovery_after      = excluded.next_discovery_after
	`,
		provider,
		boolToInt(m.DiscoveryEnabled), boolToInt(m.SubscriptionDiscoverable),
		lastDiscovered, m.LastDiscoveryError,
		m.BackoffStep, nextAfter,
	)
	if err != nil {
		return fmt.Errorf("catalog: set provider meta %s: %w", provider, err)
	}
	return nil
}

// GetProviderMeta returns the row for one provider, or nil, nil when
// no row exists. The discovery loop treats an absent row as
// "discovery disabled" (fail-closed) per ADR-2.
func (s *sqliteCatalog) GetProviderMeta(ctx context.Context, provider string) (*ProviderMeta, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT provider, discovery_enabled, subscription_discoverable,
		       last_discovered_at, last_discovery_error,
		       backoff_step, next_discovery_after
		FROM catalog_provider_meta
		WHERE provider = ?
	`, provider)
	pm, err := scanProviderMeta(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("catalog: get provider meta %s: %w", provider, err)
	}
	return pm, nil
}

// ListProviderMetas returns every row, ordered by provider ASC.
// Used by /api/providers to surface last_discovered_at and
// last_discovery_error per provider on the dashboard.
func (s *sqliteCatalog) ListProviderMetas(ctx context.Context) ([]ProviderMeta, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, discovery_enabled, subscription_discoverable,
		       last_discovered_at, last_discovery_error,
		       backoff_step, next_discovery_after
		FROM catalog_provider_meta
		ORDER BY provider
	`)
	if err != nil {
		return nil, fmt.Errorf("catalog: list provider metas: %w", err)
	}
	defer rows.Close()

	var out []ProviderMeta
	for rows.Next() {
		pm, err := scanProviderMeta(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog: scan provider meta: %w", err)
		}
		out = append(out, *pm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate provider metas: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type scanner interface {
	Scan(dest ...any) error
}

func scanModelRow(r scanner) (*Model, error) {
	var m Model
	var simg, stools, sthink int
	var discoveredAt, updatedAt string
	var pricingSource string
	if err := r.Scan(
		&m.Provider, &m.ModelID, &m.DisplayName, &m.Tier,
		&m.ContextWindow, &m.MaxOutput,
		&simg, &stools, &sthink,
		&m.Source, &discoveredAt, &updatedAt,
		&m.Pricing.Input, &m.Pricing.Output,
		&m.Pricing.CacheRead, &m.Pricing.CacheWrite, &m.Pricing.Thinking,
		&pricingSource,
	); err != nil {
		return nil, err
	}
	m.Caps.SupportsImages = simg != 0
	m.Caps.SupportsTools = stools != 0
	m.Caps.SupportsThinking = sthink != 0
	m.Pricing.Source = pricingSource
	m.DiscoveredAt = parseTimestamp(discoveredAt)
	m.UpdatedAt = parseTimestamp(updatedAt)
	return &m, nil
}

func scanProviderMeta(r scanner) (*ProviderMeta, error) {
	var pm ProviderMeta
	var disc, sub, backoffStep int
	var lastDisc, nextAfter sql.NullString
	if err := r.Scan(
		&pm.Provider, &disc, &sub,
		&lastDisc, &pm.LastDiscoveryError,
		&backoffStep, &nextAfter,
	); err != nil {
		return nil, err
	}
	pm.DiscoveryEnabled = disc != 0
	pm.SubscriptionDiscoverable = sub != 0
	pm.BackoffStep = backoffStep
	if lastDisc.Valid {
		pm.LastDiscoveredAt = parseTimestamp(lastDisc.String)
	}
	if nextAfter.Valid {
		pm.NextDiscoveryAfter = parseTimestamp(nextAfter.String)
	}
	return &pm, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullTimestamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timestampLayout)
}

// parseTimestamp tolerates both the RFC3339 format we write and the
// SQLite-default "YYYY-MM-DD HH:MM:SS" format that CURRENT_TIMESTAMP
// produces for migration-default columns (catalog_models.discovered_at,
// updated_at).
func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(s)); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// ListModelIDsForProviders returns all catalog_models.model_id values
// for the given provider keys, grouped by provider. Used by the
// OpenRouter refresher's mirror-pricing path (fix 20260514-pricing-mirror)
// to resolve OpenRouter IDs against existing direct-provider rows.
//
// Portable SQL: single-column IN with dynamic placeholders. Avoids the
// modernc.org/sqlite portability concerns of multi-column tuple-IN.
//
// Absent providers (e.g., querying for "ollama" when no ollama rows
// exist) yield an empty slice in the returned map — NOT an error.
// Callers can rely on `out[provider]` being non-nil for every provider
// they passed in.
func (s *sqliteCatalog) ListModelIDsForProviders(
	ctx context.Context, providers []string,
) (map[string][]string, error) {
	if len(providers) == 0 {
		return map[string][]string{}, nil
	}
	placeholders := strings.Repeat("?,", len(providers))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(providers))
	for i, p := range providers {
		args[i] = p
	}
	query := fmt.Sprintf(
		`SELECT provider, model_id FROM catalog_models WHERE provider IN (%s)`,
		placeholders,
	)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListModelIDsForProviders: query: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]string, len(providers))
	for rows.Next() {
		var p, m string
		if err := rows.Scan(&p, &m); err != nil {
			return nil, fmt.Errorf("ListModelIDsForProviders: scan: %w", err)
		}
		out[p] = append(out[p], m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListModelIDsForProviders: rows: %w", err)
	}
	// Ensure absent providers yield non-nil empty slices so callers
	// can use `out[p]` without a nil check.
	for _, p := range providers {
		if _, ok := out[p]; !ok {
			out[p] = []string{}
		}
	}
	return out, nil
}
