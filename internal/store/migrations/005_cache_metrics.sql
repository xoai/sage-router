-- 005_cache_metrics.sql: add cache token tracking to usage_log

ALTER TABLE usage_log ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_log ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
