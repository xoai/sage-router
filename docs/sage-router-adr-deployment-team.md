# Sage-Router ADR Addendum: Deployment Model & Team Features

> ADR Sections 33-35 — to be appended to sage-router-adr-v2.md
> Status: **Decided**
> Date: 2026-03-19

---

## 33. Decided: No Modes — Features Activate on Demand

### Problem

Sage-router serves two audiences with different needs: individual developers
on laptops and teams sharing a centralized server. We considered separating
these into two modes (`personal` / `team`) or two editions (separate binaries).

### Decision

**No modes. No editions. One binary. Features activate when configured and
are invisible when not.**

The software shape-shifts based on how you use it:

| What you do | What sage-router becomes |
|---|---|
| Create 1 key, no budget, no ACLs | Personal proxy |
| Create multiple named keys | Team proxy with per-key usage views |
| Set budgets on keys | Budget-enforced proxy with alerts and limits |
| Set allowed models on keys | Policy-enforced proxy with model restrictions |
| Set rate limits on keys | Rate-limited proxy preventing abuse |
| Run Ollama with an embedding model | Intelligent routing with auto-classification |

Every team feature is a zero-cost no-op when not configured. No conditional
compilation, no feature gates, no mode flags.

### Rationale

**The technical difference is tiny.** Team features amount to ~6 conditional
checks across the codebase: budget check in authenticate, ACL check in
resolve, key_id in usage tracking, rate limit check, and dashboard view
switching. This doesn't justify two binaries, two build pipelines, or two
release processes.

**Users upgrade naturally.** A developer starts sage-router on their laptop.
It works. They tell their team. The team lead deploys it on a shared server,
creates keys per team, adds budgets. Same binary, same config format, zero
migration.

**Separate editions create pricing expectations.** "sage-router" vs
"sage-router-team" implies the team version costs money. This creates
adoption friction before we've earned community trust.

**Git is the precedent.** Git doesn't have a "personal mode" and "team mode."
You can use it alone or with 500 collaborators. Features are there when you
need them.

### How Features Activate

```go
// Budget enforcement — only runs if budget is set (zero = unlimited)
if key.BudgetMonthly > 0 {
    spent := s.store.GetMonthlySpend(key.ID)
    if key.HardLimit && spent >= key.BudgetMonthly {
        return errBudgetExceeded(key.Name, spent, key.BudgetMonthly)
    }
}

// ACL enforcement — only runs if allowed_models is not wildcard
if key.AllowedModels != "*" {
    if !matchesPattern(ctx.Model, key.AllowedModels) {
        return errModelNotAllowed(key.Name, ctx.Model)
    }
}

// Rate limiting — only runs if rate limit is configured (zero = unlimited)
if key.RateLimitRPM > 0 {
    if !s.limiter.Allow(key.ID) {
        return errRateLimited(key.Name)
    }
}
```

The dashboard adapts automatically:

```
1 API key  → personal-style usage view (no per-key breakdown)
2+ API keys → per-key usage table with cost breakdown per key
Budgets set → budget progress bars and alert indicators
ACLs set   → model access matrix visible in key management
```

---

## 34. Decided: Team Features via Keys-as-Groups

### Problem

Teams need per-group cost tracking, model restrictions, and usage quotas.
Traditional approaches require user accounts, group membership, and RBAC
permission systems — significant infrastructure for a lightweight proxy.

### Decision

**A key IS a group.** Multiple people sharing the same key are implicitly
a group. The key's name is the group name. The key's attributes are the
group's permissions. No users, no groups, no roles, no RBAC.

### Schema Extension

Five columns added to the existing `api_keys` table. One foreign key added
to the existing `usage_log` table. No new tables.

```sql
ALTER TABLE api_keys ADD COLUMN budget_monthly     REAL    DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN budget_hard_limit  BOOLEAN DEFAULT false;
ALTER TABLE api_keys ADD COLUMN allowed_models     TEXT    DEFAULT '*';
ALTER TABLE api_keys ADD COLUMN rate_limit_rpm     INTEGER DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN routing_strategy   TEXT    DEFAULT '';

ALTER TABLE usage_log ADD COLUMN api_key_id TEXT REFERENCES api_keys(id);
```

### Key Attributes

