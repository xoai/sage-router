# Sage-Router OAuth Research Report

> Research date: 2026-03-17
> Purpose: Determine per-provider auth strategy for sage-router v1
> Status: COMPLETE — ready for decision-making

---

## Executive Summary

The OAuth landscape has fundamentally shifted since 9router was designed.
**Anthropic has locked down Claude subscription OAuth for third-party tools**
with active server-side enforcement and account bans. This single fact
invalidates 9router's core value proposition of "free Claude via OAuth"
and reshapes our entire auth strategy.

The good news: **OpenAI has moved in the opposite direction**, explicitly
endorsing third-party tool access via Codex OAuth. GitHub Copilot provides
fine-grained PATs. Google Gemini CLI offers a generous free tier with
API keys. The ecosystem is larger than Claude alone.

### Provider Classification

| Provider | Auth Path for Sage-Router | One-Click? | Status |
|----------|--------------------------|------------|--------|
| **OpenAI / Codex** | OAuth (ChatGPT) + API Key | ✅ Yes | Explicitly supported |
| **Google Gemini** | API Key (free tier) | ✅ Yes | Free, 60 req/min |
| **GitHub Copilot** | Fine-grained PAT | ⚠️ Semi | PAT generation, then paste |
| **Anthropic Claude** | API Key only | ❌ No OAuth | OAuth banned for 3rd party |
| **OpenRouter** | API Key | ❌ Paste key | Standard API key |
| **GLM / Kimi / MiniMax** | API Key | ❌ Paste key | Standard API key |
| **Ollama** | None (local) | ✅ Yes | No auth needed |

---

## Detailed Findings Per Provider

### 1. Anthropic / Claude Code — BLOCKED

**Finding:** Anthropic explicitly bans third-party OAuth access.

On February 19, 2026, Anthropic updated its legal compliance documentation:

> "Using OAuth tokens obtained through Claude Free, Pro, or Max accounts
> in any other product, tool, or service — including the Agent SDK — is
> not permitted and constitutes a violation of the Consumer Terms of Service."

This is actively enforced:
- Server-side checks deployed January 9, 2026
- Client identity verification rejects non-genuine Claude Code binaries
- Automated account bans for third-party harness usage
- Legal requests sent to OpenCode, resulting in Claude OAuth removal
- OpenCode removed all Claude OAuth code on February 19, 2026

**Impact for sage-router:**
- NO OAuth flow for Claude. Period.
- API Key auth only (via Anthropic Console)
- This is the ethical position we already committed to (no cloaking)

**Dashboard UX:**
```
Claude / Anthropic
  Auth: API Key
  → Get your key from console.anthropic.com
  → [Paste API Key] [Save]
```

---

### 2. OpenAI / Codex — EXPLICITLY SUPPORTED

**Finding:** OpenAI explicitly supports third-party OAuth access to Codex.

Key evidence:
- OpenAI officially partnered with OpenCode for subscription OAuth support
- OpenClaw documentation states: "OpenAI explicitly supports subscription
  OAuth usage in external tools/workflows"
- Codex CLI uses ChatGPT OAuth with device code flow as the default auth
- The Responses API is fully accessible to third-party tools
- Models like gpt-5.4 are available via Codex OAuth

**Auth methods available:**
1. OAuth via ChatGPT (device code flow) — uses subscription credits
2. API Key via OpenAI dashboard — pay-per-token

**Impact for sage-router:**
- We CAN implement Codex OAuth device code flow
- This is the strongest "one-click connect" opportunity
- Need to use OpenAI's public OAuth endpoints:
  - authorize: https://auth.openai.com/oauth/authorize
  - token: https://auth.openai.com/oauth/token
- May need to register our own OAuth client with OpenAI, or use the
  Codex CLI's public client ID (needs verification)

**Risk:** OpenAI may also restrict this in the future. The fact that they're
currently embracing it doesn't guarantee permanence. Design the auth module
so OAuth can be swapped for API key without user-facing changes.

