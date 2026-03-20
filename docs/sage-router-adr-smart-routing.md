# Sage-Router ADR Addendum: Smart Routing

> ADR Section 28 — to be appended to sage-router-adr-v2.md
> Status: **Decided**
> Date: 2026-03-19

---

## 28. Decided: Smart Routing Architecture

### Context

Sage-router's combo system provides static failover: try Model A, if unavailable
try Model B. The user manually defines the order. This handles *availability*
but has zero awareness of what a request actually *needs*.

Smart routing adds content-aware model selection: analyze the request, pick the
best-fit model from available connections. The goal is not to replace manual
selection but to provide an intelligent default for users who don't want to
become model selection experts.

### Prior Art Reviewed

- **RouteLLM** (LMSYS, ICLR 2025): Trained BERT/LLM classifiers on Chatbot Arena
  preference data. Routes between exactly 2 models (strong vs weak). Requires GPU.
  Not viable for a local binary.
- **OpenRouter Auto Router**: Simple strategy-based routing (`:nitro` for speed,
  `:floor` for cost, `auto` for balanced). No prompt analysis. Works well at scale
  with minimal intelligence.
- **Not Diamond, Martian, Neutrino**: Commercial routers with proprietary
  classification. Martian claims 98% cost savings. COLM 2025 paper found commercial
  routers vulnerable to adversarial prompt manipulation.
- **LLMRouter** (UIUC, 2025): Open-source library with 16+ router algorithms.
  Academic focus, Python-only, requires training data.
- **Nobody addresses session affinity.** No existing router considers conversation
  continuity. Switching models mid-conversation is an unsolved problem in the
  ecosystem.

### Decision

Smart routing is built in **three progressive layers**, each delivering standalone
value. The core principle: **ship what you can measure, defer what you can't.**

#### Design Principles (derived from advisory review)

1. **Don't build what you can't measure.** Build telemetry first, routing
   intelligence second. Let real usage data inform the decision tree.

2. **Don't add latency to the hot path.** Session affinity check is O(1) and
   runs first. Classification runs only on new conversations, only examines
   the last user message, and must complete in <5ms.

3. **Single objective, not blended scoring.** Users pick one optimization goal
   (fast/cheap/best). Filter by hard constraints, sort by that one dimension.
   No scoring function, no weight tuning, no black box.

4. **Smart routing produces a model list, existing fallback consumes it.**
   The router outputs an ordered list of candidate models. The fallback mechanism
   iterates through them. One abstraction, two sources: static combo (user-defined)
   or dynamic routing (system-generated).

5. **Session affinity is the default; switching is the exception.** Never switch
   models mid-conversation unless forced by unavailability. Make every routing
   decision visible and explainable.

6. **Transparent recommendation, not hidden magic.** Every auto-routed request
   shows *why* that model was chosen — in logs and in the dashboard. Users can
   see, understand, and override.

---

### Layer 1: Strategy Presets + Session Affinity (v1.1)

**Zero intelligence, pure policy.** Three routing strategies that require no
request analysis:

```
model: auto           → default strategy (user-configurable, default: balanced)
model: auto:fast      → pick the fastest available model (by historical p50 latency)
model: auto:cheap     → pick the cheapest available model (by cost per token)
model: auto:best      → pick the highest capability tier model
```

**How it works:**

1. Check session affinity cache (O(1) lookup)
   - Hit → use the affinity model (skip routing entirely)
   - Miss → continue to step 2

2. List all available models from active connections

3. Sort by the selected strategy's single objective:
   - `fast`: sort by p50 latency ascending (from usage history)
   - `cheap`: sort by input cost per 1M tokens ascending
   - `best`: sort by capability tier descending (see Capability Tiers below)
   - `balanced`: sort by capability tier descending, break ties by cost ascending

4. Return the sorted list as a dynamic combo

5. Existing fallback mechanism iterates: try first, if unavailable try second, etc.

