-- 010_catalog_pricing.sql: per-model pricing with cache tiers.
--
-- Split from catalog_models because pricing refreshes daily (via the
-- OpenRouter oracle) while capability metadata changes only when a
-- provider ships a new model. Update-amplification analysis favors
-- separation; ON DELETE CASCADE keeps the two tables consistent.
--
-- Prices are USD per 1M tokens. CacheWrite=0 means "free at write"
-- by convention (e.g., OpenAI); Anthropic-family seeds explicitly
-- write CacheWrite = Input * 1.25. EstimateCost reads p.CacheWrite
-- verbatim — there is no cross-provider default.
--
-- See .sage/docs/decision-models-pricing-resolution.md §Part B for
-- the per-provider cache-write table and the rationale for
-- killing the cross-provider 1.25× default that the r1 design used.

CREATE TABLE IF NOT EXISTS catalog_pricing (
    provider          TEXT NOT NULL,
    model_id          TEXT NOT NULL,
    input_price       REAL NOT NULL DEFAULT 0,
    output_price      REAL NOT NULL DEFAULT 0,
    cache_read_price  REAL NOT NULL DEFAULT 0,
    cache_write_price REAL NOT NULL DEFAULT 0,
    thinking_price    REAL NOT NULL DEFAULT 0,
    source            TEXT NOT NULL DEFAULT 'seed'
                      CHECK (source IN ('seed', 'discovery', 'openrouter', 'user')),
    updated_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (provider, model_id),
    FOREIGN KEY (provider, model_id)
        REFERENCES catalog_models(provider, model_id) ON DELETE CASCADE
);
