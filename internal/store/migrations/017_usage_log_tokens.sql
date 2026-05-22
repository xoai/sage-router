-- 017_usage_log_tokens.sql: the M4 dual-sourced compression-savings
-- measurement (cycle 20260522-m4-compression).
--
-- tokens_before — the tokenizer's estimate of the request BEFORE
--   compression. The uncompressed request is never sent upstream, so no
--   provider count for it can exist; this term is unavoidably an estimate.
-- tokens_after  — the provider's ACTUAL post-request input-token count.
--
-- Both DEFAULT 0. A row from a request that was not compressed (key flag
-- off, or under the pre-flight gate) leaves both 0 — no savings is shown.

ALTER TABLE usage_log ADD COLUMN tokens_before INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_log ADD COLUMN tokens_after  INTEGER NOT NULL DEFAULT 0;