**Capability Tiers** (not individual scores — tiers are stable and rarely change):

```
Tier 1 (frontier):  claude-sonnet-4, gpt-4o, gemini-2.5-pro, o3
Tier 2 (strong):    claude-haiku-4.5, gpt-4o-mini, gemini-2.5-flash
Tier 3 (efficient): gpt-4.1-mini, gemini-2.0-flash, smaller models
Tier 4 (free):      ollama local models, free-tier provider models
```

Tiers are defined in `internal/config/models.go` and shipped with the binary.
Updating a model's tier is a one-line change. This is deliberately coarse —
we're not pretending to have precise quality rankings.

**Session Affinity:**

```go
type SessionCache struct {
    mu    sync.RWMutex
    items map[uint64]*SessionEntry // fnv hash → entry
}

type SessionEntry struct {
    Provider   string
    Model      string
    LastUsedAt time.Time
    TurnCount  int
}
```

- **Key:** FNV-1a hash of the first user message content (first 200 bytes).
  Stable across turns because the first message doesn't change.
- **TTL:** 2 hours of inactivity. LRU eviction at 10,000 entries.
- **Write:** After every successful response, update the entry with the model used.
- **Read:** Before any routing, check the cache. Hit → use affinity model.

**Affinity break policy** (when the affinity model is unavailable):

```
Priority 1: Same model, different account (account-level fallback)
Priority 2: Same family (Sonnet → Haiku, GPT-4o → GPT-4o-mini)
Priority 3: Same vendor (any Anthropic model → any other Anthropic model)
Priority 4: Cross-vendor (last resort, logged as a warning)
```

Every affinity break is logged with the reason:

```
[ROUTE] Session affinity break: claude-sonnet-4 rate-limited
[ROUTE] Fallback: claude-haiku-4-5 (same family)
```

**What Layer 1 does NOT do:**
- No request content analysis
- No code detection or reasoning markers
- No soft scoring or weight tuning
- No ML models or embeddings

---

### Layer 2: Hard Constraint Filtering (v2)

**Add capability gates before strategy sorting.** When `model: auto` is used,
filter out models that *cannot handle* the request before sorting by strategy:

| Signal | Detection Method | Constraint |
|--------|-----------------|------------|
| Has images | `content[].type == "image"` in messages | Require `supportsImages` |
| Has tools | `tools[]` non-empty | Require `supportsTools` |
| Has PDF/documents | `content[].type == "document"` | Require `supportsPDF` |
| Long context | Estimated tokens > 128K | Require `contextWindow > 200K` |
| Extended thinking | `thinking` config present | Require `supportsThinking` |

**Every constraint is binary (yes/no) and verifiable.** No soft scores, no
judgment calls. A model either supports images or it doesn't.

Detection cost: one scan of the last user message + quick checks on the request
body. Under 1ms for any realistic request.

**Flow:**

```
available_models
  → filter by hard constraints (images, tools, context, etc.)
  → check session affinity
  → sort by strategy objective
  → return as dynamic combo list
```

---

### Layer 3: Signal-Based Routing with Decision Tree (v2+)

**Only built after 4-6 weeks of telemetry data from Layer 1 and 2.**

Add soft signals that *prefer* certain models without excluding others:

| Signal | Detection | Preference |
|--------|-----------|------------|
| High code density | `{}/()` character frequency in last user message | Prefer coding-strong models |
| Reasoning markers | "step by step", "analyze", "prove", "why" | Prefer reasoning-strong models |
| Simple query | Short last message, no tools, no images | Prefer fast/cheap models |
| Long system prompt | System prompt > 4K tokens | Prefer prompt-caching models |

**Implementation: explicit decision tree, not a scoring function.**

