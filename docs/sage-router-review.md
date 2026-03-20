# Sage-Router v0.1 — Code Review & Release Readiness Assessment

**Reviewed:** 8,691 lines of Go across 56 files + Preact dashboard
**Date:** 2026-03-18
**Verdict:** NOT ready for release. Strong foundation, but 5 critical issues must be fixed first.

---

## Overall Assessment

The codebase demonstrates genuine craftsmanship. The module structure follows the ADR faithfully. The canonical types honor all six invariants we specified. The SQLite store has proper WAL mode, embedded migrations, and encryption primitives. The Preact dashboard is built and embedded. The startup banner matches the spec.

But there's a significant gap between the architecture we designed and what's actually wired together. The most serious issue: **the 10-stage pipeline exists as code but isn't connected to anything** — the actual request handling bypasses it entirely with inline logic in `routes_v1.go`. This means the pipeline's testability benefits, stage-level observability, and clean separation of concerns are theoretical, not real.

Below I've categorized every finding by severity.

---

## Critical Issues (Must Fix Before Any Release)

### C1. Zero Test Files

**The entire codebase has no tests.** No `*_test.go` files exist anywhere. The `testdata/` directories (`claude_to_canonical/`, `gemini_to_canonical/`, `stream_chunks/`) are empty.

This directly contradicts ADR §10: *"Golden files are the specification. Every bug fix adds a test fixture. The suite grows monotonically."*

The translators are the highest-risk code. Without tests, we have no confidence that Claude→Canonical→OpenAI or any other translation path produces correct output. Shipping untested translators would undermine the quality commitment that justifies sage-router's existence.

**Fix:** Before release, write at minimum:
- Unit tests for `pkg/canonical/validate.go` (easy, high value)
- Golden-file tests for OpenAI and Claude `ToCanonical`/`FromCanonical` (critical path)
- Unit tests for `provider/selector.go` Select algorithm
- Unit tests for `provider/state.go` CanTransition
- Unit tests for `store/crypto.go` Encrypt/Decrypt round-trip
- Unit tests for `auth/manager.go` password + JWT + API key

### C2. Pipeline Exists But Isn't Used

The `internal/pipeline/` package has 11 files implementing the 10-stage pipeline architecture from the ADR. But `server/routes_v1.go` has its own `handleChatCompletions` → `executeRequest` → `streamResponse` flow that duplicates the logic without using the pipeline at all.

This means:
- Two implementations of model resolution (`pipeline/resolve.go` vs `routes_v1.go:resolveModel`)
- Two implementations of API key extraction (`pipeline/authenticate.go` vs `routes_v1.go:extractAPIKey`)  
- Two implementations of format detection and translation dispatch
- The pipeline's `Run()` method, which checks for cancellation between stages, is never called
- The `RequestContext` from `pipeline/context.go` is never instantiated