**Dashboard UX:**
```
OpenAI / Codex
  Auth: OAuth (ChatGPT subscription) or API Key
  → [Connect with ChatGPT →] (opens browser)
  → or [Use API Key] (paste key)
```

---

### 3. Google Gemini CLI — FREE TIER WITH API KEY

**Finding:** Gemini offers a generous free tier that doesn't require OAuth.

- Free tier: 60 requests/min, 1,000 requests/day
- Auth options: Google Account OAuth, or API Key from Google AI Studio
- Gemini CLI is open-source (Apache 2.0)
- Google hasn't explicitly blocked third-party OAuth usage
- API Key can be obtained from aistudio.google.com with one click

**The practical path:**
API Key is simpler and more reliable than OAuth for our use case.
The free tier is generous enough that most users won't need OAuth.
For paid tiers (Google AI Pro/Ultra), Google Account OAuth is available
but requires a Google Cloud project setup.

**Impact for sage-router:**
- Primary path: API Key from Google AI Studio (free, one-click generation)
- Secondary path: Google OAuth for Pro/Ultra subscribers (requires
  Google Cloud project, more complex setup)
- No risk of enforcement action — API keys are the documented path

**Dashboard UX:**
```
Google Gemini
  Auth: API Key (free tier included!)
  → Get your key from aistudio.google.com (free, no credit card)
  → [Paste API Key] [Save]
  → Free: 60 req/min, 1,000 req/day
```

---

### 4. GitHub Copilot — FINE-GRAINED PAT

**Finding:** Copilot's internal API is technically usable via fine-grained PATs.

Key evidence:
- Copilot CLI supports Fine-grained PATs with "Copilot Requests" permission
- aider.chat documentation states: "The Copilot docs explicitly allow
  third-party agents that hit this API"
- api.githubcopilot.com exposes an OpenAI-compatible API
- LiteLLM has an official Copilot integration
- GitHub hasn't blocked third-party access like Anthropic has

**However:**
- The Copilot internal API (copilot_internal) is stated as "intended solely
  for officially supported clients" in GitHub community responses
- Using it outside of VS Code/JetBrains may violate TOS
- The fine-grained PAT with "Copilot Requests" permission is the
  supported path for programmatic access

**Gray area assessment:**
More permissive than Anthropic, but not as explicit as OpenAI. GitHub
is currently tolerating third-party usage (aider, LiteLLM, OpenCode all
work). But they haven't officially endorsed it for routing proxies.

**Impact for sage-router:**
- Support API key/PAT auth (fine-grained PAT with Copilot Requests permission)
- Do NOT implement OAuth device flow that mimics VS Code/JetBrains
- Document the PAT generation process clearly
- Monitor GitHub's stance — if they endorse it, add OAuth later

**Dashboard UX:**
```
GitHub Copilot
  Auth: Fine-grained Personal Access Token
  → Generate at github.com/settings/tokens → "Copilot Requests" permission
  → [Paste Token] [Save]
```

---

### 5. OpenRouter — API KEY

**Finding:** Standard API key provider. No OAuth flow.

- Sign up at openrouter.ai, get API key
- Provides access to 100+ models from multiple providers
- Standard OpenAI-compatible API
- No special auth considerations

**Dashboard UX:**
```
OpenRouter
  Auth: API Key
  → Get your key from openrouter.ai/keys
  → [Paste API Key] [Save]
```

---

### 6. Chinese Providers (GLM, Kimi, MiniMax, Qwen) — API KEY

**Finding:** All use standard API key authentication.

- GLM (Zhipu AI): API key from open.bigmodel.cn
- Kimi (Moonshot): API key from platform.moonshot.cn
- MiniMax: API key from api.minimax.chat
- Qwen (Alibaba): API key from dashscope.aliyun.com

Some have "Coding Plans" with restricted usage (Alibaba's Multi-Model
Coding Plan explicitly prohibits automated scripts and custom backends).

