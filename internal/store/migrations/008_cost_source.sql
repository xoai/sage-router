-- 008_cost_source.sql: distinguish subscription-served requests (cost=0)
-- from API-key-served requests. Pre-existing usage_log rows are backfilled
-- with 'apikey' via the column DEFAULT.

ALTER TABLE usage_log ADD COLUMN cost_source TEXT NOT NULL DEFAULT 'apikey';
