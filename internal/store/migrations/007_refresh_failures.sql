-- 007_refresh_failures.sql: track consecutive subscription refresh failures.
-- SQLite ADD COLUMN with DEFAULT populates existing rows with the default
-- value, so no separate backfill statement is needed.

ALTER TABLE connections ADD COLUMN refresh_failures INTEGER NOT NULL DEFAULT 0;
