# 9Router — Deep Architecture & Code Analysis

**Project**: 9Router v0.3.54 — A local AI routing gateway  
**Codebase**: ~60,475 lines of JavaScript across ~200 files  
**Stack**: Next.js 16 + React 19 + lowdb + Zustand + Tailwind CSS 4  
**License**: MIT  

---

## 1. Executive Overview

9Router is an **open-source, locally-running AI API gateway** that solves a real developer pain point: managing multiple AI provider accounts, quotas, and formats from a single endpoint. It runs on `localhost:20128`, exposes an **OpenAI-compatible `/v1/*` API surface**, and intelligently routes requests across 40+ AI providers with automatic format translation, credential management, multi-account round-robin, and tiered fallback.

**Core Value Proposition**: A developer configures Claude Code, Cursor, Codex CLI, or any OpenAI-compatible tool to point at `http://localhost:20128/v1`. 9Router then transparently routes requests through a priority chain — subscription-tier providers first, then cheap providers, then free providers — ensuring zero downtime even when quotas run out.

### What Makes It Interesting

This isn't just a proxy. It's a **protocol translator + credential manager + load balancer + observability layer** rolled into a single Next.js process. The key engineering challenge is the format translation matrix: a Claude-format request arriving from Claude Code must be translated to Gemini format to hit a Gemini-CLI account, then the Gemini-format SSE stream must be translated back to Claude format — all in real-time streaming.

---

## 2. Architecture Deep Dive

### 2.1 System Context

The system has four boundaries:

1. **Client boundary**: CLI tools (Claude Code, Codex, Cursor, Cline, etc.) and the browser dashboard
2. **Gateway boundary**: The Next.js process that routes, translates, and manages credentials
3. **Upstream boundary**: 40+ AI providers with different API formats
4. **Cloud boundary**: Optional cloud sync service for multi-device state sharing

### 2.2 Layered Architecture

The codebase is organized into six distinct layers:

| Layer | Location | Responsibility |
|-------|----------|----------------|
| **API Surface** | `src/app/api/v1/*` | OpenAI/Claude/Gemini-compatible endpoints |
| **Dashboard API** | `src/app/api/*` | CRUD for providers, keys, combos, settings |
| **Routing Core** | `src/sse/handlers/chat.js` | Model resolution, combo handling, account selection loop |
| **Execution Core** | `open-sse/handlers/chatCore.js` | Format detection → translation → executor dispatch → stream handling |
| **Provider Executors** | `open-sse/executors/*` | Provider-specific HTTP, auth, URL construction |
| **Persistence** | `src/lib/localDb.js`, `src/lib/usageDb.js` | JSON file-based state and usage tracking |