```go
func classifyRequest(req *canonical.Request) []Preference {
    prefs := []Preference{}
    lastMsg := lastUserMessage(req)
    
    if codeCharRatio(lastMsg) > 0.4 {
        prefs = append(prefs, Preference{Signal: "code_heavy", Boost: "coding"})
    }
    if hasReasoningMarkers(lastMsg) {
        prefs = append(prefs, Preference{Signal: "reasoning", Boost: "reasoning"})
    }
    if isSimpleQuery(lastMsg) {
        prefs = append(prefs, Preference{Signal: "simple", Boost: "speed"})
    }
    return prefs
}
```

Preferences are tiebreakers within the same capability tier — they don't
override the user's strategy or hard constraints. A `cheap` strategy with
a "code_heavy" signal still picks the cheapest model, but among models
at the same price point, it prefers the one with better coding capability.

**The decision tree is derived from real usage data**, not hand-tuned intuition.
After collecting telemetry from Layers 1-2 (retry rates, conversation lengths,
abandon rates per model per task type), we analyze which models perform best
for which signal combinations. The tree is generated from this analysis and
shipped as a config update.

---

### Telemetry: The Foundation for Everything

**Built in v1.1, before any routing intelligence.** Every request records:

```go
type RoutingTelemetry struct {
    // Request signals (computed during classification)
    EstimatedTokens   int
    HasTools          bool
    HasImages         bool
    CodeCharRatio     float64
    LastMsgLength     int
    TurnCount         int
    
    // Routing decision
    Strategy          string    // "fast", "cheap", "best", "balanced", "manual"
    ModelSelected     string    // "anthropic/claude-sonnet-4"
    RoutingReason     string    // "session_affinity", "strategy_sort", "constraint_filter"
    AffinityHit       bool
    AffinityBroken    bool
    AffinityBreakReason string  // "rate_limited", "unavailable"
    
    // Outcome signals (collected after response)
    LatencyTTFB       time.Duration
    LatencyTotal      time.Duration
    TokensUsed        int
    Cost              float64
    Status            string    // "success", "error", "abandoned"
    
    // Quality proxy signals
    IsRetry           bool      // user sent same/similar request again
    ConversationEnded bool      // no follow-up within 5 minutes
    FollowUpNegative  bool      // next message contains "wrong", "fix", "try again"
}
```

This data powers:
1. The routing report in the dashboard (cost/latency/availability comparisons)
2. The p50 latency values used by the `fast` strategy
3. Future decision tree derivation in Layer 3
4. User-facing explanations of routing decisions

**Retry detection:** If two requests within 60 seconds have the same first user
message and the same last user message (but the second has an additional
user turn like "that's wrong"), the first request is marked as a retry.
This is our best automated proxy for "the response was bad."

---

### The Unified Abstraction: ModelList

Following the Gang of Four's insight, smart routing and manual combos share
one interface:

```go
// ModelList produces an ordered list of models to try for a request.
// The fallback executor consumes this list — it doesn't know or care
// whether the list came from a static combo or the smart router.
type ModelList interface {
    Resolve(req *canonical.Request, available []*Connection) []ModelRef
}

// StaticCombo returns a fixed, user-defined list.
type StaticCombo struct {
    Models []ModelRef
}

func (c *StaticCombo) Resolve(_ *canonical.Request, _ []*Connection) []ModelRef {
    return c.Models
}

// SmartRouter analyzes the request and returns a ranked list.
type SmartRouter struct {
    Strategy    Strategy
    Affinity    *SessionCache
    Constraints []Constraint
    Signals     []Signal  // empty in Layer 1-2, populated in Layer 3
}

func (r *SmartRouter) Resolve(req *canonical.Request, available []*Connection) []ModelRef {
    // 1. Check session affinity
    if model := r.Affinity.Get(req); model != "" {
        // Return affinity model first, then fallbacks in family order
        return r.buildAffinityList(model, available)
    }
    
    // 2. Filter by hard constraints
    candidates := r.filterByConstraints(req, available)
    
    // 3. Apply signal-based preferences (Layer 3 only, no-op in Layer 1-2)
    candidates = r.applySignals(req, candidates)
    
    // 4. Sort by strategy objective
    r.Strategy.Sort(candidates)
    
    return candidates
}
```

