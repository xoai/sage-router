package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrSettingNotFound is returned (wrapped via %w) by Store.GetSetting
// when no row exists for the requested key. Callers that need to
// distinguish "row absent" from "I/O failure" can check via
// `errors.Is(err, store.ErrSettingNotFound)`. The wrapping message
// includes the key name for log-side diagnostics; the sentinel
// guarantees structural matching independent of the message format.
var ErrSettingNotFound = errors.New("setting not found")

// Store defines the persistence interface for sage-router.
type Store interface {
	// Connections
	ListConnections(filter ConnectionFilter) ([]Connection, error)
	GetConnection(id string) (*Connection, error)
	CreateConnection(c *Connection) error
	UpdateConnection(id string, updates map[string]any) error
	DeleteConnection(id string) error

	// Subscription refresh-failure counter (DB-canonical; no in-memory shadow).
	BumpConnectionRefreshFailures(id string) (int, error)
	GetConnectionRefreshFailures(id string) (int, error)

	// Combos (model groups)
	ListCombos() ([]Combo, error)
	GetComboByName(name string) (*Combo, error)
	CreateCombo(c *Combo) error
	UpdateCombo(id string, c *Combo) error
	DeleteCombo(id string) error

	// Aliases (model name aliases)
	GetAlias(alias string) (string, error)
	SetAlias(alias, target string) error
	DeleteAlias(alias string) error
	ListAliases() (map[string]string, error)

	// API Keys
	ListAPIKeys() ([]APIKey, error)
	GetAPIKeyByHash(keyHash string) (*APIKey, error)
	CreateAPIKey(k *APIKey) error
	UpdateAPIKey(id string, updates map[string]any) error
	ValidateAPIKey(keyHash string) (bool, error)
	HasAPIKeys() (bool, error)
	DeleteAPIKey(id string) error
	GetMonthlySpend(keyID string) (float64, error)

	// Settings (key-value configuration).
	//
	// GetSetting returns (value, nil) when the row exists (including
	// when the stored value is the explicit empty string), and
	// ("", wrapped ErrSettingNotFound) when the row is missing. Use
	// `errors.Is(err, ErrSettingNotFound)` to distinguish the absent
	// case from I/O errors. A non-nil error that does NOT match the
	// sentinel indicates a real DB problem (driver, schema, transport).
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
	AllSettings() (map[string]string, error)

	// Usage tracking
	RecordUsage(entry *UsageEntry) error
	QueryUsage(filter UsageFilter) ([]UsageEntry, error)
	UsageSummary(filter UsageFilter) (*UsageSummary, error)
	SubscriptionUsageGroups(filter UsageFilter) ([]SubscriptionUsageGroup, error)

	// Catalog-driven usage queries (added by Models Discovery M1).
	//
	// GetCacheHitRate returns the ratio of cache_read_tokens to total
	// input-side tokens for the given connection within the lookback
	// window. Used by smart-routing `cheap` strategy to compute
	// cache-aware effective price. Returns 0 on no data or div-by-zero.
	GetCacheHitRate(ctx context.Context, connectionID string, lookback time.Duration) (float64, error)

	// ListUsageInRange returns usage_log rows in the given date range,
	// optionally filtered by cost_source ("apikey" / "subscription";
	// "" = all). Used by the M3 recompute admin endpoint to re-apply
	// pricing to apikey rows only.
	ListUsageInRange(ctx context.Context, from, to time.Time, costSource string) ([]UsageEntry, error)

	// UpdateUsageCost overwrites the cost column on one usage_log row.
	// No effect when id doesn't exist (idempotent no-op). Used by
	// recompute; never touches cost_source.
	UpdateUsageCost(ctx context.Context, id string, cost float64) error

	// Routing telemetry
	RecordRouting(entry *RoutingEntry) error
	QueryRoutingLog(filter UsageFilter) ([]RoutingEntry, error)
	RoutingSummary(filter UsageFilter) (*RoutingSummary, error)

	// Encryption
	SetEncryptionKey(key []byte)

	// Lifecycle
	Migrate() error
	Close() error

	// DB exposes the underlying *sql.DB for cross-package wiring —
	// the catalog Store (internal/catalog) shares the same connection
	// for FK consistency and to avoid double-opening SQLite. Do NOT
	// use this for application logic; go through the Store interface
	// methods so the impl can be swapped under tests.
	DB() *sql.DB
}

// Connection represents a configured provider connection.
type Connection struct {
	ID              string          `json:"id"`
	Provider        string          `json:"provider"`
	Name            string          `json:"name"`
	AuthType        string          `json:"auth_type"`
	AccessToken     string          `json:"access_token,omitempty"`
	RefreshToken    string          `json:"refresh_token,omitempty"`
	APIKey          string          `json:"api_key,omitempty"`
	Priority        int             `json:"priority"`
	State           string          `json:"state"`
	ExpiresAt       *time.Time      `json:"expires_at,omitempty"`
	ProviderData    json.RawMessage `json:"provider_data,omitempty"`
	RefreshFailures int             `json:"refresh_failures"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// ConnectionFilter controls which connections are returned by ListConnections.
type ConnectionFilter struct {
	Provider   string   `json:"provider,omitempty"`
	State      string   `json:"state,omitempty"`
	ExcludeIDs []string `json:"exclude_ids,omitempty"`
}

// Combo is a named group of models that can be referenced as a single target.
type Combo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Models    []string `json:"models"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKey represents a hashed API key for authenticating requests to sage-router.
type APIKey struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	KeyHash         string    `json:"-"`
	Prefix          string    `json:"prefix"`
	BudgetMonthly   float64   `json:"budget_monthly"`
	BudgetHardLimit bool      `json:"budget_hard_limit"`
	AllowedModels   string    `json:"allowed_models"`
	RateLimitRPM    int       `json:"rate_limit_rpm"`
	RoutingStrategy string    `json:"routing_strategy"`
	CreatedAt       time.Time `json:"created_at"`
}

