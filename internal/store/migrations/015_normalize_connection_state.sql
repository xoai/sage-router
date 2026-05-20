-- 015_normalize_connection_state.sql: normalize the connections.state column
-- to the persistable Lifecycle/Auth vocabulary {idle, disabled, auth_expired}
-- ahead of the M2 three-facet circuit-breaker model
-- (cycle 20260520-m2-circuit-breaker).
--
-- Transient-health values (active, rate_limited, cooldown, errored, refreshing)
-- are in-memory-only under the facet model and must not persist — they collapse
-- to 'idle'. 'disabled' and 'auth_expired' are kept: the load path hydrates the
-- Lifecycle/Auth facets from them. The breaker facet is never persisted; a
-- connection loads CLOSED on restart.
--
-- Idempotent at the SQL level: after this runs, no row matches the transient
-- set, so re-applying it (e.g. after a crash before the _migrations row was
-- written) changes zero rows. Runs in its own transaction.

BEGIN;
UPDATE connections SET state = 'idle'
 WHERE state IN ('active', 'rate_limited', 'cooldown', 'errored', 'refreshing');
COMMIT;