**Pipeline integration:** This replaces Stage ③ (Resolve) when `model: auto`
is detected. For explicit `provider/model` requests, the pipeline works exactly
as before — no routing logic involved.

```
① Ingress → ② Auth → ③ Resolve
                         ├── explicit model → direct path (no change)
                         ├── combo name → StaticCombo.Resolve()
                         └── auto[:strategy] → SmartRouter.Resolve()
                      → ③b CostOptimize → ④ Select (iterates the list) → ...
```

---

### UX: Making Routing Decisions Visible

**In logs (every auto-routed request):**

```
[ROUTE] auto:balanced | coding task | 12K tokens | 3 tools
[ROUTE] Session affinity: claude-sonnet-4 (turn 5)
[ROUTE] → anthropic/claude-sonnet-4 (affinity hit)
```

```
[ROUTE] auto:cheap | simple query | 200 tokens | no tools
[ROUTE] No affinity (new conversation)  
[ROUTE] → gemini/gemini-2.5-flash ($0.15/1M, fastest cheap option)
```

```
[ROUTE] auto:best | has images | 45K tokens
[ROUTE] Session affinity: claude-sonnet-4 (BROKEN - rate limited)
[ROUTE] Fallback: claude-haiku-4-5 (same family)
[ROUTE] → anthropic/claude-haiku-4-5 (affinity break: rate_limited)
```

**In the dashboard routing report:**

