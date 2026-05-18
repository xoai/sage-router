-- 014_usage_log_index_composite.sql: swap the standalone api_key_id
-- index for a composite (api_key_id, created_at DESC). The composite
-- covers the standalone's job via SQLite's leftmost-prefix rule, so
-- the standalone is dropped to save write cost on the hot usage_log
-- insert path.
--
-- Optimization targets (per spec 20260517-usage-page-filters):
--   Q2 budget query: api_key_id = ? AND created_at >= ?
--   Q3 multi-key range: api_key_id IN (...) AND created_at BETWEEN ? AND ? ORDER BY created_at DESC

DROP INDEX IF EXISTS idx_usage_log_api_key_id;
CREATE INDEX IF NOT EXISTS idx_usage_log_api_key_id_created_at
    ON usage_log(api_key_id, created_at DESC);
