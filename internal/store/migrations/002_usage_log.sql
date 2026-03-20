-- 002_usage_log.sql: usage tracking table

CREATE TABLE IF NOT EXISTS usage_log (
    id            TEXT PRIMARY KEY,
    request_id    TEXT NOT NULL,
    provider      TEXT NOT NULL,
    model         TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens  INTEGER NOT NULL DEFAULT 0,
    cost          REAL NOT NULL DEFAULT 0.0,
    latency_ms    INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'ok',
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_usage_log_provider ON usage_log(provider);
CREATE INDEX IF NOT EXISTS idx_usage_log_model ON usage_log(model);
CREATE INDEX IF NOT EXISTS idx_usage_log_created_at ON usage_log(created_at);
CREATE INDEX IF NOT EXISTS idx_usage_log_connection_id ON usage_log(connection_id);