```
┌──────────────────────────────────────────────────────────┐
│  Smart Routing Report (last 7 days)                      │
│                                                          │
│  Strategy: balanced                                      │
│  Total requests: 847 auto-routed                         │
│                                                          │
│  ROUTING DECISIONS                                       │
│  Session affinity hits:   672 (79%)                      │
│  New conversation routes: 148 (17%)                      │
│  Affinity breaks:          27 (3%)                       │
│    └ Rate limited: 19, Unavailable: 8                    │
│                                                          │
│  OUTCOMES vs MANUAL BASELINE                             │
│  Avg cost/request:  $0.008  (manual avg: $0.012) -33%   │
│  Avg TTFB:          1.1s    (manual avg: 1.4s)   -21%   │
│  Success rate:      99.1%   (manual: 97.8%)      +1.3%  │
│  Retry rate:        4.2%    (manual: 5.1%)        -0.9% │
│                                                          │
│  MODEL DISTRIBUTION                                      │
│  anthropic/claude-sonnet-4:  52%  (affinity)             │
│  openai/gpt-4o-mini:        24%  (simple queries)        │
│  gemini/gemini-2.5-flash:   18%  (long context)          │
│  openai/o3-mini:             6%  (reasoning)             │
│                                                          │
│  ⓘ Manual baseline = what your default model would       │
│    have cost for the same requests                       │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

The "manual baseline" comparison is computed by calculating what the user's
default model (most frequently used model in non-auto requests) would have
cost for the same token counts. This gives a concrete, personal cost comparison.

---

### What We Explicitly Don't Build

1. **No ML-based classifier.** No BERT, no embeddings, no GPU requirement.
   Classification is heuristic in Layer 3 and absent in Layers 1-2.

2. **No multi-objective scoring function.** No weights to tune, no Pareto
   optimization. One objective per strategy, constraints are binary filters.

3. **No prompt-content-based routing in Layer 1.** The first release of smart
   routing is completely content-agnostic. It routes by strategy + availability +
   session affinity. Content analysis comes later, informed by data.

4. **No automatic model switching mid-conversation.** Session affinity is
   broken only by provider unavailability, never by "a better model exists."
   The minimax argument: the expected quality loss from switching exceeds
   the expected quality gain from picking a marginally "better" model.

5. **No quality claims we can't back up.** We claim cost reduction, latency
   improvement, and availability improvement — all measurable. We do NOT
   claim "better quality" until we have user feedback data to support it.

---

### Implementation Timeline

| Layer | Version | Prerequisites | Effort |
|-------|---------|--------------|--------|
| Strategy presets + session affinity | v1.1 | Telemetry infrastructure | 1 week |
| Hard constraint filtering | v2.0 | Layer 1 deployed, basic telemetry flowing | 3 days |
| Signal-based decision tree | v2.x | 4-6 weeks of telemetry data from Layers 1-2 | 1-2 weeks |
| Dashboard routing report | v1.1 | Layer 1 | 2 days |
| User feedback (thumbs up/down) | v2.x | Dashboard routing report | 2 days |

---

### Design Principles (appended to §27)

**"Smart routing respects conversation continuity."**
Switching models mid-conversation is a last resort, not an optimization.
Session affinity is the default. Affinity breaks are logged and visible.

**"Route by policy, not by prediction."**
Users choose an objective (fast, cheap, best). The router applies that
objective deterministically. No hidden intelligence, no unpredictable behavior.

**"Measure first, optimize second."**
Telemetry collection ships before routing intelligence. Real usage data
informs the decision tree. Intuition is replaced by evidence.

**"The router produces a list, the fallback consumes it."**
Smart routing and manual combos are the same abstraction — a ModelList.
The fallback executor doesn't know or care where the list came from.

---

---

## 29. Decided: Context Bridge — Preserving Quality Across Model Switches

### Problem

When session affinity breaks and the router switches to a different model,
the new model receives the full `messages[]` array but lacks the prior model's
implicit understanding — its reasoning patterns, architectural decisions, and
code style. The messages contain this information, but it's spread across
dozens of turns and sits in the "lost in the middle" zone where transformer
attention is weakest.

The result: even with identical conversation history, the new model produces
noticeably lower-quality responses that miss established conventions and
repeat already-resolved decisions.

### Key Insight

The problem is not missing data — it's **attention degradation over long context**.
The context bridge doesn't add new information. It re-surfaces important
information from deep in the conversation into a position (system prompt)
where the new model will reliably attend to it.

This is the same principle behind "context distillation" recommended by
researchers for long conversations. We apply it automatically at the
routing boundary.

### Rejected Approach: Compressed File Attachment

We considered compressing the full conversation history into a zip file and
attaching it to the request when switching models. A 1MB conversation compresses
to ~200KB — seemingly a 5× efficiency gain.

**This doesn't work.** When an LLM API receives a file attachment, it extracts
the text and injects it as tokens into the context window. Zip compression is
undone before the model sees it. A 1MB text file becomes ~250K tokens regardless
of how it was transferred. Compression solves a storage/transfer problem, but
the bottleneck is a cognitive/attention problem. Token count, not byte count,
is the constraint that matters.

**However**, compression IS useful for our storage layer. Gzip on the in-memory
ring buffer stretches a 50MB RAM budget to hold ~250MB of raw conversation text.
We apply compression to storage, not to model input.

### Design: Two-Tier Context Bridge

#### Tier 1: Structured State + Sliding Window (default)

**Zero cost. < 1ms. No LLM call.**

Combines two complementary approaches:

**Structured state extraction** — scan assistant messages for concrete artifacts.
This captures *what was built* without the *reasoning around it*:

```
## Context Bridge (previous model: anthropic/claude-sonnet-4)
Project: Go REST API with gin + PostgreSQL
Files modified: middleware/auth.go, models/task.go, db/migrations/003.sql
Current task: Fix N+1 query in GET /api/tasks
Approach: Add eager loading with GORM Preload
Decisions made:
- JWT auth over session cookies (turn 3)
- PostgreSQL over SQLite (turn 5)
- Gin over Echo (turn 2)
```

Extraction is heuristic: scan assistant messages for file paths (`/path/to/file`),
decision language ("I'll use X", "Let's go with Y", "I chose X because"),
code blocks (fenced markdown), and error messages.

**Sliding window** — the last 2-3 full turns (user + assistant), uncompressed.
Recent turns contain the active working context — the current problem, latest
code, and immediate state. Older turns are represented only by the structured
state.

Total bridge payload:

```
[Structured state: ~300 tokens]
[Full turn N-2: user + assistant, ~2K tokens]
[Full turn N-1: user + assistant, ~2K tokens]
──────────────────────────────────────────────
Total: ~4-5K tokens appended to system prompt
```

This is what a senior engineer would write in a handoff note: project background
plus the specific thing we were just working on.

#### Tier 2: LLM-Compressed Bridge (opt-in)

**~$0.003 per switch. ~1-2s added latency.**

Sends the last N turns to a fast cheap model (GPT-4o-mini, Gemini Flash) with:

> "Compress this conversation into a context briefing for a new assistant
> taking over. Include: current task, key decisions, code artifacts, and
> blockers. Max 500 tokens."

Higher fidelity than heuristic extraction, but adds cost and latency at the
exact moment the user is already experiencing degraded service (a model switch
typically happens because of rate limiting). Use when context preservation is
more important than response speed.

### Implementation

#### Conversation Store (ring buffer)

```go
type ConversationStore struct {
    mu      sync.RWMutex
    convos  map[uint64]*ConversationHistory // session hash → history
    maxSize int64                           // total memory budget (default: 50MB)
    curSize int64
}

