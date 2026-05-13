package catalog

import (
	"context"
	"fmt"
	"strings"

	"sage-router/internal/config"
)

// SeedFromConstants populates catalog_models + catalog_pricing from
// the static constants in internal/config/{models,pricing}.go. Writes
// rows with source='seed'. Idempotent — re-running is a no-op because
// the source-precedence WHERE clause refuses to overwrite a seed row
// with another seed row's identical content (the row's existing data
// matches what we'd write, so the WHERE matches but the UPDATE is a
// silent no-op for fields that are unchanged; updated_at would tick,
// so we skip the UPDATE entirely via the source-precedence guard on
// seed→seed... actually no — seed CAN overwrite seed in our WHERE
// clause; we rely on the seed values being byte-identical across
// runs to make the UPDATE semantically idempotent).
//
// For Anthropic-family rows (provider="anthropic" and ModelID contains
// "claude"), CacheWrite is set to Input * 1.25 explicitly — see AC2d
// and ADR-3 §Part B "Cache-write semantics are per-provider." For all
// other providers, CacheWrite=0 means "free at write" by convention.
//
// AC4 + AC2b: callable from test code (no main.go dependency).
func SeedFromConstants(ctx context.Context, store Store) error {
	for modelID, m := range config.ModelCatalog {
		mod := Model{
			Provider:      m.Provider,
			ModelID:       modelID,
			DisplayName:   m.DisplayName,
			Tier:          m.Tier,
			ContextWindow: m.ContextWindow,
			MaxOutput:     m.MaxOutput,
			Caps: Capabilities{
				SupportsImages:   m.SupportsImages,
				SupportsTools:    m.SupportsTools,
				SupportsThinking: m.SupportsThinking,
			},
			Source: SourceSeed,
		}
		if err := store.UpsertModel(ctx, mod); err != nil {
			return fmt.Errorf("seed model %s/%s: %w", m.Provider, modelID, err)
		}

		// Build the seed pricing row. The static catalog only carries
		// (InputPrice, OutputPrice). Cache pricing is filled in by the
		// OpenRouter refresh and per-provider discovery (M2). The one
		// exception is Anthropic — AC2d requires explicit CacheWrite =
		// Input * 1.25.
		pr := Pricing{
			Input:  m.InputPrice,
			Output: m.OutputPrice,
			Source: SourceSeed,
		}
		if isAnthropicFamily(m.Provider, modelID) {
			pr.CacheWrite = m.InputPrice * 1.25
		}
		if err := store.UpsertPricing(ctx, m.Provider, modelID, pr); err != nil {
			return fmt.Errorf("seed pricing %s/%s: %w", m.Provider, modelID, err)
		}
	}
	return nil
}

// isAnthropicFamily identifies seed rows that need explicit CacheWrite
// pricing. Today: provider=="anthropic" AND model ID contains "claude".
// Extends naturally if we add Anthropic-clone providers later.
func isAnthropicFamily(provider, modelID string) bool {
	if provider != "anthropic" {
		return false
	}
	return strings.Contains(modelID, "claude")
}

// providerMetaDefaults is the seeded-defaults table from ADR-2
// §Seeded defaults (revised after r1 review). discovery_enabled is
// fail-closed by DDL DEFAULT (0); the seeder writes the explicit true
// values for the five providers we've verified, plus false for
// github-copilot (no confirmed public /v1/models endpoint as of
// 2026-05-12 per task 1.3.5).
//
// subscription_discoverable defaults to false; openai and anthropic
// have OAuth flows that list models per their respective docs (Claude
// OAuth uses the same /v1/models endpoint — see
// docs/anthropic-discovery-feasibility.md).
var providerMetaDefaults = []ProviderMeta{
	{Provider: "openai", DiscoveryEnabled: true, SubscriptionDiscoverable: true},
	{Provider: "anthropic", DiscoveryEnabled: true, SubscriptionDiscoverable: true},
	{Provider: "gemini", DiscoveryEnabled: true, SubscriptionDiscoverable: false},
	{Provider: "openrouter", DiscoveryEnabled: true, SubscriptionDiscoverable: false},
	{Provider: "ollama", DiscoveryEnabled: true, SubscriptionDiscoverable: false},
	{Provider: "github-copilot", DiscoveryEnabled: false, SubscriptionDiscoverable: false},
}

// SeedProviderMeta writes one row per known provider with the
// documented defaults. Idempotent: existing rows are NOT overwritten.
// This is critical — the discovery loop writes BackoffStep,
// LastDiscoveredAt, etc. at runtime, and re-running the seeder must
// not clobber that runtime state. See AC2c + TestSeedProviderMeta_Idempotent.
//
// All seeded rows have BackoffStep=0 and NextDiscoveryAfter=zero
// (the ProviderMeta zero-value, which the SQLite store translates
// to NULL via nullTimestamp). AC2c.
func SeedProviderMeta(ctx context.Context, store Store) error {
	for _, pm := range providerMetaDefaults {
		existing, err := store.GetProviderMeta(ctx, pm.Provider)
		if err != nil {
			return fmt.Errorf("seed provider meta read %s: %w", pm.Provider, err)
		}
		if existing != nil {
			// Already seeded — skip. Preserves runtime state from prior
			// discovery-loop writes (BackoffStep, LastDiscoveredAt, etc).
			continue
		}
		if err := store.SetProviderMeta(ctx, pm.Provider, pm); err != nil {
			return fmt.Errorf("seed provider meta write %s: %w", pm.Provider, err)
		}
	}
	return nil
}
