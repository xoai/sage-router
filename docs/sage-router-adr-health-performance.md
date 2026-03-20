# Sage-Router ADR Addendum: Connection Health & Performance

> ADR Sections 30-32 — to be appended to sage-router-adr-v2.md
> Status: **Decided**
> Date: 2026-03-19

---

## 30. Decided: Unified Connection Health Model

### Problem

We had four overlapping failure-handling mechanisms designed independently:
executor retries, exponential backoff cooldowns, a connection state machine,
and a proposed circuit breaker. Each operated at a different timescale but
they didn't know about each other, creating ambiguity about which mechanism
"owns" connection health.

Additionally, a concurrency bug existed: if two requests are in flight to the
same connection and both return 429, the second failure attempts a state
transition from `Cooldown` (set by the first failure) — a transition not in
the table.

### Decision

**The state machine is the single source of truth for connection health.**
All other mechanisms feed events into it. The selector reads from it.
No component talks to any other directly.

```
              ┌────────────────────────┐
              │    Connection State    │
              │      Machine           │
              │  (single source of     │
              │      truth)            │
              └──┬────┬────┬────┬──────┘
                 │    │    │    │
       ┌─────────┘    │    │    └──────────┐
       ▼              ▼    ▼               ▼
  Executor        Backoff  Health       Selector
  (writes         Timer    Checker      (reads
   events)       (writes  (writes       state)
                  events)  events)
```

**Responsibility separation:**

- `HealthManager`: Owns all state transitions. Receives events from executors
  (request success/failure), timers (cooldown expiry), and the background
  checker (token expiry). Serializes transitions to eliminate race conditions.
  This is the only code that mutates connection state.

- `Selector`: Pure read-only query. Given a list of connections and their
  current states, returns the best candidate. Does not mutate anything.
  Independently testable with no concurrency concerns.

**Circuit breaker expressed as state machine rules (not a separate system):**

Two new transitions added to the existing table:

```
Errored + (consecutive_failures > 5) → Disabled
Disabled + (health check passes after 5 min) → Idle
```

This IS a circuit breaker. We just don't need a separate CircuitOpen/HalfOpen
vocabulary. The state machine already has the right states — `Errored` is
half-open, `Disabled` is open, `Idle` is closed. Two transition rules, zero
new concepts.

**Concurrent-failure race condition fix:**

Add `Cooldown → Cooldown` to the transition table. When a second in-flight
request fails while the connection is already in cooldown, the cooldown timer
resets with an incremented backoff level. This is the simplest correct fix.

Updated transition table:

```go
var validTransitions = map[State]map[State]bool{
    StateIdle:        {StateActive: true, StateDisabled: true},
    StateActive:      {StateActive: true, StateRateLimited: true, StateAuthExpired: true, StateErrored: true, StateDisabled: true},
    StateRateLimited: {StateCooldown: true, StateDisabled: true},
    StateCooldown:    {StateCooldown: true, StateIdle: true, StateDisabled: true},  // ← Cooldown→Cooldown added
    StateAuthExpired: {StateRefreshing: true, StateDisabled: true},
    StateRefreshing:  {StateActive: true, StateErrored: true, StateDisabled: true},
    StateErrored:     {StateIdle: true, StateDisabled: true},                       // ← Disabled on 5+ failures
    StateDisabled:    {StateIdle: true},                                            // ← Idle on health check pass
}
```

### Advisory Input

- **Korotkevich** identified the concurrent-failure race condition
- **Dean** proposed the unified event-source model
- **Gang of Four** advocated separating HealthManager (writes) from Selector (reads)
- **Torvalds** insisted the circuit breaker must not be a separate system

---

## 31. Decided: Background Health Checker

### Problem

Our connection state machine is passive — it discovers problems only when a
real request fails. If a user's API key expired 2 hours ago and they send
their first request of the day, that request fails, triggers fallback, and
the user sees degraded latency. We could have detected the expiry during
idle time.

### Decision

A lightweight background goroutine checks **local state only** — no network
calls, no API probes, no cost.

```go
type HealthChecker struct {
    interval time.Duration  // 60 seconds
    health   *HealthManager
    store    store.Store
}
```

**What it checks (every 60 seconds):**

| Check | Cost | Action |
|-------|------|--------|
| Token expiry (`expires_at < now`) | Zero — reads local DB | Transition → `AuthExpired` |
| Token expiring soon (`< 5 min`) | Zero — reads local DB | Attempt preemptive refresh |
| Cooldown expired | Zero — reads in-memory state | Transition → `Idle` |
| Disabled connection (5 min elapsed) | Zero — reads in-memory state | Transition → `Idle` (test next request) |

**What it does NOT do:**

- No test API calls to providers (costs money, might trigger rate limits)
- No network connectivity checks (unreliable for API availability)
- No model listing or capability probing

The health checker is ~30 lines of code. It runs as a goroutine started in
`main.go` and stopped during graceful shutdown.

