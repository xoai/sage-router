-- Cycle 20260517-provider-auth-variants M2.6.4.
--
-- Reverses migration 012 (cycle 20260517-openai-subscription-responses-api).
-- The RFC 8693 exchanged_token plumbing was wrong-path per memory `f32bbc73`
-- (architectural-findings.md): api.openai.com/v1/responses is unreachable
-- for ChatGPT subscribers. CodexSubscriptionExecutor uses the PKCE
-- access_token directly against chatgpt.com/backend-api/codex/responses
-- with no exchange step, so the column is dead data.
--
-- Existing rows lose their `exchanged_token` value. Acceptable per AC-R6
-- (M0 fold): connections continue working via Credentials.AccessToken which
-- survives the migration. CodexSubscriptionExecutor's PreflightChecker
-- handles the "access_token empty" case (e.g., for rows seeded without
-- having completed PKCE) by surfacing ErrTierMissingScopes → AuthExpired
-- so the dashboard prompts re-auth.

BEGIN;
ALTER TABLE connections DROP COLUMN exchanged_token;
COMMIT;
