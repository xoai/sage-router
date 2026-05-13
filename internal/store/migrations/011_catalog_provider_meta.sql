-- 011_catalog_provider_meta.sql: per-provider discovery-loop state.
--
-- Distinct from `internal/config/providers.go: KnownProviders` —
-- this table holds runtime discovery state (last refresh time, last
-- error, exponential backoff counter), not static provider
-- definitions. KnownProviders remains alive as the source for Name,
-- Format, BaseURL, AuthTypes, Models[]. See ADR-1 §"Field source"
-- table and the holistic /review RC1 resolution.
--
-- discovery_enabled defaults to 0 (fail-closed) — an absent row
-- means "do not discover." catalog.SeedProviderMeta() writes the
-- per-provider true/false values from the documented defaults
-- table on first boot.
--
-- backoff_step is a 0..5 counter; recordResult() in
-- internal/catalog/refresh.go maps it to [1h, 2h, 4h, 8h, 24h].
-- Replaces the r1 design's Sub()-delta-decoding which had an
-- off-by-one (see ADR-2 §Exponential backoff for the r2 fix).

CREATE TABLE IF NOT EXISTS catalog_provider_meta (
    provider                  TEXT PRIMARY KEY,
    discovery_enabled         INTEGER NOT NULL DEFAULT 0,
    subscription_discoverable INTEGER NOT NULL DEFAULT 0,
    last_discovered_at        TIMESTAMP,
    last_discovery_error      TEXT NOT NULL DEFAULT '',
    backoff_step              INTEGER NOT NULL DEFAULT 0,
    next_discovery_after      TIMESTAMP
);