**Preemptive token refresh:** When a token expires within 5 minutes, the
health checker calls the executor's `RefreshCredentials()` method. If refresh
succeeds, the connection stays `Active` and the user never notices. If refresh
fails, the connection transitions to `AuthExpired` — still better than
discovering it on a live request.

### What We Explicitly Don't Build

- **Slow start:** After cooldown recovery, HAProxy gradually ramps traffic.
  At our scale (dozens of connections, not thousands of servers), the first
  request after recovery is a sufficient test. No gradual ramp needed.

- **Active probing:** HAProxy sends synthetic health checks to backends.
  We'd need to craft valid LLM requests per provider, handle billing, and
  deal with rate limits. The cost/benefit doesn't justify it.

### Advisory Input

- **Torvalds**: "Check if tokens expired. That's it. Don't gold-plate it."
- **Dean**: "The health checker feeds events into the state machine, same as every other event source."

---

## 32. Decided: Zero-Copy Same-Format Streaming

### Problem

When the client format equals the provider format (the most common case —
OpenAI client → OpenAI provider), our pipeline still parses every SSE chunk,
unmarshals JSON, runs it through the translation layer (which is a no-op),
re-marshals, and writes. That's 2 JSON round-trips per streaming chunk.
For a 500-chunk response, that's 1000 unnecessary marshal/unmarshal operations.

### Decision

When `clientFormat == targetFormat`, bypass all translation and pipe bytes
directly from upstream to downstream.

```go
// In the streaming response handler:

if ctx.ClientFormat == ctx.TargetFormat {
    // Fast path: zero-copy passthrough
    // No parsing, no translation, no marshaling
    // Just pipe SSE bytes and flush after each event boundary
    return pipeSSE(ctx.ResponseWriter, result.Body, ctx.Flusher)
}

// Slow path: translate chunk by chunk
return translateAndStream(ctx, result.Body)
```

**`pipeSSE` implementation:**

```go
func pipeSSE(w http.ResponseWriter, body io.ReadCloser, flusher http.Flusher) error {
    buf := make([]byte, 32*1024) // 32KB read buffer
    for {
        n, err := body.Read(buf)
        if n > 0 {
            w.Write(buf[:n])
            // Flush after each potential event boundary (double newline)
            if bytes.Contains(buf[:n], []byte("\n\n")) && flusher != nil {
                flusher.Flush()
            }
        }
        if err == io.EOF {
            return nil
        }
        if err != nil {
            return err
        }
    }
}
```

**Why this matters:** HAProxy's core principle is "data doesn't reach higher
levels unless needed." In our profiling estimate, format translation + JSON
marshaling accounts for ~25% of per-request CPU time. Same-format passthrough
eliminates this entirely for the common case.

**Usage extraction on the fast path:** The zero-copy path skips translation
but we still need token counts for usage tracking. Solution: scan for the
final chunk that contains `"usage":` and extract token counts from it.
This is a single scan of one SSE event, not a full parse of every event.

```go
// On the fast path, capture only the final chunk for usage
if bytes.Contains(buf[:n], []byte(`"usage":`)) {
    // Extract usage from this chunk only — last chunk of the stream
    captureUsage(ctx, buf[:n])
}
```

**Format matching frequency:** Based on our provider configuration, same-format
passthrough applies to:
- OpenAI client → OpenAI/OpenRouter/GitHub Copilot/Ollama = ~60% of requests
- Claude client → Anthropic = ~20% of requests

So roughly **80% of requests** can take the zero-copy fast path. The remaining
20% (cross-format: Claude client → OpenAI provider, etc.) use the full
translation pipeline.

### Advisory Input

- **Carmack**: "This one optimization matters more than all six HAProxy patterns combined."
- **HAProxy docs**: "Its architecture ensures data doesn't reach higher levels unless needed."

---

### Consolidated Design Principles (appended to §27)

**"The state machine is the single source of truth."**
All failure handling — retries, backoff, circuit breaking, health checks —
feeds events into the connection state machine. The selector only reads state.
No parallel mechanisms, no competing models.

**"Don't touch data that doesn't need touching."**
Same-format requests bypass translation entirely. Zero-copy streaming for the
common case. Full translation only when formats differ.

**"No network calls in the background."**
The health checker reads local state — token timestamps, cooldown timers,
failure counters. It never sends API requests. Background operations must
be free and invisible.

---

### Decision Log (appended)

| Date | Decision |
|------|----------|
| 2026-03-19 | State machine is single source of truth for connection health |
| 2026-03-19 | HealthManager (writes state) separated from Selector (reads state) |
| 2026-03-19 | Circuit breaker = two state machine rules, not a separate system |
| 2026-03-19 | Fix: Cooldown → Cooldown transition for concurrent failures |
| 2026-03-19 | Background health checker: local state only, 60s interval, ~30 LoC |
| 2026-03-19 | Zero-copy same-format streaming passthrough |
| 2026-03-19 | Dropped: slow start, declarative ACLs, active network probing |
| 2026-03-19 | Dropped: HAProxy patterns that don't apply at our scale |