type ConversationHistory struct {
    Turns     []Turn
    LastModel string
    UpdatedAt time.Time
}

type Turn struct {
    Role       string   // "user" or "assistant"
    Content    []byte   // gzip-compressed raw text (images stripped)
    CodeBlocks []string // extracted code fences (uncompressed, for bridge)
    TokenCount int      // estimated token count
    Model      string   // which model generated this (assistant turns only)
}
```

**Storage efficiency:** Raw conversation text is gzip-compressed per turn.
Base64 image data is stripped before storage (it's in the messages array
anyway — we only need text for bridging). A 50MB budget holds ~250MB of
raw conversation text — roughly 50 million tokens of history.

**Eviction:** LRU by `UpdatedAt`. When `curSize` exceeds `maxSize`, evict
the least-recently-updated conversation. Conversations older than 2 hours
(matching session affinity TTL) are evicted proactively.

#### Bridge Generation

```go
func BuildContextBridge(history *ConversationHistory, maxTokens int) string {
    var bridge strings.Builder
    bridge.WriteString("## Context from previous assistant (")
    bridge.WriteString(history.LastModel)
    bridge.WriteString(")\n\n")

    // Part 1: Structured state (decisions + artifacts)
    bridge.WriteString(extractStructuredState(history))
    bridge.WriteString("\n")

    // Part 2: Last 2-3 full turns (recent working context)
    recentTurns := lastNTurns(history, 3)
    for _, turn := range recentTurns {
        content := decompress(turn.Content)
        bridge.WriteString(fmt.Sprintf("**%s:**\n", turn.Role))
        bridge.WriteString(truncateToTokens(content, maxTokens/3))
        bridge.WriteString("\n\n")
    }

    return truncateToTokens(bridge.String(), maxTokens)
}

