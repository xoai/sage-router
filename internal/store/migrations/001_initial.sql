-- 001_initial.sql: core tables for sage-router

CREATE TABLE IF NOT EXISTS connections (
    id          TEXT PRIMARY KEY,
    provider    TEXT NOT NULL,
    name        TEXT NOT NULL,
    auth_type   TEXT NOT NULL DEFAULT 'api_key',
    access_token  TEXT NOT NULL DEFAULT '',
    refresh_token TEXT NOT NULL DEFAULT '',
    api_key     TEXT NOT NULL DEFAULT '',
    priority    INTEGER NOT NULL DEFAULT 0,
    state       TEXT NOT NULL DEFAULT 'active',
    expires_at  TEXT,
    provider_data TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_connections_provider ON connections(provider);
CREATE INDEX IF NOT EXISTS idx_connections_state ON connections(state);

CREATE TABLE IF NOT EXISTS combos (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    models     TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS aliases (
    alias  TEXT PRIMARY KEY,
    target TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    key_hash   TEXT NOT NULL UNIQUE,
    prefix     TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
