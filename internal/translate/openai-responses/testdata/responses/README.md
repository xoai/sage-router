# M1 Synthetic Fixtures — OpenAI Responses API

## Why synthetic?

Cycle `20260517-openai-subscription-responses-api` planned to capture these
fixtures from a live `/v1/responses` round-trip (plan §M1). During the
cycle we hit an upstream OpenAI account limitation: the test account's
ChatGPT subscription tier (`prolite`) does not have OAuth-based API
access enabled for any of its workspaces. Both attempted workspaces
(AI Lab and Personal) returned `workspace_restriction` at the authorize
step, blocking the RFC 8693 token-exchange.

The cycle machinery itself was fully validated cross-platform:
- Linux + Windows binaries build cleanly
- Migration 012 applies on boot
- Auto-detect reads codex `auth.json`
- `Flow.ExchangeForAPIKey` form shape matches Codex CLI Rust source
  (`codex-rs/login/src/server.rs::obtain_api_key`) byte-for-byte
- Graceful failure path: warning logged, connection persists with
  empty `ExchangedToken`, M2b.8 pre-flight catches it later

Only the LIVE upstream call was blocked, by upstream account policy.

## Fixture provenance

All JSON + SSE bodies in this directory are hand-constructed from
OpenAI's published API reference at
`https://developers.openai.com/api/reference/resources/responses/methods/create`
(captured 2026-05-17) and the streaming-events reference at
`https://developers.openai.com/api/reference/resources/responses/streaming-events`.

The shapes match what the spec documents. Field ordering inside JSON
objects is the order shown in the official examples.

## Risk acknowledgment

These fixtures are **spec-accurate** but **not live-captured**. Real
upstream responses may include additional fields the spec doesn't
document, or subtle differences in event ordering / sequence_number
allocation. The translator (M3) + streaming translator (M4) built
against these fixtures should be re-validated against live capture
before being marked production-ready.

A follow-up `M1-live-revalidation` cycle should:
1. Re-capture each fixture against a real OpenAI API-enabled account.
2. Diff the captured payload vs the synthetic fixture in this dir.
3. Update the synthetic fixture AND the translator if a discrepancy
   touches a load-bearing path.

## Files

| File | Purpose |
|---|---|
| `single_turn_user.input.json` | Canonical inbound request for M3.2 test |
| `single_turn_user.expected.json` | Expected upstream response body for M3.3 test |
| `multi_turn.input.json` | Multi-turn (user/assistant/user) request shape for M3 |
| `multi_turn.expected.json` | Multi-turn response body for M3 |
| `streaming.raw.sse` | Raw SSE wire bytes for M4 streaming translator test |
| `tier_error.expected.json` | 401 "Missing scopes" body for M3.4 tier-error parser |
