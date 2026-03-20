-- 004_keys_as_groups.sql: per-key budget, ACL, rate-limit, routing strategy (§33-34)

ALTER TABLE api_keys ADD COLUMN budget_monthly    REAL    NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN budget_hard_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN allowed_models    TEXT    NOT NULL DEFAULT '*';
ALTER TABLE api_keys ADD COLUMN rate_limit_rpm    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN routing_strategy  TEXT    NOT NULL DEFAULT '';

ALTER TABLE usage_log ADD COLUMN api_key_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_usage_log_api_key_id ON usage_log(api_key_id);
