-- 009_catalog_models.sql: dynamic model catalog.
--
-- Replaces the read path of internal/config/models.go. Each row
-- describes one model available from one provider. Capability flags
-- (supports_images/tools/thinking) drive smart-routing constraint
-- filtering; tier drives strategy sort; source tracks data
-- provenance for precedence-based upserts.
--
-- See .sage/docs/decision-models-catalog-storage.md for design
-- rationale (especially the two-tables split from catalog_pricing,
-- and the source-enum CHECK lock-in trade-off).

CREATE TABLE IF NOT EXISTS catalog_models (
    provider          TEXT NOT NULL,
    model_id          TEXT NOT NULL,
    display_name      TEXT NOT NULL DEFAULT '',
    tier              INTEGER NOT NULL DEFAULT 3,
    context_window    INTEGER NOT NULL DEFAULT 0,
    max_output        INTEGER NOT NULL DEFAULT 0,
    supports_images   INTEGER NOT NULL DEFAULT 0,
    supports_tools    INTEGER NOT NULL DEFAULT 0,
    supports_thinking INTEGER NOT NULL DEFAULT 0,
    source            TEXT NOT NULL DEFAULT 'seed'
                      CHECK (source IN ('seed', 'discovery', 'openrouter', 'user')),
    discovered_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (provider, model_id)
);

CREATE INDEX IF NOT EXISTS idx_catalog_models_provider ON catalog_models(provider);