```yaml
# Example: agency with per-project keys
keys:
  - name: "project-alpha"
    budget:
      monthly: 500.00
      alertAt: 400.00
      hardLimit: true
    models:
      allow: ["*"]
    rateLimit:
      requestsPerMinute: 60

  - name: "project-beta"
    budget:
      monthly: 300.00
      hardLimit: true
    models:
      allow: ["anthropic/*"]   # client requires Anthropic only
    rateLimit:
      requestsPerMinute: 30

  - name: "interns"
    budget:
      monthly: 30.00
      hardLimit: true
    models:
      allow: ["gemini/*", "gpt-4o-mini"]
    rateLimit:
      requestsPerMinute: 10
    routing:
      strategy: "cheap"
```

### Real-World Coverage

| Scenario | How keys-as-groups handles it |
|---|---|
| **Small startup (5-15 devs)** | One shared key. No restrictions. Single-pane usage view. |
| **Tiered access (seniors vs interns)** | Two keys with different model allow-lists and budgets. |
| **Agency with client projects** | Per-project keys. Per-key cost tracking for client billing. |
| **CI/CD pipeline** | Dedicated key with low budget, restricted models, rate limited. |

### When This Model Breaks Down

- **Individual accountability:** "Which developer on the team made this request?"
  Not possible with shared keys. Acceptable for Scenarios A-C. Environments
  requiring per-person audit trails need SSO integration, which is out of scope.

- **Dynamic membership:** Developer moves teams. Solution: give them the new
  key, revoke the old one. Slightly manual, happens rarely.

- **Cross-cutting policies:** Solvable with multiple keys per person
  (one general key, one project-specific key). Slightly awkward, workable.

None of these limitations justify building a full RBAC system. When a real
customer says "I need per-user tracking with SSO," that's the signal to
build the user layer. Not before.

### Dashboard for Teams

```
┌──────────────────────────────────────────────────┐
│  Usage Overview (March 2026)                     │
│                                                  │
│  Key              Requests   Cost     Budget     │
│  ──────────────── ────────── ──────── ────────── │
│  project-alpha    1,847      $142.30  $500 (28%) │
│  project-beta       523       $67.10  $300 (22%) │
│  interns            312       $18.40   $30 (61%) │
│  ci-pipeline      3,201       $31.20   $50 (62%) │
│  ──────────────── ────────── ──────── ────────── │
│  Total            5,883      $259.00             │
│                                                  │
│  [+ Create New Key]                              │
└──────────────────────────────────────────────────┘
```

Clicking a key drills into its request log, model distribution, cost over
time, and routing decisions — the same views as the personal dashboard,
filtered by `api_key_id`.

---

## 35. Decided: Embedding Classifier — Advanced Feature, Honest Positioning

### Problem

Our heuristic classifier (regex patterns, character frequency) correctly
classifies ~70-80% of requests. An embedding-based classifier using a model
like EmbeddingGemma could reach ~85-90% by capturing semantic meaning, but
requires significant hardware resources.

### Decision

The embedding classifier is an **optional advanced feature** positioned
honestly as requiring dedicated hardware. It is not hidden or gated — it's
available to anyone — but documentation and UI clearly communicate its
requirements and intended deployment.

### Positioning

**In documentation:**

> **Embedding-based classification (advanced)**
>
> For teams running sage-router on a centralized server, the embedding
> classifier provides more accurate request classification for intelligent
> routing. Instead of pattern matching, it uses a lightweight embedding
> model to understand the semantic intent of each request.
>
> Requirements:
> - A running Ollama instance with an embedding model
>   (e.g., `ollama pull embeddinggemma`)
> - Recommended: 1GB+ free RAM for the embedding model
> - Recommended: dedicated server or workstation (not a laptop)
> - Adds ~50ms latency per new conversation (cached conversations
>   use session affinity and skip classification)
>
> This feature is designed for team deployments where multiple users
> with diverse workloads benefit from automatic model selection. For
> personal use, the default heuristic classifier is fast and sufficient.

**In dashboard settings:**