The split between `src/` and `open-sse/` is architecturally significant. `open-sse/` is the **portable, framework-agnostic core** — it contains translators, executors, streaming utilities, and config. `src/` contains the **Next.js-specific integration**: route handlers, local DB adapters, dashboard UI, and auth. This separation means the routing core could theoretically be deployed as a Cloudflare Worker (there's evidence of this in the `cloud/` directory and `isCloud` guards throughout the code).

### 2.3 The Translation Hub Pattern

The most sophisticated part of the architecture is the **hub-and-spoke translation system**. Rather than implementing N×M translators for every source→target format pair, 9Router uses **OpenAI format as the universal hub**:

```
Source Format → [OpenAI Hub] → Target Format
```

This means:
- Each new source format needs only 1 translator (to OpenAI)
- Each new target format needs only 1 translator (from OpenAI)
- Adding a new format costs O(1) work, not O(N)

**Current format matrix** (10 request translators × 8 response translators):

| Source → Hub (Request) | Hub → Target (Request) |
|------------------------|------------------------|
| Claude → OpenAI | OpenAI → Claude |
| Gemini → OpenAI | OpenAI → Gemini |
| Antigravity → OpenAI | OpenAI → Kiro |
| OpenAI Responses → OpenAI | OpenAI → Cursor |
| | OpenAI → Ollama |

---

## 3. Core Execution Flow

### 3.1 Request Lifecycle (The Critical Path)

When a POST hits `/v1/chat/completions`:

1. **Next.js rewrite**: `/v1/*` → `/api/v1/*` (via `next.config.mjs`)
2. **Route handler**: Parses JSON body, extracts model string
3. **API key validation**: If `requireApiKey` setting is enabled
4. **Model resolution**: `getModelInfo()` parses `provider/model` or resolves aliases
5. **Combo check**: If model name matches a combo, enter `handleComboChat()` loop
6. **Credential selection**: `getProviderCredentials()` with priority-ordered round-robin
7. **Token refresh pre-check**: `checkAndRefreshToken()` for OAuth providers
8. **Format detection**: Source format from endpoint + body shape; target format from provider config
9. **Request translation**: Source → OpenAI → Target (via translator registry)
10. **Executor dispatch**: Provider-specific `execute()` — builds URL, headers, transforms body
11. **Upstream call**: `proxyAwareFetch()` with optional SOCKS/HTTP proxy support
12. **401/403 handling**: Auto-refresh credentials and retry once
13. **Response stream translation**: Target → OpenAI → Source (chunk-by-chunk)
14. **Usage tracking**: Extract token counts, persist to `usage.json` and `log.txt`

### 3.2 Fallback Strategy

The fallback system operates at **two levels**:

**Account-level fallback** (within a single provider):
- If Account A returns 429/401/403/5xx → mark it with exponential backoff cooldown → try Account B
- Cooldowns: 1s base, doubling up to 2 min max, with 15 backoff levels
- Model-level locks: A 429 on `claude-sonnet-4` doesn't block `claude-haiku`

**Combo-level fallback** (across providers):
- User defines a "combo" like `my-combo = [cc/claude-sonnet-4, ag/claude-sonnet-4, if/qwen-coder]`
- If Model 1 fails with fallback-eligible error → try Model 2 → try Model 3
- Only non-fallback errors (like 400 Bad Request) stop the chain

This two-level strategy means a single request can try: Account A of Provider 1 → Account B of Provider 1 → Provider 2 Account A → Provider 3 Account A — all transparently.

### 3.3 MITM Proxy (Advanced Feature)

The `src/mitm/` module implements a **man-in-the-middle HTTPS proxy** for intercepting traffic from tools that don't support custom API endpoints. It:

- Generates a local Root CA certificate
- Dynamically creates per-domain TLS certificates via SNI callback
- Intercepts `cloudcode-pa.googleapis.com` and `api.individual.githubcopilot.com`
- Redirects intercepted requests to `localhost:20128/v1/chat/completions`
- Bypasses its own MITM for outbound requests via Google DNS resolution (`8.8.8.8`)

This is how 9Router can route Gemini CLI and GitHub Copilot traffic without those tools knowing.

---

## 4. Key Design Patterns

### 4.1 Executor Pattern (Strategy)
Each provider gets a `BaseExecutor` subclass. The base handles retry logic (429 with fixed delay, URL fallback) and the subclass overrides `buildUrl()`, `buildHeaders()`, `transformRequest()`, `refreshCredentials()`. This is clean Strategy pattern.

**Specialized executors**: Antigravity, Gemini-CLI, GitHub, iFlow, Kiro, Codex, Cursor, Vertex  
**Default executor**: All other providers (including custom compatible nodes)

### 4.2 Singleton DB with Read-Through
`localDb.js` uses lowdb with a singleton pattern but **reads from disk on every access** (`await dbInstance.read()`). This prevents stale data across Next.js route workers but trades off performance. For a local tool with low concurrency, this is an acceptable tradeoff.

### 4.3 Lazy Initialization
The translator registry uses `ensureInitialized()` with a `require()` pattern (not dynamic `import()`) for bundler compatibility. All translators self-register on first use.

### 4.4 Stream Controller Pattern
`createStreamController()` wraps abort signals, disconnect detection, and cleanup into a unified object. This handles the common pitfall of SSE connections where the client disconnects mid-stream.

---

## 5. Dependencies Analysis

### Runtime Dependencies (Notable)

| Package | Purpose | Risk Assessment |
|---------|---------|-----------------|
| `next@16.1.6` | App framework | Heavy for a local proxy; brings React SSR overhead |
| `lowdb@7.0.1` | JSON file DB | No concurrency protection; fine for single-user local tool |
| `better-sqlite3@12.6.2` | SQLite binding | Listed but usage unclear — may be for future migration |
| `node-forge@1.3.3` | MITM cert generation | Security-sensitive; generates root CA |
| `selfsigned@5.5.0` | Self-signed certs | Used alongside node-forge for MITM |
| `undici@7.19.2` | HTTP client (proxy) | ProxyAgent for SOCKS/HTTP proxy support |
| `jose@6.1.3` | JWT handling | Dashboard auth tokens |
| `bcryptjs@3.0.3` | Password hashing | Dashboard login |
| `express@5.2.1` | HTTP server | Unclear why needed alongside Next.js |
| `socks-proxy-agent@8.0.5` | SOCKS proxy | For users behind firewalls |

### Dependency Concerns

1. **`express` alongside Next.js**: Possibly for the MITM server or a legacy integration path. Having two HTTP frameworks is unusual.
2. **`better-sqlite3` unused?**: Listed in `package.json` but I found no SQLite usage in the routing core. May be dead weight or for the cloud module.
3. **`http-proxy-middleware`**: Likely for the dashboard proxy guard (`src/proxy.js`) — another redundancy concern with Next.js rewrites.

---

## 6. Security Assessment

### Strengths

- **Local-first**: All credentials stay on the user's machine in `~/.9router/db.json`
- **API key format with CRC**: `sk-{machineId}-{keyId}-{crc8}` with HMAC-SHA256 integrity check
- **Dashboard auth**: JWT-based cookie auth with bcrypt password hashing
- **Proxy-aware fetch**: Respects `NO_PROXY`, supports SOCKS5, has strict-proxy mode

### Weaknesses & Risks

1. **Default password `123456`**: The `INITIAL_PASSWORD` env var defaults to `123456`. On a shared network, this is a credential exposure vector. The dashboard controls all AI provider tokens.

2. **Secrets in plain JSON**: Provider access tokens, refresh tokens, and API keys are stored unencrypted in `db.json`. Anyone with filesystem access can read them. No at-rest encryption.

3. **HMAC secret hardcoded**: `API_KEY_SECRET` defaults to `"endpoint-proxy-api-key-secret"` if the env var isn't set. This means all default-config installations share the same HMAC key — an attacker can forge valid API keys.

4. **MITM Root CA**: The MITM proxy generates a Root CA that the user must trust system-wide. If the `~/.9router/mitm/rootCA.key` is compromised, an attacker can intercept all HTTPS traffic on that machine.

5. **`rejectUnauthorized: false`**: The MITM bypass path in `proxyFetch.js` disables TLS verification for googleapis.com connections. This is necessary for the MITM architecture but weakens the security posture.

6. **Request logging sensitivity**: When `ENABLE_REQUEST_LOGS=true`, full headers and bodies (including tokens) are written to `logs/`. The ARCHITECTURE.md correctly notes this but there's no automatic redaction.

7. **No rate limiting on dashboard API**: The management endpoints (`/api/providers`, `/api/settings`) have no rate limiting or brute-force protection beyond the login password.

---

## 7. Performance Considerations

### Strengths

- **Streaming-first**: SSE passthrough means time-to-first-token is only marginally impacted by the proxy layer
- **DNS caching**: `proxyFetch.js` caches DNS resolutions for 5 minutes
- **Proxy dispatcher pooling**: Reuses `undici.ProxyAgent` instances with LRU eviction (max 20)
- **Skip patterns**: Certain requests (like Claude Code's title generation prompts) are detected and short-circuited

### Bottlenecks

1. **Read-through DB on every request**: `getDb()` calls `dbInstance.read()` (disk I/O) on every API call. For a local tool with <10 concurrent requests, this is fine. At scale, it would be a chokepoint.

2. **Single-threaded JSON write**: `lowdb` writes are serialized. Under concurrent requests, writes to `db.json` and `usage.json` contend for the file lock.

3. **Next.js overhead**: Running a full React SSR framework for what is primarily an API proxy adds ~200MB memory baseline and slower cold starts compared to a lean HTTP server.

4. **Translation cost**: Every streaming chunk goes through `translateResponse()` which does two Map lookups and function calls. For a 100K-token response with hundreds of chunks, this is negligible but measurable.

---

## 8. Strengths & Weaknesses

### Strengths

1. **Solves a real problem well**: The multi-provider routing with automatic fallback genuinely removes friction from AI-assisted development workflows. The tiered subscription → cheap → free strategy is pragmatic.

2. **Hub-and-spoke translation is elegant**: The OpenAI-as-hub approach scales linearly with new formats. Adding a new provider format requires 2 files (request + response translator), not N×M modifications.

3. **Comprehensive provider coverage**: 10+ specialized executors, 10 request translators, 8 response translators. Support for OAuth, API keys, device-code flows, and compatible node endpoints.

4. **Thoughtful fallback mechanics**: Exponential backoff with model-level locks, account-level cooldowns, combo-level cascading. The `accountFallback.js` module is well-structured with clear separation of concerns.

5. **Portable core**: The `open-sse/` module is framework-agnostic. The `isCloud` guards show intent to run on Cloudflare Workers. This is good architecture for future deployment flexibility.

6. **Developer UX**: Auto-opening dashboard, `npm install -g 9router` single command setup, OAuth flows that work from the browser dashboard, clipboard-friendly API keys.

7. **Observability built in**: Usage tracking with token counts, cost estimation, request logs, and per-provider success/failure history. The dashboard surfaces this data.

### Weaknesses

1. **Next.js is over-engineered for this use case**: A local API proxy doesn't need React SSR, webpack compilation, or the full Next.js middleware chain. A Fastify/Hono/Express server with a separate static dashboard would be 5× lighter. The ~2M `src/app` directory is mostly UI code that bloats the core routing path.

2. **No test coverage for critical paths**: The `tests/` directory exists with `vitest.config.js` but the unit tests are sparse relative to the 60K LoC codebase. The combo fallback logic, translator correctness, and account rotation are all under-tested for how critical they are.

3. **Security defaults are dangerous**: Default password `123456`, hardcoded HMAC secret, plaintext credential storage, and MITM CA generation create a wide attack surface for a tool that manages AI provider tokens worth real money.

4. **JSON file DB won't scale**: `lowdb` is excellent for prototypes but has no transactions, no concurrent write safety, and no query optimization. If usage grows (multi-user, high-throughput), this will need migration. The presence of `better-sqlite3` in `package.json` suggests this is known.

5. **Tight coupling to provider quirks**: The executor layer has many provider-specific heuristics (e.g., the `ccFilterNaming` flag, Antigravity project ID cold-miss fix, Codex forced streaming). As providers change their APIs, this creates ongoing maintenance burden.

6. **Cloud sync is under-documented**: The `cloud/` directory and `NEXT_PUBLIC_CLOUD_URL` pattern suggest a proprietary cloud sync service, but its protocol and security model are opaque from the source code. Users are trusting an external service with their provider credentials.

7. **Error handling is inconsistent**: Some paths return `createErrorResult()` (an object), others return `new Response()` directly. The combo handler returns HTTP `406 Not Acceptable` when all models fail, which is semantically incorrect (406 means content negotiation failure, not provider exhaustion).

---

## 9. Metadata

| Metric | Value |
|--------|-------|
| Total LoC (JS/JSX) | ~60,475 |
| API Routes | ~35 route handlers |
| Provider Executors | 10 (9 specialized + 1 default) |
| Request Translators | 10 |
| Response Translators | 8 |
| Supported Formats | 10 (OpenAI, Claude, Gemini, Antigravity, Kiro, Cursor, Codex, Gemini-CLI, OpenAI Responses, Ollama) |
| DB Entities | 8 (connections, nodes, aliases, combos, keys, settings, pricing, proxy pools) |
| Docker Image Base | node:20-alpine (multi-stage build) |
| Default Port | 20128 |
| Persistence | JSON files (~/.9router/) |
| i18n | English, Vietnamese, Chinese |

---

## 10. Key Insights & Open Questions

### Key Insights

1. **The real moat is the translation matrix**: Anyone can build a proxy. Building correct, streaming-compatible, bidirectional translators between 10 API formats — handling tool calls, thinking blocks, system prompts, and edge cases — is months of work. This is 9Router's primary technical asset.

2. **The MITM approach is clever but risky**: Intercepting Gemini CLI and Copilot traffic via DNS hijacking and dynamic cert generation is the only way to route these tools (they don't support custom endpoints). But it requires users to trust a local Root CA, which is a hard sell for security-conscious teams.

3. **The project straddles open-source and commercial**: The npm package is `9router` (public), but the repo package is `9router-app` (private). The cloud sync service is proprietary. This hybrid model works but creates trust concerns around the cloud sync surface.

4. **Vietnam-origin developer tool with global ambition**: Vietnamese README, MoMo-adjacent patterns, but English-first documentation and support for global providers. The i18n support (VI, ZH-CN) targets Asian developer markets specifically.

### Open Questions

1. **Why `better-sqlite3` in dependencies?** Is there a planned migration from lowdb to SQLite? If so, when, and will it break the `db.json` contract?

2. **What does the cloud sync service store?** Provider credentials are synced to `NEXT_PUBLIC_CLOUD_URL` — what's the encryption model? Who operates the default cloud endpoint?

3. **How is the MITM CA distributed securely?** The cert generation is local, but `rootCA.key` is stored in `~/.9router/mitm/` with no special permissions enforcement.

4. **What's the token refresh failure mode?** If both the refresh token and the access token expire simultaneously (e.g., user's laptop was asleep for days), does the system gracefully degrade or silently fail?

5. **What's the `express` dependency for?** It's listed in `package.json` but the core routing uses Next.js API routes. Is it for the MITM server, a legacy artifact, or something else?

6. **How are concurrent `db.json` writes handled?** Next.js can run route handlers in parallel. Two simultaneous writes to lowdb can cause data loss. Is this mitigated?

---

## 11. Suggested Deeper Dives

1. **Translator correctness**: Pick a complex request (multi-turn with tool calls + thinking blocks) and trace it through `openai-to-claude.js` → upstream → `claude-to-openai.js`. Verify edge cases like empty tool results, multi-image content blocks, and streaming think-block boundaries.

2. **OAuth flow security**: Audit the device-code flow in `src/app/api/oauth/` — specifically how tokens are stored post-exchange and whether refresh tokens are rotated.

3. **MITM threat model**: Map out what happens if `rootCA.key` is exfiltrated. Can the attacker intercept non-AI traffic? What's the blast radius?

4. **Cloud sync protocol**: Reverse-engineer the sync payload format from `src/shared/services/cloudSyncScheduler.js` and `src/app/api/sync/cloud/route.js`. Determine what credentials leave the machine.

5. **Performance profiling**: Measure latency overhead of the proxy layer under load (100 concurrent streaming requests). Identify if the lowdb read-through or translation layer is the bottleneck.

6. **Comparison with alternatives**: How does 9Router compare to LiteLLM, OpenRouter, or custom nginx-based proxies? Where does its approach win and lose?
