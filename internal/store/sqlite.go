package store

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// sqliteStore implements Store backed by a SQLite database.
type sqliteStore struct {
	db     *sql.DB
	encKey []byte // AES-256 key for encrypting secrets; nil = no encryption
}

// SetEncryptionKey enables AES-256-GCM encryption for connection secrets.
func (s *sqliteStore) SetEncryptionKey(key []byte) {
	s.encKey = key
}

// encryptField encrypts a value if an encryption key is set and the value is non-empty.
func (s *sqliteStore) encryptField(value string) string {
	if s.encKey == nil || value == "" {
		return value
	}
	encrypted, err := Encrypt(value, s.encKey)
	if err != nil {
		return value // fallback to plaintext on error
	}
	return encrypted
}

// decryptField decrypts a value if an encryption key is set. Falls back to
// returning the original value if decryption fails (handles pre-encryption plaintext).
func (s *sqliteStore) decryptField(value string) string {
	if s.encKey == nil || value == "" {
		return value
	}
	plain, err := Decrypt(value, s.encKey)
	if err != nil {
		return value // already plaintext or corrupted — return as-is
	}
	return plain
}

// NewSQLiteStore opens (or creates) a SQLite database at path and returns a
// Store. Call Migrate() after opening to ensure the schema is up to date.
func NewSQLiteStore(path string) (Store, error) {
	// For in-memory databases, use shared cache so all connections from the
	// pool see the same data. Without this, each pooled connection gets its
	// own empty database.
	dsn := path
	if path == ":memory:" {
		// Use a unique name per instance so parallel tests don't collide,
		// but share cache within a single connection pool.
		dsn = fmt.Sprintf("file:memdb_%d?mode=memory&cache=shared", time.Now().UnixNano())
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// Enable WAL mode and foreign keys.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %s: %w", pragma, err)
		}
	}

	return &sqliteStore{db: db}, nil
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// Migrate applies all embedded SQL migration files in order.
func (s *sqliteStore) Migrate() error {
	// Ensure the migrations meta-table exists.
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS _migrations (
		name       TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("create _migrations table: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	// Sort by filename to guarantee order.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, e := range entries {
		name := e.Name()

		// Check if already applied.
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM _migrations WHERE name = ?", name).Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if count > 0 {
			continue
		}

		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		if _, err := s.db.Exec(string(data)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}

		if _, err := s.db.Exec("INSERT INTO _migrations (name) VALUES (?)", name); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

const connCols = `id, provider, name, auth_type, access_token, refresh_token, api_key, priority, state, expires_at, provider_data, created_at, updated_at`

func scanConnection(row interface{ Scan(dest ...any) error }) (*Connection, error) {
	var c Connection
	var expiresAt sql.NullString
	var providerData sql.NullString
	var createdAt, updatedAt string

	err := row.Scan(
		&c.ID, &c.Provider, &c.Name, &c.AuthType,
		&c.AccessToken, &c.RefreshToken, &c.APIKey,
		&c.Priority, &c.State, &expiresAt, &providerData,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	if expiresAt.Valid && expiresAt.String != "" {
		t, err := time.Parse("2006-01-02T15:04:05Z", expiresAt.String)
		if err != nil {
			t, _ = time.Parse("2006-01-02 15:04:05", expiresAt.String)
		}
		c.ExpiresAt = &t
	}
	if providerData.Valid && providerData.String != "" {
		c.ProviderData = json.RawMessage(providerData.String)
	}

	c.CreatedAt = parseTime(createdAt)
	c.UpdatedAt = parseTime(updatedAt)
	return &c, nil
}

func (s *sqliteStore) ListConnections(filter ConnectionFilter) ([]Connection, error) {
	w := buildConnectionFilter(filter)
	q := "SELECT " + connCols + " FROM connections" + w.sql() + " ORDER BY priority ASC, name ASC"

	rows, err := s.db.Query(q, w.params...)
	if err != nil {
		return nil, fmt.Errorf("list connections: %w", err)
	}
	defer rows.Close()

	var out []Connection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, fmt.Errorf("scan connection: %w", err)
		}
		s.decryptConnectionSecrets(c)
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *sqliteStore) GetConnection(id string) (*Connection, error) {
	row := s.db.QueryRow("SELECT "+connCols+" FROM connections WHERE id = ?", id)
	c, err := scanConnection(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("connection %q not found", id)
	}
	if err == nil {
		s.decryptConnectionSecrets(c)
	}
	return c, err
}

// decryptConnectionSecrets decrypts the secret fields of a connection in-place.
func (s *sqliteStore) decryptConnectionSecrets(c *Connection) {
	c.AccessToken = s.decryptField(c.AccessToken)
	c.RefreshToken = s.decryptField(c.RefreshToken)
	c.APIKey = s.decryptField(c.APIKey)
}

func (s *sqliteStore) CreateConnection(c *Connection) error {
	now := timeStr(time.Now().UTC())
	var expiresAt *string
	if c.ExpiresAt != nil {
		s := timeStr(*c.ExpiresAt)
		expiresAt = &s
	}
	var providerData *string
	if c.ProviderData != nil {
		s := string(c.ProviderData)
		providerData = &s
	}

	_, err := s.db.Exec(`INSERT INTO connections (`+connCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Provider, c.Name, c.AuthType,
		s.encryptField(c.AccessToken), s.encryptField(c.RefreshToken), s.encryptField(c.APIKey),
		c.Priority, c.State, expiresAt, providerData,
		now, now,
	)
	if err != nil {
		return fmt.Errorf("create connection: %w", err)
	}
	c.CreatedAt = parseTime(now)
	c.UpdatedAt = c.CreatedAt
	return nil
}

func (s *sqliteStore) UpdateConnection(id string, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}

	// Always bump updated_at.
	updates["updated_at"] = timeStr(time.Now().UTC())

	// Encrypt secret fields if present.
	for _, secretCol := range []string{"access_token", "refresh_token", "api_key"} {
		if v, ok := updates[secretCol]; ok {
			if str, isStr := v.(string); isStr {
				updates[secretCol] = s.encryptField(str)
			}
		}
	}

	var setClauses []string
	var params []any
	for col, val := range updates {
		setClauses = append(setClauses, col+" = ?")
		params = append(params, val)
	}
	params = append(params, id)

	q := "UPDATE connections SET " + joinStrings(setClauses, ", ") + " WHERE id = ?"
	res, err := s.db.Exec(q, params...)
	if err != nil {
		return fmt.Errorf("update connection: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("connection %q not found", id)
	}
	return nil
}

func (s *sqliteStore) DeleteConnection(id string) error {
	res, err := s.db.Exec("DELETE FROM connections WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete connection: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("connection %q not found", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Combos
// ---------------------------------------------------------------------------

func (s *sqliteStore) ListCombos() ([]Combo, error) {
	rows, err := s.db.Query("SELECT id, name, models, created_at FROM combos ORDER BY name ASC")
	if err != nil {
		return nil, fmt.Errorf("list combos: %w", err)
	}
	defer rows.Close()

	var out []Combo
	for rows.Next() {
		c, err := scanCombo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *sqliteStore) GetComboByName(name string) (*Combo, error) {
	row := s.db.QueryRow("SELECT id, name, models, created_at FROM combos WHERE name = ?", name)
	c, err := scanCombo(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("combo %q not found", name)
	}
	return c, err
}

func (s *sqliteStore) CreateCombo(c *Combo) error {
	modelsJSON, err := json.Marshal(c.Models)
	if err != nil {
		return fmt.Errorf("marshal combo models: %w", err)
	}
	now := timeStr(time.Now().UTC())

	_, err = s.db.Exec("INSERT INTO combos (id, name, models, created_at) VALUES (?,?,?,?)",
		c.ID, c.Name, string(modelsJSON), now,
	)
	if err != nil {
		return fmt.Errorf("create combo: %w", err)
	}
	c.CreatedAt = parseTime(now)
	return nil
}

func (s *sqliteStore) UpdateCombo(id string, c *Combo) error {
	modelsJSON, err := json.Marshal(c.Models)
	if err != nil {
		return fmt.Errorf("marshal combo models: %w", err)
	}

	res, err := s.db.Exec("UPDATE combos SET name = ?, models = ? WHERE id = ?",
		c.Name, string(modelsJSON), id,
	)
	if err != nil {
		return fmt.Errorf("update combo: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("combo %q not found", id)
	}
	return nil
}

func (s *sqliteStore) DeleteCombo(id string) error {
	res, err := s.db.Exec("DELETE FROM combos WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete combo: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("combo %q not found", id)
	}
	return nil
}

func scanCombo(row interface{ Scan(dest ...any) error }) (*Combo, error) {
	var c Combo
	var modelsRaw string
	var createdAt string

	if err := row.Scan(&c.ID, &c.Name, &modelsRaw, &createdAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(modelsRaw), &c.Models); err != nil {
		return nil, fmt.Errorf("unmarshal combo models: %w", err)
	}
	c.CreatedAt = parseTime(createdAt)
	return &c, nil
}

// ---------------------------------------------------------------------------
// Aliases
// ---------------------------------------------------------------------------

func (s *sqliteStore) GetAlias(alias string) (string, error) {
	var target string
	err := s.db.QueryRow("SELECT target FROM aliases WHERE alias = ?", alias).Scan(&target)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("alias %q not found", alias)
	}
	return target, err
}

func (s *sqliteStore) SetAlias(alias, target string) error {
	_, err := s.db.Exec(
		"INSERT INTO aliases (alias, target) VALUES (?,?) ON CONFLICT(alias) DO UPDATE SET target = excluded.target",
		alias, target,
	)
	return err
}

func (s *sqliteStore) DeleteAlias(alias string) error {
	res, err := s.db.Exec("DELETE FROM aliases WHERE alias = ?", alias)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("alias %q not found", alias)
	}
	return nil
}

func (s *sqliteStore) ListAliases() (map[string]string, error) {
	rows, err := s.db.Query("SELECT alias, target FROM aliases ORDER BY alias ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var alias, target string
		if err := rows.Scan(&alias, &target); err != nil {
			return nil, err
		}
		out[alias] = target
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// API Keys
// ---------------------------------------------------------------------------

const apiKeyCols = `id, name, key_hash, prefix, budget_monthly, budget_hard_limit, allowed_models, rate_limit_rpm, routing_strategy, created_at`

func scanAPIKey(row interface{ Scan(dest ...any) error }) (*APIKey, error) {
	var k APIKey
	var createdAt string
	if err := row.Scan(&k.ID, &k.Name, &k.KeyHash, &k.Prefix,
		&k.BudgetMonthly, &k.BudgetHardLimit, &k.AllowedModels,
		&k.RateLimitRPM, &k.RoutingStrategy, &createdAt); err != nil {
		return nil, err
	}
	k.CreatedAt = parseTime(createdAt)
	return &k, nil
}

func (s *sqliteStore) ListAPIKeys() ([]APIKey, error) {
	rows, err := s.db.Query("SELECT " + apiKeyCols + " FROM api_keys ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (s *sqliteStore) GetAPIKeyByHash(keyHash string) (*APIKey, error) {
	row := s.db.QueryRow("SELECT "+apiKeyCols+" FROM api_keys WHERE key_hash = ?", keyHash)
	k, err := scanAPIKey(row)
	if err != nil {
		return nil, fmt.Errorf("get api key by hash: %w", err)
	}
	return k, nil
}

func (s *sqliteStore) CreateAPIKey(k *APIKey) error {
	now := timeStr(time.Now().UTC())
	if k.AllowedModels == "" {
		k.AllowedModels = "*"
	}
	_, err := s.db.Exec(
		"INSERT INTO api_keys (id, name, key_hash, prefix, budget_monthly, budget_hard_limit, allowed_models, rate_limit_rpm, routing_strategy, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
		k.ID, k.Name, k.KeyHash, k.Prefix, k.BudgetMonthly, k.BudgetHardLimit,
		k.AllowedModels, k.RateLimitRPM, k.RoutingStrategy, now,
	)
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}
	k.CreatedAt = parseTime(now)
	return nil
}

func (s *sqliteStore) UpdateAPIKey(id string, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	allowed := map[string]bool{
		"name": true, "budget_monthly": true, "budget_hard_limit": true,
		"allowed_models": true, "rate_limit_rpm": true, "routing_strategy": true,
	}
	var setClauses []string
	var params []any
	for col, val := range updates {
		if !allowed[col] {
			continue
		}
		setClauses = append(setClauses, col+" = ?")
		params = append(params, val)
	}
	if len(setClauses) == 0 {
		return nil
	}
	params = append(params, id)
	q := "UPDATE api_keys SET " + joinStrings(setClauses, ", ") + " WHERE id = ?"
	res, err := s.db.Exec(q, params...)
	if err != nil {
		return fmt.Errorf("update api key: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("api key %q not found", id)
	}
	return nil
}

func (s *sqliteStore) ValidateAPIKey(keyHash string) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM api_keys WHERE key_hash = ?", keyHash).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("validate api key: %w", err)
	}
	return count > 0, nil
}

func (s *sqliteStore) HasAPIKeys() (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM api_keys").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("count api keys: %w", err)
	}
	return count > 0, nil
}

func (s *sqliteStore) DeleteAPIKey(id string) error {
	res, err := s.db.Exec("DELETE FROM api_keys WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete api key: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("api key %q not found", id)
	}
	return nil
}

func (s *sqliteStore) GetMonthlySpend(keyID string) (float64, error) {
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var total float64
	err := s.db.QueryRow(
		"SELECT COALESCE(SUM(cost), 0) FROM usage_log WHERE api_key_id = ? AND created_at >= ?",
		keyID, timeStr(monthStart),
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("get monthly spend: %w", err)
	}
	return total, nil
}

// hashKey returns the hex-encoded SHA-256 of a raw API key string.
func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func (s *sqliteStore) GetSetting(key string) (string, error) {
	var val string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("setting %q not found", key)
	}
	return val, err
}

func (s *sqliteStore) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		"INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	return err
}

func (s *sqliteStore) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query("SELECT key, value FROM settings ORDER BY key ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

func (s *sqliteStore) RecordUsage(entry *UsageEntry) error {
	now := timeStr(time.Now().UTC())
	_, err := s.db.Exec(
		`INSERT INTO usage_log (id, request_id, provider, model, connection_id, api_key_id, input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, cost, latency_ms, status, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		entry.ID, entry.RequestID, entry.Provider, entry.Model, entry.ConnectionID,
		entry.APIKeyID, entry.InputTokens, entry.OutputTokens, entry.TotalTokens,
		entry.CacheReadTokens, entry.CacheWriteTokens,
		entry.Cost, entry.Latency.Milliseconds(), entry.Status, now,
	)
	if err != nil {
		return fmt.Errorf("record usage: %w", err)
	}
	entry.CreatedAt = parseTime(now)
	return nil
}

func (s *sqliteStore) QueryUsage(filter UsageFilter) ([]UsageEntry, error) {
	q, params := buildUsageQuery(filter)
	rows, err := s.db.Query(q, params...)
	if err != nil {
		return nil, fmt.Errorf("query usage: %w", err)
	}
	defer rows.Close()

	var out []UsageEntry
	for rows.Next() {
		e, err := scanUsageEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) UsageSummary(filter UsageFilter) (*UsageSummary, error) {
	w := buildUsageFilter(filter)

	// Overall totals.
	qTotal := "SELECT COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost),0) FROM usage_log" + w.sql()
	var summary UsageSummary
	if err := s.db.QueryRow(qTotal, w.params...).Scan(
		&summary.TotalRequests, &summary.TotalTokens, &summary.TotalCost,
	); err != nil {
		return nil, fmt.Errorf("usage summary totals: %w", err)
	}

	// Per-provider breakdown.
	qProv := "SELECT provider, COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost),0) FROM usage_log" + w.sql() + " GROUP BY provider"
	rows, err := s.db.Query(qProv, w.params...)
	if err != nil {
		return nil, fmt.Errorf("usage summary by provider: %w", err)
	}
	defer rows.Close()

	summary.ByProvider = make(map[string]ProviderSummary)
	for rows.Next() {
		var prov string
		var ps ProviderSummary
		if err := rows.Scan(&prov, &ps.Requests, &ps.Tokens, &ps.Cost); err != nil {
			return nil, err
		}
		summary.ByProvider[prov] = ps
	}

	return &summary, rows.Err()
}

func scanUsageEntry(row interface{ Scan(dest ...any) error }) (*UsageEntry, error) {
	var e UsageEntry
	var latencyMs int64
	var createdAt string

	err := row.Scan(
		&e.ID, &e.RequestID, &e.Provider, &e.Model, &e.ConnectionID,
		&e.APIKeyID, &e.InputTokens, &e.OutputTokens, &e.TotalTokens,
		&e.CacheReadTokens, &e.CacheWriteTokens,
		&e.Cost, &latencyMs, &e.Status, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	e.Latency = time.Duration(latencyMs) * time.Millisecond
	e.CreatedAt = parseTime(createdAt)
	return &e, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// timeStr formats a time.Time to the string format stored in SQLite.
func timeStr(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// parseTime parses a time string stored in SQLite back to time.Time.
// It tries ISO 8601 first, then the SQLite datetime() default format.
func parseTime(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		t, _ = time.Parse("2006-01-02 15:04:05", s)
	}
	return t
}

// joinStrings is a tiny helper so we don't import strings in this file for one call.
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}

// --- Routing Telemetry ---

func (s *sqliteStore) RecordRouting(entry *RoutingEntry) error {
	now := timeStr(time.Now().UTC())
	affinityHit := 0
	if entry.AffinityHit {
		affinityHit = 1
	}
	affinityBreak := 0
	if entry.AffinityBreak {
		affinityBreak = 1
	}
	bridgeInjected := 0
	if entry.BridgeInjected {
		bridgeInjected = 1
	}

	_, err := s.db.Exec(
		`INSERT INTO routing_log (id, request_id, strategy, provider, model, routing_reason, affinity_hit, affinity_break, bridge_injected, constraints, candidates, filtered, latency_ms, status, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		entry.ID, entry.RequestID, entry.Strategy, entry.Provider, entry.Model,
		entry.RoutingReason, affinityHit, affinityBreak, bridgeInjected,
		entry.Constraints, entry.CandidateCount, entry.FilteredCount,
		entry.LatencyMs, entry.Status, now,
	)
	if err != nil {
		return fmt.Errorf("record routing: %w", err)
	}
	entry.CreatedAt = parseTime(now)
	return nil
}

func (s *sqliteStore) QueryRoutingLog(filter UsageFilter) ([]RoutingEntry, error) {
	w := buildUsageFilter(filter)
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	q := "SELECT id, request_id, strategy, provider, model, routing_reason, affinity_hit, affinity_break, bridge_injected, constraints, candidates, filtered, latency_ms, status, created_at FROM routing_log" + w.sql() + " ORDER BY created_at DESC LIMIT ?"
	params := append(w.params, limit)

	rows, err := s.db.Query(q, params...)
	if err != nil {
		return nil, fmt.Errorf("query routing log: %w", err)
	}
	defer rows.Close()

	var out []RoutingEntry
	for rows.Next() {
		var e RoutingEntry
		var affinityHit, affinityBreak, bridgeInjected int
		var createdAt string
		if err := rows.Scan(
			&e.ID, &e.RequestID, &e.Strategy, &e.Provider, &e.Model,
			&e.RoutingReason, &affinityHit, &affinityBreak, &bridgeInjected,
			&e.Constraints, &e.CandidateCount, &e.FilteredCount,
			&e.LatencyMs, &e.Status, &createdAt,
		); err != nil {
			return nil, err
		}
		e.AffinityHit = affinityHit == 1
		e.AffinityBreak = affinityBreak == 1
		e.BridgeInjected = bridgeInjected == 1
		e.CreatedAt = parseTime(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) RoutingSummary(filter UsageFilter) (*RoutingSummary, error) {
	w := buildUsageFilter(filter)
	// Reuse the same time filter, just against routing_log table.
	whereSQL := w.sql()

	var summary RoutingSummary
	qTotal := "SELECT COUNT(*), COALESCE(SUM(affinity_hit),0), COALESCE(SUM(bridge_injected),0) FROM routing_log" + whereSQL
	var totalDecisions, totalAffinityHits, totalBridges int
	if err := s.db.QueryRow(qTotal, w.params...).Scan(&totalDecisions, &totalAffinityHits, &totalBridges); err != nil {
		return nil, fmt.Errorf("routing summary totals: %w", err)
	}
	summary.TotalDecisions = totalDecisions
	summary.BridgeInjections = totalBridges
	if totalDecisions > 0 {
		summary.AffinityHitRate = float64(totalAffinityHits) / float64(totalDecisions)
	}

	// By strategy
	qStrat := "SELECT strategy, COUNT(*) FROM routing_log" + whereSQL + " GROUP BY strategy"
	rows, err := s.db.Query(qStrat, w.params...)
	if err != nil {
		return nil, fmt.Errorf("routing summary by strategy: %w", err)
	}
	defer rows.Close()

	summary.ByStrategy = make(map[string]int)
	for rows.Next() {
		var strat string
		var count int
		if err := rows.Scan(&strat, &count); err != nil {
			return nil, err
		}
		summary.ByStrategy[strat] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// By provider
	qProv := "SELECT provider, COUNT(*) FROM routing_log" + whereSQL + " GROUP BY provider"
	rows2, err := s.db.Query(qProv, w.params...)
	if err != nil {
		return nil, fmt.Errorf("routing summary by provider: %w", err)
	}
	defer rows2.Close()

	summary.ByProvider = make(map[string]int)
	for rows2.Next() {
		var prov string
		var count int
		if err := rows2.Scan(&prov, &count); err != nil {
			return nil, err
		}
		summary.ByProvider[prov] = count
	}

	return &summary, rows2.Err()
}