```
Routing → Classification Method

  ● Heuristic (default)
    Pattern-based classification. Fast (<1ms), no dependencies.
    Suitable for all deployments.

  ○ Embedding (advanced)
    Semantic classification via local embedding model.
    More accurate for ambiguous requests.
    Requires: Ollama running with an embedding model.
    ⚠ Recommended for server deployments with 1GB+ spare RAM.
    
    Ollama URL: [http://localhost:11434      ]
    Model:      [embeddinggemma              ]
    [Test Connection]
```

### Architecture: Ollama as Sidecar

The embedding model runs in Ollama, not inside sage-router. Sage-router
stays at ~15MB binary with zero ML dependencies.

```
sage-router (15MB)              Ollama (separate process)
┌──────────────────┐            ┌──────────────────────┐
│ Pipeline         │            │ embeddinggemma       │
│  ↓               │   HTTP     │  (~400MB model)      │
│ Classifier ──────│───────────→│  /api/embeddings     │
│  ↓               │  ~50ms     │                      │
│ Router           │            │                      │
└──────────────────┘            └──────────────────────┘
```

**Why Ollama, not a bundled model:**
- Adds zero bytes to sage-router's binary
- Uses infrastructure the user likely already has (teams running local
  models for cost-sensitive tasks)
- Model updates happen through Ollama's ecosystem, not sage-router releases
- Memory footprint is outside sage-router's process, visible in system monitor

### Classification Input (shared by both classifiers)

Both heuristic and embedding classifiers receive the same truncated input.
This interface is stable regardless of which classifier is active:

```go
type ClassificationInput struct {
    SystemPromptHead string // first 2 sentences of system prompt
    FirstUserHead    string // first 2 sentences of first user message
    LastUserMessage  string // full last user message, up to 200 tokens
}

type Intent string
const (
    IntentCoding    Intent = "coding"
    IntentReasoning Intent = "reasoning"
    IntentCreative  Intent = "creative"
    IntentSimple    Intent = "simple"
    IntentGeneral   Intent = "general"
)

type Classification struct {
    Intent     Intent
    Confidence float64  // 0.0 - 1.0
}

type Classifier interface {
    Classify(input ClassificationInput) (Classification, error)
}
```

**Truncation rationale:** First sentences capture the task context
(system prompt + initial instruction). Last message captures the current
intent. The middle (conversation history) is context, not intent. Skipping
it reduces embedding inference from ~200ms (full history) to ~50ms
(~300 tokens).

### Capability Detection (auto mode)

```yaml
routing:
  classifier: auto   # default
```

`auto` means:
1. Check if `routing.classifierURL` is configured → use embedding classifier
2. Check if Ollama is running at `localhost:11434` with an embedding model → use it
3. Fall back to heuristic classifier

No error if Ollama isn't available. No warning on startup. The heuristic
classifier is always the silent fallback. The only time the user sees a
message is if they explicitly configure `classifier: embedding` and Ollama
isn't reachable — then it's a clear error.

### Reference Vectors for Intent Classification

The embedding classifier works by comparing the request's embedding vector
against pre-computed reference vectors for each intent category:

```go
type EmbeddingClassifier struct {
    ollamaURL  string
    model      string
    references map[Intent][]float64  // pre-computed reference vectors
}

func (c *EmbeddingClassifier) Classify(input ClassificationInput) (Classification, error) {
    text := input.SystemPromptHead + "\n" + input.LastUserMessage
    
    vector, err := c.embed(text)
    if err != nil {
        return Classification{Intent: IntentGeneral}, err
    }
    
    bestIntent := IntentGeneral
    bestSim := 0.0
    for intent, ref := range c.references {
        sim := cosineSimilarity(vector, ref)
        if sim > bestSim {
            bestSim = sim
            bestIntent = intent
        }
    }
    
    return Classification{Intent: bestIntent, Confidence: bestSim}, nil
}
```

Reference vectors are computed at startup by embedding a curated set of
representative sentences per intent:

```go
var intentExamples = map[Intent][]string{
    IntentCoding: {
        "Fix the bug in the authentication middleware",
        "Write a function that parses CSV files",
        "Refactor this database query to avoid N+1",
        "Help me design the REST API endpoints",
        "Why is this goroutine leaking memory",
        // ... 20-30 examples per intent
    },
    IntentReasoning: {
        "Analyze why this approach fails at scale",
        "Compare the tradeoffs between these architectures",
        "Prove that this algorithm is O(n log n)",
        "Think through the edge cases for this design",
        // ...
    },
    // ...
}
```

