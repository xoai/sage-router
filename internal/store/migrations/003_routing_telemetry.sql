-- 003_routing_telemetry.sql: routing decision tracking for smart routing analytics

CREATE TABLE IF NOT EXISTS routing_log (
    id              TEXT PRIMARY KEY,
    request_id      TEXT NOT NULL,
    strategy        TEXT NOT NULL DEFAULT 'manual',
    provider        TEXT NOT NULL,
    model           TEXT NOT NULL,
    routing_reason  TEXT NOT NULL DEFAULT '',
    affinity_hit    INTEGER NOT NULL DEFAULT 0,
    affinity_break  INTEGER NOT NULL DEFAULT 0,
    bridge_injected INTEGER NOT NULL DEFAULT 0,
    constraints     TEXT NOT NULL DEFAULT '{}',
    candidates      INTEGER NOT NULL DEFAULT 0,
    filtered        INTEGER NOT NULL DEFAULT 0,
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'ok',
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_routing_log_strategy ON routing_log(strategy);
CREATE INDEX IF NOT EXISTS idx_routing_log_provider ON routing_log(provider);
CREATE INDEX IF NOT EXISTS idx_routing_log_created_at ON routing_log(created_at);
CREATE INDEX IF NOT EXISTS idx_routing_log_request_id ON routing_log(request_id);