// UsageEntry records a single proxied request for billing and analytics.
type UsageEntry struct {
	ID               string        `json:"id"`
	RequestID        string        `json:"request_id"`
	Provider         string        `json:"provider"`
	Model            string        `json:"model"`
	ConnectionID     string        `json:"connection_id"`
	APIKeyID         string        `json:"api_key_id"`
	InputTokens      int           `json:"input_tokens"`
	OutputTokens     int           `json:"output_tokens"`
	TotalTokens      int           `json:"total_tokens"`
	CacheReadTokens  int           `json:"cache_read_tokens"`
	CacheWriteTokens int           `json:"cache_write_tokens"`
	Cost             float64       `json:"cost"`
	// CostSource records who pays for this request. "apikey" means the
	// user pays per-token via their API key — Cost reflects actual spend.
	// "subscription" means the user paid a flat subscription fee — Cost is
	// 0 and the dashboard computes "savings" against the would-have-been
	// API cost via the pricing table.
	CostSource string        `json:"cost_source"`
	Latency    time.Duration `json:"latency"`
	Status     string        `json:"status"`
	CreatedAt  time.Time     `json:"created_at"`
}

// UsageFilter controls which usage entries are returned or summarised.
type UsageFilter struct {
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`
	APIKeyID string    `json:"api_key_id,omitempty"`
	From     time.Time `json:"from,omitempty"`
	To       time.Time `json:"to,omitempty"`
	Limit    int       `json:"limit,omitempty"`
}

// UsageSummary is an aggregated view of usage data.
type UsageSummary struct {
	TotalRequests int                        `json:"total_requests"`
	TotalTokens   int                        `json:"total_tokens"`
	TotalCost     float64                    `json:"total_cost"`
	ByProvider    map[string]ProviderSummary `json:"by_provider"`

	// SubscriptionSavings is the sum of would-have-been API costs across
	// rows where CostSource = "subscription". Computed at query time by
	// re-applying the current pricing table to each row's token counts —
	// pricing-table updates retroactively reflect in the reported savings.
	SubscriptionSavings float64 `json:"subscription_savings"`

	// ByCostSource breaks the summary down by who paid. The map key is
	// "apikey" or "subscription". Useful for the dashboard's "API cost vs
	// subscription savings" widget.
	ByCostSource map[string]CostSourceSummary `json:"by_cost_source"`
}

// CostSourceSummary is the per-cost-source slice of UsageSummary.
type CostSourceSummary struct {
	Requests int     `json:"requests"`
	Tokens   int     `json:"tokens"`
	Cost     float64 `json:"cost"`
}

// SubscriptionUsageGroup is the (provider, model, input/output/cache)
// aggregation the server uses to compute subscription_savings via the
// catalog pricing. Returned by Store.SubscriptionUsageGroups.
//
// Cache token sums (added by Models Discovery M1.8b — RC2 fix) are
// needed for AC25b: subscription_savings must reflect cache-read
// pricing when the aggregation window contains rows with
// cache_read_tokens > 0. Without these fields, the routes_api.go:636
// rewire would silently multiply cache pricing by zero.
type SubscriptionUsageGroup struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
}

// ProviderSummary is per-provider aggregated usage.
type ProviderSummary struct {
	Requests int     `json:"requests"`
	Tokens   int     `json:"tokens"`
	Cost     float64 `json:"cost"`
}

// RoutingEntry records a single routing decision for analytics.
type RoutingEntry struct {
	ID              string    `json:"id"`
	RequestID       string    `json:"request_id"`
	Strategy        string    `json:"strategy"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	RoutingReason   string    `json:"routing_reason"`
	AffinityHit     bool      `json:"affinity_hit"`
	AffinityBreak   bool      `json:"affinity_break"`
	BridgeInjected  bool      `json:"bridge_injected"`
	Constraints     string    `json:"constraints"`
	CandidateCount  int       `json:"candidates"`
	FilteredCount   int       `json:"filtered"`
	LatencyMs       int       `json:"latency_ms"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}

// RoutingSummary is aggregated routing analytics.
type RoutingSummary struct {
	TotalDecisions   int            `json:"total_decisions"`
	AffinityHitRate  float64        `json:"affinity_hit_rate"`
	BridgeInjections int            `json:"bridge_injections"`
	ByStrategy       map[string]int `json:"by_strategy"`
	ByProvider       map[string]int `json:"by_provider"`
}
