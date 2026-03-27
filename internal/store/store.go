package store

import (
	"encoding/json"
	"time"
)

// Store defines the persistence interface for sage-router.
type Store interface {
	// Connections
	ListConnections(filter ConnectionFilter) ([]Connection, error)
	GetConnection(id string) (*Connection, error)
	CreateConnection(c *Connection) error
	UpdateConnection(id string, updates map[string]any) error
	DeleteConnection(id string) error

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

	// Settings (key-value configuration)
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
	AllSettings() (map[string]string, error)

	// Usage tracking
	RecordUsage(entry *UsageEntry) error
	QueryUsage(filter UsageFilter) ([]UsageEntry, error)
	UsageSummary(filter UsageFilter) (*UsageSummary, error)

	// Routing telemetry
	RecordRouting(entry *RoutingEntry) error
	QueryRoutingLog(filter UsageFilter) ([]RoutingEntry, error)
	RoutingSummary(filter UsageFilter) (*RoutingSummary, error)

	// Encryption
	SetEncryptionKey(key []byte)

	// Lifecycle
	Migrate() error
	Close() error
}

// Connection represents a configured provider connection.
type Connection struct {
	ID           string          `json:"id"`
	Provider     string          `json:"provider"`
	Name         string          `json:"name"`
	AuthType     string          `json:"auth_type"`
	AccessToken  string          `json:"access_token,omitempty"`
	RefreshToken string          `json:"refresh_token,omitempty"`
	APIKey       string          `json:"api_key,omitempty"`
	Priority     int             `json:"priority"`
	State        string          `json:"state"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	ProviderData json.RawMessage `json:"provider_data,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
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
	ID              string        `json:"id"`
	RequestID       string        `json:"request_id"`
	Provider        string        `json:"provider"`
	Model           string        `json:"model"`
	ConnectionID    string        `json:"connection_id"`
	APIKeyID        string        `json:"api_key_id"`
	InputTokens     int           `json:"input_tokens"`
	OutputTokens    int           `json:"output_tokens"`
	TotalTokens     int           `json:"total_tokens"`
	CacheReadTokens  int          `json:"cache_read_tokens"`
	CacheWriteTokens int          `json:"cache_write_tokens"`
	Cost            float64       `json:"cost"`
	Latency         time.Duration `json:"latency"`
	Status          string        `json:"status"`
	CreatedAt       time.Time     `json:"created_at"`
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