func extractStructuredState(history *ConversationHistory) string {
    var state strings.Builder

    files := extractFileReferences(history)    // regex: /path/to/file patterns
    decisions := extractDecisions(history)      // regex: "I'll use", "let's go with", "chose X"
    codeArtifacts := extractKeyCode(history)    // last code block per unique file path

    if len(files) > 0 {
        state.WriteString("Files: " + strings.Join(files, ", ") + "\n")
    }
    if len(decisions) > 0 {
        state.WriteString("Decisions:\n")
        for _, d := range decisions {
            state.WriteString("- " + d + "\n")
        }
    }
    if len(codeArtifacts) > 0 {
        state.WriteString("Key code:\n")
        for file, code := range codeArtifacts {
            state.WriteString(fmt.Sprintf("  %s:\n```\n%s\n```\n", file, truncate(code, 200)))
        }
    }
    return state.String()
}
```

#### Pipeline Integration

The bridge is injected in Stage ④ (Select) when an affinity break is detected:

```
Stage ④ Select
  → Detects affinity break (new model ≠ session's last model)
  → If contextBridge enabled AND conversation history exists:
      bridge := BuildContextBridge(history, 4000)  // ~4K token budget
      Append bridge to canonical.Request.System
      Tag request: metadata.bridge_injected = true
  → Continue to Stage ⑤ Translate
```

**Bridge lifecycle:**
1. Injected on the first request after a model switch
2. Kept for the next 2 subsequent requests (3 total turns with new model)
3. Removed automatically after turn 3 — the new model has established
   its own context by then
4. If the model switches AGAIN, a new bridge is generated from the
   full conversation history (including the turns handled by the
   intermediate model)

**Bridge is a system message, not a conversation message.** This is important:
- System messages sit at the beginning of context where attention is strongest
- They don't compound into the conversation history for subsequent requests
- They can be removed cleanly after 3 turns without altering the messages array

### Configuration

```
routing.contextBridge = "off" | "heuristic" | "llm"

  off:       No bridge injection (default in v1.1)
  heuristic: Structured state + sliding window (default from v2)
  llm:       LLM-generated summary (opt-in, requires a fast model connection)
```

```
routing.contextBridgeMaxTokens = 4000    # max bridge size in tokens
routing.contextBridgeTurns = 3           # how many turns to keep the bridge
routing.conversationStoreMaxMB = 50      # RAM budget for conversation history
```

### Reuse Beyond Bridging

The conversation store serves multiple purposes:

1. **Context bridge** — the primary use case described here
2. **Retry detection** — compare current request to recent history to detect
   "try again" patterns (telemetry proxy signal for quality)
3. **Dashboard conversation viewer** — show recent request/response pairs
   for debugging and routing inspection
4. **Future: conversation-level cost tracking** — aggregate costs per
   conversation, not just per request

### Timeline

| Component | Version | Effort |
|-----------|---------|--------|
| Conversation store (ring buffer + gzip) | v1.1 | 2 days |
| Heuristic bridge generation | v2.0 | 2 days |
| Bridge injection in pipeline | v2.0 | 1 day |
| LLM bridge (opt-in) | v2.x | 1 day |
| Dashboard conversation viewer | v2.0 | 1 day |

The conversation store ships with v1.1 (alongside smart routing Layer 1)
because it's useful for telemetry immediately. Bridge injection ships
with v2.0 after we have data on affinity break frequency and quality impact.

---

### Decision Log

| Date | Decision |
|------|----------|
| 2026-03-19 | Smart routing: three progressive layers |
| 2026-03-19 | No scoring function — single objective sort |
| 2026-03-19 | Session affinity as default, FNV hash + 2hr TTL |
| 2026-03-19 | Affinity break: same family → same vendor → cross-vendor |
| 2026-03-19 | Telemetry before intelligence (4-6 weeks of data first) |
| 2026-03-19 | ModelList interface unifies combos and smart routing |
| 2026-03-19 | Capability tiers (4 levels) instead of individual scores |
| 2026-03-19 | Quality measured via proxy signals (retry rate, abandon rate) |
| 2026-03-19 | All routing decisions visible in logs and dashboard |
| 2026-03-19 | Context bridge: heuristic default, LLM opt-in |
| 2026-03-19 | Rejected: zip file attachment (tokens ≠ bytes, solves wrong problem) |
| 2026-03-19 | Conversation store: gzip ring buffer, 50MB budget, LRU eviction |
| 2026-03-19 | Bridge injection: system message, 3-turn lifecycle, auto-removed |
| 2026-03-19 | Conversation store ships v1.1, bridge injection ships v2.0 |
