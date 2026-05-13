-- 006_normalize_auth_type.sql: normalize legacy AuthType vocabulary.
-- Idempotent at the SQL level: UPDATE on a non-matching row is a no-op.
-- The migration runner additionally guarantees exactly-once application.

UPDATE connections SET auth_type = 'apikey'       WHERE auth_type = 'api_key';
UPDATE connections SET auth_type = 'subscription' WHERE auth_type = 'oauth';
