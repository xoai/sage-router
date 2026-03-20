# Sage-Router: Architecture Decision Record

> This document records every significant design decision, the reasoning behind it,
> and the alternatives we considered. It is the single source of truth for "why"
> questions about the project's architecture.
>
> Status: **Living document** — updated as decisions are made or revised.
> Last updated: 2026-03-17 (revision 2)

---

## Table of Contents

1. [Project Philosophy](#1-project-philosophy)
2. [Competitive Positioning](#2-competitive-positioning)
3. [Decided: Runtime & Language](#3-decided-runtime--language)
4. [Decided: Core Architecture Pattern](#4-decided-core-architecture-pattern)
5. [Decided: Canonical Type System](#5-decided-canonical-type-system)
6. [Decided: Translation Architecture](#6-decided-translation-architecture)
7. [Decided: Claude Translator Strategy](#7-decided-claude-translator-strategy)
8. [Decided: Storage Engine](#8-decided-storage-engine)
9. [Decided: Provider Connection State Machine](#9-decided-provider-connection-state-machine)
10. [Decided: Translator Rewrite (Not Transpile)](#10-decided-translator-rewrite-not-transpile)
11. [Decided: Cost Optimization Module](#11-decided-cost-optimization-module)
12. [Decided: MITM Proxy as Opt-In](#12-decided-mitm-proxy-as-opt-in)
13. [Decided: No Cloaking — Ethical Proxy](#13-decided-no-cloaking--ethical-proxy)
14. [Decided: Plugin System — Compile-Time](#14-decided-plugin-system--compile-time)
15. [Decided: Dashboard Architecture](#15-decided-dashboard-architecture)
16. [Decided: Security Model](#16-decided-security-model)
17. [Decided: Distribution Model](#17-decided-distribution-model)
18. [Decided: Cloud Sync Deferred to v2](#18-decided-cloud-sync-deferred-to-v2)
19. [Decided: Non-Chat Endpoints Strategy](#19-decided-non-chat-endpoints-strategy)
20. [Decided: OAuth & Provider Auth Strategy](#20-decided-oauth--provider-auth-strategy)
21. [Decided: Claude Auth — Transparent Proxy with Disclosure](#21-decided-claude-auth--transparent-proxy-with-disclosure)
22. [Decided: Graceful Shutdown](#22-decided-graceful-shutdown)
23. [Open: Cross-Platform Credential Detection](#23-open-cross-platform-credential-detection)
24. [Open: Mid-Stream Failure Handling](#24-open-mid-stream-failure-handling)
25. [Open: Build Pipeline — Node.js Dependency](#25-open-build-pipeline--nodejs-dependency)
26. [Open: Skip/Bypass Pattern Configuration](#26-open-skipbypass-pattern-configuration)
27. [Reference: Design Principles](#27-reference-design-principles)

---

## 1. Project Philosophy

Sage-router exists to prove that a developer tool can be **powerful without being heavy,
and simple without being simplistic.**

### Core Beliefs

**Lightweight means fewer moving parts, not fewer features.**
We don't cut features to reduce size. We choose the right foundation so that features
are cheap to add. A single Go binary with embedded SQLite and an embedded dashboard
delivers more features in 15MB than a Node.js app does in 200MB.

**Quality is a decision, not a phase.**
Code quality isn't something we "add later." Every module has clear interfaces,
every translator has golden-file tests, every state transition is explicit. We'd
rather ship 5 providers with bulletproof translators than 15 providers with subtle bugs.

**Developer experience is the product.**
Sage-router's users are developers. They notice latency, they notice clumsy UX,
they notice unnecessary steps. Every interaction — from `curl | sh` installation
to the first routed request — should feel like the tool respects their time.

**Honest engineering over clever hacks.**
We don't impersonate first-party clients. We don't inject fake billing headers.
We don't rely on undocumented endpoints that could break tomorrow. When provider
policies restrict OAuth usage, we disclose that honestly and let users decide.

### What We're Building

A local AI API gateway that:
- Installs in under 3 seconds, runs as a single binary
- Exposes an OpenAI-compatible /v1 endpoint
- Routes requests across 40+ providers with automatic format translation
- Falls back transparently when accounts hit rate limits or quotas
- Tracks usage and cost across all providers
- Actively optimizes cost (prompt caching, budget awareness)
- Provides a fast, information-dense dashboard for management

### What We're NOT Building

- A hosted/cloud AI gateway (we're local-first)
- A general-purpose API gateway or service mesh
- A tool that requires users to compromise on security for convenience
- A tool that relies on gray-area exploits for its core value

---

## 2. Competitive Positioning

### Why sage-router exists alongside LiteLLM and OpenRouter

**LiteLLM** (Python, 30K+ stars) is the closest competitor. It's a mature proxy
with 100+ provider support. **OpenRouter** is a hosted solution that aggregates
providers behind a single API.

Sage-router is differentiated in three ways that matter to its target users:

**1. Single binary, zero dependencies, local-first.**
LiteLLM requires Python + pip + dependencies. OpenRouter requires an internet
connection and sends your credentials to a cloud service. Sage-router is a
15MB binary that runs anywhere. `curl | sh` and you're routing in 3 seconds.
No runtime, no package manager, no cloud account.

**2. Multi-account rotation with intelligent fallback.**
LiteLLM has basic fallback (try model A, then model B). Sage-router has
per-account exponential backoff with model-scoped locks, combo-level cascading,
and a formal state machine. If Account A on Claude hits a rate limit on
sonnet-4, sage-router knows to try Account B for sonnet-4 but can still use
Account A for haiku — and will try Account A again for sonnet-4 after the
backoff expires. This granularity matters for users with multiple accounts.

**3. Cost optimization as a first-class feature.**
LiteLLM tracks cost. Sage-router actively reduces it. Prompt cache injection,
cost estimates before execution, budget alerts, and (in v2) batch API routing
and request deduplication. The cost module isn't bolted on — it's a pipeline
stage that runs on every request.

**Secondary differentiators:**
- Encrypted credential storage (LiteLLM stores in plaintext)
- Built-in dashboard embedded in the binary (LiteLLM's UI is a separate service)
- Auto-detection of local CLI tool credentials (frictionless onboarding)
- Beautiful, information-dense "Terminal Luxe" dashboard UX

### Target users

- **Solo developer** with 2-3 AI subscriptions who wants one endpoint
- **Small team** sharing provider accounts with usage visibility
- **Cost-conscious developer** who wants automatic fallback from paid → free tiers

---

## 3. Decided: Runtime & Language

**Decision:** Go (1.22+)

**Rationale:**
Go gives us a single statically-linked binary with zero runtime dependencies.
`net/http` is production-grade. Goroutines handle concurrent streaming naturally.
The stdlib covers 90% of our needs. Cross-compilation is trivial. Cold start
is <100ms vs seconds for Node.js.

The main cost is rewriting 9router's JS translators in Go. We accept this cost
because Go's type system catches format translation bugs at compile time, and
the rewritten translators will be faster for JSON parsing.

---

## 4. Decided: Core Architecture Pattern

**Decision:** Pipeline architecture with 10 composable stages.

```
① Ingress → ② Authenticate → ③ Resolve → ③b CostRoute →
④ Select → ⑤ Translate → ⑤b CacheHints → ⑥ Execute →
⑦ StreamTranslate → ⑧ Track
```

**Rationale:**
Each stage is a function with a single responsibility. They communicate through
a shared `RequestContext` struct. Stages are independently testable.

Fallback (retry with different account) jumps from ⑥ back to ④.
Combo fallback (try different model) jumps from ⑥ back to ③.

---

## 5. Decided: Canonical Type System

**Decision:** Sage-router owns a canonical intermediate representation (`pkg/canonical/`)
that is OpenAI-shaped today but independent of any provider.

**Key invariants:**

1. **Content is ALWAYS an array, never a bare string.**
2. **Tool call arguments are ALWAYS a JSON string, never a parsed object.**
3. **System prompt is a dedicated field, not messages[0].**
4. **Union semantics, not intersection.** (Thinking, documents, etc. always present.)
5. **Flat tagged union for Content blocks, with constructor functions.**
6. **Document/PDF support included from day one.**

---

## 6. Decided: Translation Architecture

**Decision:** Hub-and-spoke with sage-owned canonical as the hub.
Each provider implements the `Translator` interface with `ToCanonical()`
and `FromCanonical()` methods. Registered at compile time via `init()`.

---

## 7. Decided: Claude Translator Strategy

**Decision:** Full rewrite in Go with golden-file test suite.
18 request test cases + 8 stream test cases minimum.
Every bug discovered becomes a permanent test fixture.

---

## 8. Decided: Storage Engine

**Decision:** Embedded SQLite with WAL mode. Pure Go implementation.
Secrets encrypted at rest (AES-256-GCM). No ORM — typed query helpers.
Schema migrations embedded in binary.

---

## 9. Decided: Provider Connection State Machine

**Decision:** Explicit state machine (Idle → Active → RateLimited → Cooldown →
AuthExpired → Refreshing → Errored → Disabled). Every transition is a table lookup.
Exponential backoff for rate limits. Model-scoped locks.

---

## 10. Decided: Translator Rewrite (Not Transpile)

**Decision:** Rewrite all translators in Go from scratch, using 9router as reference.
Golden-file tests with real request/response pairs ensure behavioral equivalence.

---

## 11. Decided: Cost Optimization Module

**Decision:** Dedicated `internal/cost/` module at two pipeline touch-points.

- **Stage ③b** (after resolve): routing-level decisions (batch, dedup, budget alerts)
- **Stage ⑤b** (after translate): format-level injections (cache_control, breakpoints)

v1 scope: Prompt caching only. Batch API and deduplication are v2.
Keeps translate stage pure (format conversion only).

---

## 12. Decided: MITM Proxy as Opt-In

**Decision:** Ship MITM as opt-in (`--enable-mitm`). Clear warnings when activated.

---

## 13. Decided: No Cloaking — Ethical Proxy

**Decision:** Sage-router will NOT implement request cloaking, fake billing headers,
or first-party client impersonation.

We do not inject fake client identity headers. We do not pretend to be Claude Code,
Codex, or any other tool. If a provider can detect that traffic is coming through
a proxy, that's fine — we don't hide.

**This does not mean we refuse to route traffic from these tools.** It means we
don't deceive providers about the nature of the traffic. The distinction between
"honest proxy" and "no proxy" is important — see §21 for the Claude-specific
auth approach.

---

## 14. Decided: Plugin System — Compile-Time

**Decision:** Compile-time registration via Go `init()`. No runtime plugin loading.

---

## 15. Decided: Dashboard Architecture

**Decision:** Preact SPA (~45KB gzipped) embedded in Go binary. "Terminal Luxe"
aesthetic. Dark mode default. 6 pages. Command palette (Cmd+K). Keyboard-first.

---

## 16. Decided: Security Model

**Decision:** Fix every security gap from 9router: random first-run password,
derived HMAC secret, AES-256-GCM at rest, restricted file permissions,
rate-limited login, automatic secret redaction in logs.

---

## 17. Decided: Distribution Model

**Decision:** Single binary for 5 platform targets. Channels: curl installer,
brew, go install, Docker (FROM scratch), npx wrapper.

---

## 18. Decided: Cloud Sync Deferred to v2

**Decision:** No cloud sync in v1. Local-first perfection. Multi-device sync
comes in v2 with a user-hostable protocol.

---

## 19. Decided: Non-Chat Endpoints Strategy

**Decision:** Three request flows:
1. **Chat flow** (POST /v1/chat/completions, /v1/messages): full pipeline
2. **Responses flow** (POST /v1/responses): normalize input[] → messages[] at ingress
3. **Passthrough flow** (GET /v1/models, etc.): credential injection + proxy

---

## 20. Decided: OAuth & Provider Auth Strategy

**Decision:** Hybrid auth with per-provider strategy based on OAuth research (March 2026).

### Research findings

The OAuth landscape shifted dramatically in January-February 2026:
- **Anthropic** banned third-party OAuth (server-side enforcement + account bans + legal requests)
- **OpenAI** explicitly endorsed third-party OAuth (partnered with OpenCode)
- **Google Gemini** offers generous free tier via API key (60 req/min, 1,000/day)

### Provider-specific auth paths

| Provider | Primary Auth | Secondary Auth | One-Click |
|----------|-------------|----------------|-----------|
| OpenAI / Codex | OAuth device code | API Key | ✅ |
| Google Gemini | API Key (free tier) | Google OAuth (paid) | ✅ |
| Anthropic / Claude | API Key | Auto-detect Claude Code credentials | ✅ |
| GitHub Copilot | Fine-grained PAT | — | ⚠️ Semi |
| OpenRouter | API Key | — | ❌ |
| GLM / Kimi / MiniMax | API Key | — | ❌ |
| Ollama | None (local) | — | ✅ |

### Auth type system

```go
type AuthType string
const (
    AuthAPIKey     AuthType = "apikey"
    AuthOAuth      AuthType = "oauth"
    AuthDeviceCode AuthType = "device_code"
    AuthAutoDetect AuthType = "auto_detect"  // Read from local CLI tool credentials
    AuthNone       AuthType = "none"
)
```

Each provider declares supported auth types. Dashboard renders appropriate UI.
If a provider's OAuth gets blocked, we flip to API key — one config change.

### Onboarding priority

Lead with the easiest path for new users:
1. Google Gemini (free API key, no credit card)
2. OpenAI (OAuth officially supported, or API key)
3. Anthropic (API key, or auto-detect Claude Code credentials)
4. All others (API keys)

### OAuth client registration

Only one registration needed: OpenAI (for Codex device code flow).
Research needed on whether to register our own app or use published public
client credentials.

---

## 21. Decided: Claude Auth — Transparent Proxy with Disclosure

**Decision:** Support Claude connections via both API Key (recommended, fully compliant)
AND auto-detected Claude Code credentials (opt-in, disclosed, user responsibility).

### Context

Anthropic's TOS states OAuth tokens from Free/Pro/Max plans are "intended exclusively
for Claude Code and Claude.ai." However, sage-router can serve as a transparent
proxy between Claude Code (the legitimate client) and Claude's API, adding routing,
fallback, and usage tracking.

This is architecturally different from tools that replace Claude Code. We sit
between Claude Code and the API — Claude Code remains the client.

### The approach

**No cloaking. No impersonation. Full disclosure. Seamless UX.**

1. **Auto-detect Claude Code credentials** from local filesystem (`~/.claude/`
   on macOS/Linux, platform-appropriate path on Windows). Zero user effort when
   Claude Code is already installed and authenticated.

2. **Mandatory disclosure** before connection is activated:
   - Clear explanation that Anthropic's TOS restricts OAuth to Claude Code/Claude.ai
   - Statement that Anthropic does not officially support this configuration
   - User must acknowledge with a checkbox
   - Recommendation to use API Key for full compliance shown alongside

3. **API Key is always the default and recommended path.** The auto-detect option
   is presented as "Also detected: Claude Code credentials" — secondary, not primary.

4. **No fake headers.** We never inject client identity headers that claim to be
   Claude Code. If Anthropic can distinguish proxy traffic from direct traffic,
   we accept that.

5. **Credential file watching.** When Claude Code refreshes its tokens (which it
   does automatically), sage-router picks up the new tokens via filesystem
   watching. Connection stays alive with zero user intervention.

6. **Graceful fallback.** If auto-detection fails (Claude Code not installed,
   tokens expired, credentials in keychain instead of file), silently fall back
   to "Paste API Key" with no error shown — just the API Key input field.

### UX flow

```
User clicks "Connect Claude"
  ↓
Sage-router checks for Claude Code credentials on disk
  ├── Found → Show card: "✓ Claude Code detected · user@gmail.com"
  │           + disclosure checkbox + [Connect] / [Use API Key Instead]
  └── Not found → Show: "API Key" input field
                  + small note: "Or install Claude Code first"
```

Two clicks for the happy path. Zero typing. Full transparency.

### Precedent

OpenCode (112K+ stars) maintains the same feature with a disclaimer that
Anthropic doesn't officially support it. Multiple tools (aider, LiteLLM) read
local credentials from CLI tools. This pattern is established in the ecosystem.

### Risk management

If Anthropic objects or changes enforcement:
- The feature is a configuration flag, not an architectural dependency
- API Key auth works independently and is always available
- We can disable auto-detect with a single flag change in a patch release
- Our system loses a convenience feature, not a core capability

---

## 22. Decided: Graceful Shutdown

**Decision:** Standard Go graceful shutdown with 30-second drain timeout.

On SIGTERM or SIGINT:
1. Stop accepting new connections immediately
2. Wait up to 30 seconds for in-flight streaming requests to complete
3. Force-close remaining connections after timeout
4. Log count of interrupted streams
5. Flush usage tracker batch (ensure pending usage data is written to SQLite)
6. Close database connections

**Why 30 seconds:** LLM streaming responses can run 60+ seconds for long
generations. 30 seconds is a reasonable compromise — most in-progress responses
will complete, but we don't hang indefinitely. Docker's default SIGTERM timeout
is 10s; our Dockerfile and docs will recommend `--stop-timeout 30`.

---

## 23. Open: Cross-Platform Credential Detection

**Status:** Decide during Phase 2 (auth module implementation).

**The challenge:**
Auto-detecting CLI tool credentials varies by platform and tool:

| Tool | macOS | Linux | Windows |
|------|-------|-------|---------|
| Claude Code | `~/.claude/` or Keychain | `~/.claude/` | `%APPDATA%\claude\` |
| Codex | `~/.codex/auth.json` or Keychain | `~/.codex/auth.json` | OS credential store |
| Copilot | macOS Keychain (`copilot-cli`) | libsecret or `~/.copilot/config.json` | Windows Credential Manager |

**Design direction:** `internal/auth/detect/` package with per-provider,
per-platform credential discovery. Filesystem paths first (always works),
keychain integration as enhancement. If detection fails, fall back silently
to "paste your key" — never show a broken error.

**Testing:** CI must test credential detection on all three platforms.

---

## 24. Open: Mid-Stream Failure Handling

**Status:** Accepted for v1. Revisit post-v1 based on tracked failure rate.

---

## 25. Open: Build Pipeline — Node.js Dependency

**Status:** Decide during Phase 4 (dashboard build).

**Leading option:** CI builds dashboard. Local Go builds use placeholder if
dist/ doesn't exist. `make dashboard` target for UI contributors.

---

## 26. Open: Skip/Bypass Pattern Configuration

**Status:** Implement during Phase 3.

Pre-pipeline filter stage. Known patterns (Claude Code title generation,
warmup requests) return canned responses. Configurable via settings.

---

## 27. Reference: Design Principles

These principles resolve ambiguity when making implementation decisions.

### On code structure

**"If you understand the 10 stages, you understand the system."**
The pipeline is the backbone. Every feature is a stage or a modification to a stage.

**"One boundary, one conversion."**
Format-specific transforms happen at the translator boundary. Inside the pipeline,
everything is canonical.

**"Golden files are the specification."**
The translator test suite is the source of truth for format behavior. Every bug
fix adds a test fixture. The suite grows monotonically.

### On architecture

**"Explicit over implicit."**
State machines over ad-hoc fields. Pipeline stages over layered indirection.
Constructor functions over bare struct literals.

**"Separate concerns by rate of change."**
Provider formats change monthly. Auth changes yearly. Pipeline structure changes rarely.

**"Design for the common case, handle the edge case."**
95% of requests are streaming chat completions. Optimize for this.

### On user experience

**"Respect the developer's time."**
Zero-config start. Auto-detect what we can. Every click accomplishes something.

**"Information density over visual flourish."**
Status dots over text badges. Monospace for data. No wasted space.

**"Fail visibly and recoverably."**
Show what broke, why, and what the user can do. Never silent failures.

### On ethics

**"Transparent proxy."**
No cloaking, no impersonation. Full disclosure when policies are ambiguous.
Users make informed decisions. We provide honest information and seamless UX.

---

## Appendix: Decision Log

| Date | Decision | Section |
|------|----------|---------|
| 2026-03-17 | Go as runtime language | §3 |
| 2026-03-17 | Pipeline architecture (10 stages) | §4 |
| 2026-03-17 | Canonical type system (6 invariants) | §5 |
| 2026-03-17 | Hub-and-spoke translation | §6 |
| 2026-03-17 | Claude translator rewrite + golden files | §7 |
| 2026-03-17 | SQLite + WAL + encrypted secrets | §8 |
| 2026-03-17 | Provider connection state machine | §9 |
| 2026-03-17 | Rewrite translators (not transpile) | §10 |
| 2026-03-17 | Cost optimization module (2 pipeline stages) | §11 |
| 2026-03-17 | MITM proxy as opt-in | §12 |
| 2026-03-17 | No cloaking — ethical proxy | §13 |
| 2026-03-17 | Compile-time plugin system | §14 |
| 2026-03-17 | Preact SPA dashboard, "Terminal Luxe" | §15 |
| 2026-03-17 | Security model (fix all 9router gaps) | §16 |
| 2026-03-17 | Single binary + multi-channel distribution | §17 |
| 2026-03-17 | Cloud sync deferred to v2 | §18 |
| 2026-03-17 | Three request flows (chat, responses, passthrough) | §19 |
| 2026-03-17 | OAuth research: hybrid auth per provider | §20 |
| 2026-03-17 | Claude: transparent proxy + mandatory disclosure | §21 |
| 2026-03-17 | Graceful shutdown (30s drain) | §22 |
| 2026-03-17 | Competitive positioning documented | §2 |