**Fix:** Choose one path. Either wire the pipeline into the server handler (preferred — it's the architecture we designed) or remove the pipeline package entirely. Having dead code that looks like the real implementation is worse than not having it. The recommended approach: refactor `handleChatCompletions` to construct a `RequestContext`, build the pipeline with the stage functions, and call `pipeline.Run()`.

### C3. `go.mod` Specifies Nonexistent Go Version

```
go 1.26.1
```

Go 1.26 doesn't exist. Current stable is Go 1.24.x. This will cause `go build` to fail on every machine that has a real Go installation.

**Fix:** Change to `go 1.22` or `go 1.23` (the minimum version that supports all features used, like the `net/http` method routing syntax `"POST /v1/chat/completions"`). The `"METHOD /path"` pattern requires Go 1.22+.

### C4. Plaintext Password Stored in Database

In `cmd/sage-router/main.go`:
```go
func initPassword(db store.Store) string {
    existing, err := db.GetSetting("password")  // ← reads plaintext
    ...
    db.SetSetting("password", password)  // ← stores plaintext
    return password
}
```

The raw password is persisted in the settings table alongside its bcrypt hash. Anyone with read access to the SQLite database can extract the password.

**Fix:** Store only the bcrypt hash. On first run, print the password to terminal (already done), store only the hash, and never read it back. The `initPassword` function should check for the existence of `password_hash`, not `password`.

### C5. Secrets Not Encrypted in Connection Records

The `store/crypto.go` has working AES-256-GCM encryption, but it's never actually called. Connection records store `access_token`, `refresh_token`, and `api_key` as plaintext in SQLite. The ADR §16 specifies encrypted secrets at rest.

**Fix:** Encrypt `access_token`, `refresh_token`, and `api_key` fields when writing to the database, decrypt when reading. The `DeriveKey` + `Encrypt` + `Decrypt` functions are already implemented — they just need to be integrated into `store/sqlite.go`'s connection CRUD methods.

---

## Significant Issues (Should Fix Before Release)

### S1. Connection State Machine Not Wired to Request Lifecycle

The state machine in `provider/state.go` has a proper transition table, and `provider/connection.go` has `MarkUsed()`, `MarkSuccess()`, etc. But `routes_v1.go:executeRequest` never calls these methods after a successful or failed upstream request. After a 429, the connection should transition to `RateLimited → Cooldown`, but currently it just gets added to `excludeIDs` for that single request — the state is lost after the request completes.

**Fix:** After successful response, call `conn.MarkSuccess()`. After 429/5xx, call the appropriate state transition and persist the cooldown to the in-memory selector. This is what makes the multi-account fallback actually work across requests.

### S2. No Retry Logic in Executor Layer

`internal/executor/retry.go` exists (I assume — it's in the file list) but the `DefaultExecutor.Execute()` makes a single HTTP call with no retry. 9router retries 429s twice with a 2-second delay before falling back to the next account.

**Fix:** Add retry-with-backoff in the executor's `Execute` method for 429 responses, matching the `RETRY_CONFIG` pattern from the ADR.

### S3. Non-Streaming Response Translation Missing

`forwardResponse` in `routes_v1.go` copies the upstream response body directly to the client without format translation:

```go
// For now, forward as-is (translation of non-streaming responses is simpler)
```

This means a request from a Claude-format client routed to an OpenAI provider will receive an OpenAI-format response, which the client can't parse.

**Fix:** Implement non-streaming response translation: parse the upstream response body as the target format, convert to canonical, then convert to the client's source format.

### S4. Dashboard Auth Guard Not Applied

`AuthGuardMiddleware` exists in `middleware.go` but isn't applied to `/api/*` routes in the middleware chain. Any unauthenticated user can hit management endpoints.

**Fix:** Wire `AuthGuardMiddleware` into the handler chain for `/api/*` routes, with exceptions for `/api/auth/login` and `/api/auth/check`.

### S5. ListConnections Sort Direction Wrong

```go
q := "SELECT " + connCols + " FROM connections" + w.sql() + " ORDER BY priority DESC, name ASC"
```

The ADR says lower priority number = higher priority (tried first). This should be `ORDER BY priority ASC`.

### S6. Cost Optimization Stages Not Implemented

Stages ③b (CostRoute) and ⑤b (CacheHints) from the ADR are missing. No `cache_control` injection for Claude targets. The `internal/cost/` package doesn't exist.

**Fix:** For v0.1, at minimum implement cache_control injection in the Claude `FromCanonical` method (add `cache_control: {type: "ephemeral"}` to the last system block and last tool). The full cost module can wait for v0.2.

---

## Minor Issues (Fix When Convenient)

### M1. Binary and node_modules Checked Into Git

`bin/sage-router.exe` (17MB) and `web/dashboard/node_modules/` (23MB) are in the repository. These should be in `.gitignore`. No `.gitignore` file exists.

### M2. Dead Branch in TranslateRequest

In `translate/registry.go`, the `TranslateRequest` method has an if/else that does the same thing in both branches:

```go
if source == canonical.FormatOpenAI {
    srcTranslator, ok := r.Get(source)  // same as else branch
    ...
} else {
    srcTranslator, ok := r.Get(source)  // identical
    ...
}
```

This is harmless but adds noise. Simplify to a single path.

### M3. Request ID Generation Inconsistency

`pipeline/context.go` uses `crypto/rand` for request IDs (secure, unique).
`routes_v1.go` uses `time.Now().UnixNano()` (collisions possible under load).
Since routes_v1.go is the one actually called, requests get weak IDs.

### M4. MITM Module Empty

`internal/mitm/` directory exists with size 4.0K but no Go files. Either add a stub with a "not implemented" message or remove the directory.

### M5. Combo Fallback Doesn't Check Response Status

`handleComboRequest` calls `executeRequest` which writes directly to the ResponseWriter. If the first combo entry fails after writing headers, subsequent entries can't write their own response. The combo handler should detect failure before committing to the response.

---

## What's Done Well

These deserve recognition — they demonstrate the right engineering instincts:

1. **Canonical types are exactly right.** All 6 invariants honored. Constructor functions present. Content is always array. Arguments always JSON string. System is a separate field. The `json:"-"` tag on Meta prevents serialization leakage.

2. **State machine is properly formalized.** Transition table is explicit, `CanTransition` validates moves, `ErrInvalidTransition` is a typed error, `statePriority` makes the selection algorithm readable.

3. **SQLite setup is production-grade.** WAL mode, foreign keys, busy timeout, embedded migrations with tracking table, proper scan helpers.

4. **AES-256-GCM encryption is correct.** Nonce-prefixed ciphertext, proper use of `io.ReadFull` for nonce generation.

5. **SSE reader/writer is clean.** Proper field parsing, trailing event flush on EOF, comment filtering, multi-line data support. This is better than many production SSE implementations.

6. **Graceful shutdown is implemented correctly.** Signal handling, 30-second drain, usage tracker flush, store close — matches ADR §22 exactly.

7. **Dependency count is minimal.** Only 2 direct dependencies (`golang.org/x/crypto`, `modernc.org/sqlite`). This is exactly the "less dependencies" principle in action.

8. **Dashboard is built and embedded.** Preact SPA with the correct page structure (overview, providers, models, usage, settings, connect). Built assets in `dist/`. Proper SPA fallback handler.

9. **Claude translator handles the hard cases.** System prompt parsing (string + array), content block parsing, tool_use input→arguments stringify, tool_result extraction, message merging, alternating role enforcement. The logic matches our deep-dive specification.

10. **Startup banner matches the spec.** Password printed once, clean formatting, correct URLs.

---

## Release Readiness Scorecard

| Area | Status | Notes |
|------|--------|-------|
| Canonical types | ✅ Complete | All invariants, constructors, validation |
| OpenAI translator | ✅ Complete | Request + stream |
| Claude translator | ⚠️ Mostly done | Request complete, stream needs verification |
| Gemini translator | ⚠️ Mostly done | Needs testing |
| Pipeline architecture | ❌ Not wired | Code exists but not connected |
| Provider state machine | ⚠️ Partial | Defined but not used in request lifecycle |
| Account fallback | ⚠️ Basic | Works per-request but state doesn't persist |
| Combo fallback | ⚠️ Basic | Works but can't recover from partial writes |
| SQLite store | ✅ Complete | WAL, migrations, CRUD, encryption primitives |
| Secret encryption | ❌ Not wired | Code exists but not called |
| Auth (password + JWT) | ✅ Complete | Bcrypt, HMAC-SHA256 JWT, API keys |
| Auth guard (dashboard) | ❌ Not wired | Middleware exists but not applied |
| Usage tracking | ✅ Complete | Batch writer, cost calculation |
| Dashboard | ✅ Complete | 6 pages, Preact, embedded |
| Graceful shutdown | ✅ Complete | 30s drain, flush, close |
| Cost optimization | ❌ Not started | No cache_control injection |
| Tests | ❌ Zero | No test files, empty testdata |
| MITM (opt-in) | ❌ Not started | Empty directory |
| Documentation | ✅ Complete | ADR, architecture, deep-dive all included |

**Overall: ~65% of Phase 1-4 work is done. The foundation is solid. The gaps are in wiring and testing, not design.**

---

## Recommended Path to v0.1.0

### Priority 1: Make it actually work (1 week)
1. Fix go.mod version (30 minutes)
2. Wire the pipeline into routes_v1.go OR unify the two paths (2 days)
3. Wire connection state machine to request lifecycle (1 day)
4. Fix plaintext password storage (1 hour)
5. Wire secret encryption for connection credentials (half day)
6. Wire dashboard auth guard (half day)
7. Fix ListConnections sort order (5 minutes)
8. Add .gitignore, remove binary and node_modules from git (30 minutes)

### Priority 2: Make it trustworthy (1 week)
1. Write golden-file tests for OpenAI and Claude translators (2 days)
2. Write unit tests for selector, state machine, crypto, auth (2 days)
3. Test streaming translation end-to-end (Claude→OpenAI, OpenAI→Claude) (1 day)
4. Implement non-streaming response translation (half day)
5. Add basic retry logic in executor (half day)

### Priority 3: Ship it (2-3 days)
1. Add cache_control injection in Claude FromCanonical (half day)
2. Final integration test: Claude Code → sage-router → OpenAI (1 day)
3. README with quick start instructions (half day)
4. Tag v0.1.0

**Estimated time to v0.1.0: 2-3 weeks from current state.**