Each intent's reference vector is the centroid (average) of its example
embeddings. This is computed once at startup (~2-3 seconds) and cached
in memory.

**Quality of classification depends on quality of examples.** The example
set ships with sage-router and can be extended via config. Users who find
misclassifications can add examples to improve their deployment's accuracy.

### Graceful Degradation

| Situation | Behavior |
|---|---|
| Ollama not running | Fall back to heuristic classifier, silently |
| Ollama running but model not loaded | Log warning, fall back to heuristic |
| Ollama responds but slowly (>200ms) | Use result but log latency warning |
| Ollama returns error | Fall back to heuristic for this request |
| Session affinity hit | Skip classification entirely (both classifiers) |

The embedding classifier never blocks or degrades the request pipeline.
It's an enhancement that fails gracefully to the heuristic baseline.

### Timeline

| Component | Version | Effort |
|-----------|---------|--------|
| Classifier interface + heuristic impl | v2.0 | 2 days |
| Embedding classifier + Ollama integration | v2.x | 3 days |
| Reference vector curation (initial set) | v2.x | 1 day |
| Dashboard classifier settings UI | v2.x | 1 day |
| Capability auto-detection | v2.x | 0.5 day |

---

### Revised Audience-Driven Roadmap

**v1.0 — Works for everyone, excels for personal use:**
- Multi-account failover with manual combos
- Format translation (OpenAI ↔ Claude ↔ Gemini)
- Zero-copy same-format streaming
- Unified health model + background checker
- Dashboard (adapts to single or multiple keys)
- SQLite with encrypted secrets
- Strategy presets (`auto:fast`, `auto:cheap`, `auto:best`)
- Session affinity
- Multiple API keys with names

**v2.0 — Team features activate on demand:**
- Per-key budget enforcement (when budgets configured)
- Per-key model restrictions (when allowed_models configured)
- Per-key rate limiting (when rate limits configured)
- Per-key usage tracking (`api_key_id` in usage log)
- Hard constraint filtering (capabilities, context window)
- Heuristic classifier for signal-based routing
- Context bridge with structured state + sliding window
- Dashboard team views (auto-activate with 2+ keys)
- Telemetry collection for routing quality analysis

**v2.x — Advanced routing (on demand, honest about requirements):**
- Embedding classifier via Ollama sidecar
- Capability auto-detection (use embedding if available)
- Reference vector management in dashboard
- Shadow comparison for routing quality evaluation
- User feedback loop (thumbs up/down on routing decisions)

**v3.0 — Intelligence layer (built on telemetry data):**
- Data-driven routing decision tree
- Optional bundled classifier model (`sage-router install-classifier`)
- Routing quality reports (heuristic vs embedding comparison)

---

### Design Principles (appended to §27)

**"Features activate when configured, invisible when not."**
No modes, no editions, no feature gates. Budget enforcement runs when
a budget is set. ACLs run when model restrictions exist. The embedding
classifier runs when Ollama is available. Zero overhead otherwise.

**"A key is a group."**
Team access control is expressed through API key attributes, not user/group/role
hierarchies. Multiple people sharing a key share its permissions and budget.

**"Honest positioning over marketing."**
Advanced features document their real requirements. The embedding classifier
page says "requires 1GB+ RAM and a dedicated server" — not "AI-powered
intelligent routing."

---

### Decision Log (appended)

| Date | Decision |
|------|----------|
| 2026-03-19 | No modes, no editions — features activate on demand |
| 2026-03-19 | Keys-as-groups: budget, ACLs, rate limits per key |
| 2026-03-19 | 5 columns + 1 FK for team features, no new tables |
| 2026-03-19 | Dashboard auto-adapts: 1 key = personal view, 2+ = team view |
| 2026-03-19 | Embedding classifier: Ollama sidecar, not bundled |
| 2026-03-19 | Honest positioning: embedding = advanced, server, 1GB+ RAM |
| 2026-03-19 | Classifier interface shared by heuristic + embedding |
| 2026-03-19 | Capability auto-detection: use best available classifier |
| 2026-03-19 | Graceful degradation: embedding → heuristic fallback, always |
| 2026-03-19 | No RBAC until a real customer requests it |
