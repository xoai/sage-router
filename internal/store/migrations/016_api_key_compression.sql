-- 016_api_key_compression.sql: per-API-key opt-in for tool-output
-- compression (cycle 20260522-m4-compression, M4).
--
-- INTEGER 0/1 — SQLite has no boolean type; this mirrors the existing
-- budget_hard_limit column. DEFAULT 0: compression is OFF unless the
-- operator explicitly enables it for a key.

ALTER TABLE api_keys ADD COLUMN compression_enabled INTEGER NOT NULL DEFAULT 0;