**Impact for sage-router:**
- Standard API key auth for all
- Document pricing tiers and restrictions
- Note: Alibaba's Coding Plan TOS restricts proxy usage — users must
  use their standard API plan instead

---

### 7. Ollama — NO AUTH (LOCAL)

**Finding:** Local model runner, no authentication needed.

- Runs on localhost, no auth required
- Sage-router connects directly to Ollama's API
- User just needs to provide the Ollama base URL

**Dashboard UX:**
```
Ollama (Local)
  → Base URL: http://localhost:11434
  → [Test Connection] [Save]
```

---

## Strategic Recommendations

### 1. Reframe the value proposition

9router's pitch was "free Claude via OAuth." That's dead. Sage-router's
pitch should be:

**"One endpoint for all your AI providers. Smart routing, automatic
fallback, usage tracking. Works with every major provider."**

The value shifts from "free access to locked-down providers" to
"unified management across the providers you already pay for."

### 2. Prioritize providers by auth ease

**Tier 1 — Zero friction (v1 launch):**
- Ollama (no auth)
- Google Gemini (free API key, generous free tier)
- OpenAI API Key
- Anthropic API Key

**Tier 2 — OAuth (v1 launch, if time allows):**
- OpenAI/Codex OAuth (device code flow — officially supported)

**Tier 3 — Semi-guided (v1 launch):**
- GitHub Copilot (PAT with instructions)
- OpenRouter (API key)
- Chinese providers (API keys)

### 3. Design auth module for flexibility

The auth system should support three auth types cleanly:

```go
type AuthType string
const (
    AuthAPIKey     AuthType = "apikey"      // Paste a key
    AuthOAuth      AuthType = "oauth"       // Browser-based flow
    AuthDeviceCode AuthType = "device_code" // Terminal-based flow
    AuthNone       AuthType = "none"        // Local providers (Ollama)
)
```

Each provider declares which auth types it supports. The dashboard
renders the appropriate UI. If a provider's OAuth gets blocked
tomorrow, we flip it to API key — one config change, no code rewrite.

### 4. The first-run experience should lead with Gemini

Since Gemini has the easiest free tier (API key from AI Studio, no credit
card), the onboarding flow should highlight it first:

```
Welcome to Sage Router

Get started with a free AI provider:

  [★ Google Gemini — Free, no signup]
    60 req/min, 1,000 req/day with API key

  [  OpenAI — Subscription or API Key]
    Connect via ChatGPT or paste API key

  [  Anthropic — API Key]
    Get key from console.anthropic.com

  [  + Browse all providers]
```

### 5. Document the enforcement landscape honestly

In sage-router's docs, be transparent:

> Sage-router uses only officially supported authentication methods.
> We do not use subscription OAuth tokens for providers that restrict
> third-party access. For Claude, this means API key authentication
> through the Anthropic Console. For providers like OpenAI that
> explicitly support third-party OAuth, we offer one-click connection.

This builds trust and differentiates us from tools that get blocked.

---

## Impact on Architecture

### Changes to ADR section 19 (OAuth Provider Strategy)

**Status: RESOLVED** — update from "Open" to "Decided"

**Decision:** Hybrid auth with provider-declared auth types.
- Anthropic: API key only (OAuth banned)
- OpenAI/Codex: OAuth (device code) + API key (officially supported)
- Google Gemini: API key (free tier) + OAuth for paid tiers
- GitHub Copilot: Fine-grained PAT
- All others: API key

**OAuth client registration needed:** Only for OpenAI (1 registration).
Research whether OpenAI has a developer program for OAuth app registration,
or whether we can use Codex CLI's public client credentials.

### Changes to onboarding flow

Reorder provider cards: Gemini first (free), then OpenAI, then Anthropic.
Remove "Claude Code — Free via OAuth" positioning entirely.

### Changes to Phase 1 timeline

Auth implementation is simpler than expected. API key auth covers 80%
of providers. Only OpenAI Codex OAuth needs device code flow.
Estimated savings: ~3 days from the auth module timeline.
