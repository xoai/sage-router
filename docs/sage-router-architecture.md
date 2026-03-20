# Sage-Router — Detailed Architecture & Implementation Blueprint

## 1. Directory Structure

```
sage-router/
├── cmd/
│   └── sage-router/
│       └── main.go                  # Entry point: parse flags, init, run
│
├── internal/                        # Private implementation packages
│   ├── server/
│   │   ├── server.go                # HTTP server, mux, middleware chain
│   │   ├── routes_v1.go             # /v1/* compatibility routes
│   │   ├── routes_api.go            # /api/* management routes
│   │   └── middleware.go            # Logging, recovery, CORS, auth guard
│   │
│   ├── pipeline/
│   │   ├── pipeline.go              # Stage chain builder + runner
│   │   ├── context.go               # RequestContext (carries data across stages)
│   │   ├── ingress.go               # Stage 1: parse body, detect source format
│   │   ├── authenticate.go          # Stage 2: API key validation
│   │   ├── resolve.go               # Stage 3: model/alias/combo resolution
│   │   ├── select.go                # Stage 4: provider + account selection
│   │   ├── translate_request.go     # Stage 5: source → canonical → target
│   │   ├── execute.go               # Stage 6: upstream HTTP call
│   │   ├── translate_response.go    # Stage 7: stream translate back
│   │   └── track.go                 # Stage 8: usage persistence
│   │
│   ├── provider/
│   │   ├── registry.go              # Provider registry (thread-safe map)
│   │   ├── connection.go            # Connection entity + state machine
│   │   ├── state.go                 # State enum, transition table, events
│   │   ├── selector.go              # Priority-weighted selection with round-robin
│   │   ├── cooldown.go              # Exponential backoff calculator
│   │   └── combo.go                 # Combo resolution and fallback orchestrator
│   │
│   ├── translate/
│   │   ├── registry.go              # Translator registry + hub orchestration
│   │   ├── detect.go                # Format detection (endpoint + body shape)
│   │   ├── openai/                  # OpenAI ↔ Canonical (identity with normalization)
│   │   │   ├── request.go
│   │   │   ├── response.go
│   │   │   └── stream.go
│   │   ├── claude/                  # Claude ↔ Canonical
│   │   │   ├── request.go           # messages restructure, system extraction, tools
│   │   │   ├── response.go          # content_block mapping
│   │   │   └── stream.go            # content_block_start/delta/stop → SSE chunks
│   │   ├── gemini/                  # Gemini ↔ Canonical
│   │   │   ├── request.go           # contents/parts mapping, function declarations
│   │   │   ├── response.go
│   │   │   └── stream.go
│   │   ├── responses/               # OpenAI Responses API ↔ Canonical
│   │   │   ├── request.go           # input[] → messages[]
│   │   │   ├── response.go
│   │   │   └── stream.go            # response.* event format
│   │   ├── kiro/
│   │   │   ├── request.go
│   │   │   └── stream.go
│   │   ├── cursor/
│   │   │   ├── request.go
│   │   │   └── stream.go
│   │   └── ollama/
│   │       ├── request.go
│   │       └── stream.go
│   │
│   ├── executor/
│   │   ├── executor.go              # Executor interface + default impl
│   │   ├── http.go                  # Shared HTTP client pool, proxy-aware
│   │   ├── retry.go                 # Retry with backoff, URL fallback
│   │   ├── antigravity.go           # Provider-specific: URL, headers, auth
│   │   ├── gemini_cli.go
│   │   ├── github.go
│   │   ├── codex.go
│   │   ├── cursor.go
│   │   ├── kiro.go
│   │   ├── iflow.go
│   │   └── vertex.go
│   │
│   ├── store/
│   │   ├── store.go                 # Store interface (testable)
│   │   ├── sqlite.go                # SQLite implementation
│   │   ├── migrations/              # Embedded SQL migration files
│   │   │   ├── 001_initial.sql
│   │   │   ├── 002_usage_log.sql
│   │   │   └── 003_proxy_pools.sql
│   │   ├── crypto.go                # AES-256-GCM encrypt/decrypt for secrets
│   │   └── queries.go               # Typed query helpers (no ORM)
│   │
│   ├── auth/
│   │   ├── apikey.go                # Generate, parse, verify API keys (HMAC)
│   │   ├── dashboard.go             # JWT cookie auth for dashboard
│   │   ├── password.go              # bcrypt hash + first-run generation
│   │   └── oauth/
│   │       ├── flow.go              # Generic OAuth2 + device code flow
│   │       ├── claude.go            # Claude-specific OAuth quirks
│   │       ├── gemini.go
│   │       ├── github.go
│   │       └── refresh.go           # Token refresh with retry
│   │
│   ├── usage/
│   │   ├── tracker.go               # In-memory batch → periodic SQLite flush
│   │   ├── cost.go                  # Cost calculator (pricing table lookup)
│   │   └── export.go                # Usage API data aggregation queries
│   │
│   ├── config/
│   │   ├── models.go                # Built-in model catalog (embedded JSON)
│   │   ├── pricing.go               # Default pricing per provider/model
│   │   ├── providers.go             # Known provider definitions
│   │   └── defaults.go              # Default settings, ports, paths
│   │
│   └── mitm/                        # Opt-in MITM module
│       ├── mitm.go                  # MITM proxy server (--enable-mitm)
│       ├── cert.go                  # Root CA + per-domain cert generation
│       └── dns.go                   # DNS bypass for outbound requests
│
├── pkg/                             # Public packages (importable by others)
│   ├── canonical/
│   │   ├── types.go                 # CanonicalRequest, CanonicalMessage, etc.
│   │   ├── chunk.go                 # CanonicalChunk for streaming
│   │   └── validate.go              # Request validation helpers
│   │
│   └── sse/
│       ├── reader.go                # SSE stream parser (io.Reader → events)
│       ├── writer.go                # SSE stream writer (events → http.ResponseWriter)
│       └── event.go                 # SSE event type
│
├── web/
│   ├── dashboard/                   # Preact SPA (separate build)
│   │   ├── package.json             # Minimal: preact + vite
│   │   ├── src/
│   │   │   ├── app.jsx
│   │   │   ├── pages/
│   │   │   │   ├── providers.jsx
│   │   │   │   ├── models.jsx
│   │   │   │   ├── combos.jsx
│   │   │   │   ├── usage.jsx
│   │   │   │   ├── settings.jsx
│   │   │   │   └── endpoint.jsx
│   │   │   ├── components/
│   │   │   └── api.js               # Typed API client
│   │   └── dist/                    # Built output (git-ignored, CI builds)
│   └── embed.go                     # //go:embed dashboard/dist/*
│
├── testdata/                        # Golden files for translator tests
│   ├── claude_to_canonical/
│   │   ├── simple_chat.input.json
│   │   ├── simple_chat.expected.json
│   │   ├── tool_call.input.json
│   │   ├── tool_call.expected.json
│   │   ├── thinking.input.json
│   │   └── thinking.expected.json
│   ├── gemini_to_canonical/
│   └── stream_chunks/
│
├── scripts/
│   ├── install.sh                   # curl | sh installer
│   └── release.sh                   # Cross-compile + checksum + upload
│
├── go.mod
├── go.sum
├── Makefile                         # build, test, lint, release targets
├── Dockerfile                       # Multi-stage: build → scratch/alpine
└── README.md
```

---

## 2. Key Interfaces — The Contracts

### 2.1 The Canonical Types (pkg/canonical/types.go)

These types are sage-router's **lingua franca**. Every translator converts to/from these types. They are public (`pkg/`) so external tools can import them.

```go
package canonical

// Message roles
const (
    RoleSystem    = "system"
    RoleUser      = "user"
    RoleAssistant = "assistant"
    RoleTool      = "tool"
)

// Content block types
const (
    ContentText     = "text"
    ContentImage    = "image"
    ContentToolCall = "tool_call"
    ContentToolResult = "tool_result"
    ContentThinking = "thinking"
)

// Request is the canonical intermediate representation.
// It is OpenAI-shaped today but owned by sage-router — not by OpenAI.
type Request struct {
    Model       string     `json:"model"`
    Messages    []Message  `json:"messages"`
    Tools       []Tool     `json:"tools,omitempty"`
    Stream      bool       `json:"stream"`
    MaxTokens   int        `json:"max_tokens,omitempty"`
    Temperature *float64   `json:"temperature,omitempty"`
    TopP        *float64   `json:"top_p,omitempty"`
    Thinking    *Thinking  `json:"thinking,omitempty"`
    Stop        []string   `json:"stop,omitempty"`
    Metadata    map[string]any `json:"-"` // Internal routing data, never serialized upstream
}

// Message represents a single turn in the conversation.
type Message struct {
    Role    string    `json:"role"`
    Content []Content `json:"content"`          // Always normalized to array form
    Name    string    `json:"name,omitempty"`    // For tool role
    ToolCallID string `json:"tool_call_id,omitempty"`
}

// Content is a polymorphic content block.
type Content struct {
    Type      string `json:"type"`
    Text      string `json:"text,omitempty"`
    
    // Image
    MediaType string `json:"media_type,omitempty"` // "image/jpeg", "image/png"
    Data      string `json:"data,omitempty"`        // base64
    URL       string `json:"url,omitempty"`          // image URL
    
    // Tool call (assistant → tool)
    ToolCallID string `json:"tool_call_id,omitempty"`
    ToolName   string `json:"tool_name,omitempty"`
    Arguments  string `json:"arguments,omitempty"` // JSON string
    
    // Tool result (tool → assistant)
    Content    string `json:"content,omitempty"`    // For tool results
    IsError    bool   `json:"is_error,omitempty"`
    
    // Thinking
    Thinking   string `json:"thinking,omitempty"`
}

// Tool describes a callable function.
type Tool struct {
    Name        string `json:"name"`
    Description string `json:"description,omitempty"`
    Parameters  any    `json:"parameters"` // JSON Schema object
}

// Thinking controls extended thinking.
type Thinking struct {
    Enabled    bool `json:"enabled"`
    BudgetTokens int `json:"budget_tokens,omitempty"`
}

// Chunk is a single streaming event in the canonical format.
type Chunk struct {
    ID           string   `json:"id"`
    Model        string   `json:"model,omitempty"`
    Delta        *Delta   `json:"delta,omitempty"`
    FinishReason string   `json:"finish_reason,omitempty"`
    Usage        *Usage   `json:"usage,omitempty"`
}

// Delta is the incremental content in a stream chunk.
type Delta struct {
    Role       string `json:"role,omitempty"`
    Content    string `json:"content,omitempty"`
    Thinking   string `json:"thinking,omitempty"`
    ToolCallID string `json:"tool_call_id,omitempty"`
    ToolName   string `json:"tool_name,omitempty"`
    Arguments  string `json:"arguments,omitempty"`
}

// Usage tracks token consumption.
type Usage struct {
    PromptTokens     int `json:"prompt_tokens"`
    CompletionTokens int `json:"completion_tokens"`
    TotalTokens      int `json:"total_tokens"`
}
```

### 2.2 Translator Interface (internal/translate/registry.go)

```go
package translate

import (
    "sage-router/pkg/canonical"
    "net/http"
)

// Format identifies an API wire format.
type Format string

const (
    FormatOpenAI     Format = "openai"
    FormatClaude     Format = "claude"
    FormatGemini     Format = "gemini"
    FormatResponses  Format = "openai-responses"
    FormatKiro       Format = "kiro"
    FormatCursor     Format = "cursor"
    FormatOllama     Format = "ollama"
)

// Translator converts between a provider's wire format and canonical.
// Each provider implements this interface.
type Translator interface {
    // Format returns the wire format this translator handles.
    Format() Format

    // DetectInbound returns true if the raw request body matches this format.
    // Called during ingress to determine source format.
    DetectInbound(endpoint string, body []byte) bool

    // ToCanonical converts a provider-format request body into canonical form.
    ToCanonical(body []byte, opts TranslateOpts) (*canonical.Request, error)

    // FromCanonical converts a canonical request into provider wire format.
    FromCanonical(req *canonical.Request, opts TranslateOpts) ([]byte, error)

    // StreamChunkToCanonical converts a single SSE chunk from provider format
    // into zero or more canonical chunks.
    // Returns nil slice to skip the chunk (e.g., keepalive).
    StreamChunkToCanonical(data []byte, state *StreamState) ([]canonical.Chunk, error)

    // CanonicalToStreamChunk converts a canonical chunk into provider wire format
    // for the response stream back to the client.
    CanonicalToStreamChunk(chunk canonical.Chunk, state *StreamState) ([]byte, error)
}

// TranslateOpts carries context needed during translation.
type TranslateOpts struct {
    Model       string
    Provider    string
    Stream      bool
    Credentials any // Opaque, translator casts to what it needs
}

// StreamState carries mutable state across streaming chunks.
// Each streaming response gets a fresh StreamState.
type StreamState struct {
    MessageID    string
    Model        string
    BlockIndex   int
    InThinking   bool
    ToolCalls    map[string]*ToolCallAccumulator
    FinishReason string
    Usage        *canonical.Usage
    Custom       map[string]any // Format-specific state
}

// Registry holds all registered translators.
type Registry struct {
    byFormat map[Format]Translator
}

// Translate performs: sourceFormat → canonical → targetFormat.
func (r *Registry) TranslateRequest(
    source, target Format,
    body []byte,
    opts TranslateOpts,
) (*canonical.Request, []byte, error) {
    // 1. Source → Canonical (skip if source is "openai" / canonical)
    // 2. Canonical → Target (skip if target is "openai" / canonical)
    // Returns both canonical (for logging/metrics) and target bytes
    // ...
}
```

### 2.3 Executor Interface (internal/executor/executor.go)

```go
package executor

import (
    "context"
    "io"
    "net/http"
)

// Result is what an executor returns after calling the upstream provider.
type Result struct {
    StatusCode  int
    Headers     http.Header
    Body        io.ReadCloser  // Streaming body — caller must close
    URL         string         // The actual URL that was called
    Latency     time.Duration  // Time to first byte
}

// Executor handles the upstream HTTP call for a specific provider.
type Executor interface {
    // Provider returns the provider ID this executor handles.
    Provider() string

    // Execute sends the translated request to the upstream provider.
    Execute(ctx context.Context, req *ExecuteRequest) (*Result, error)

    // RefreshCredentials attempts to refresh expired OAuth tokens.
    // Returns nil if refresh is not supported or failed.
    RefreshCredentials(ctx context.Context, creds *Credentials) (*Credentials, error)
}

// ExecuteRequest contains everything needed for the upstream call.
type ExecuteRequest struct {
    Model       string
    Body        []byte          // Already translated to target format
    Stream      bool
    Credentials *Credentials
    ProxyURL    string          // Optional per-connection proxy
    NoProxy     string          // Optional no-proxy list
}

// Credentials holds provider authentication data.
type Credentials struct {
    ConnectionID string
    AuthType     string // "oauth" | "apikey"
    AccessToken  string
    RefreshToken string
    APIKey       string
    ExpiresAt    time.Time
    ProviderData map[string]any // Provider-specific (e.g., projectId)
}
```

### 2.4 Pipeline Context (internal/pipeline/context.go)

```go
package pipeline

import (
    "context"
    "net/http"
    "sage-router/pkg/canonical"
    "sage-router/internal/executor"
    "sage-router/internal/translate"
)

// RequestContext carries all data through the pipeline stages.
// Stages read from and write to this context.
// This replaces 9router's pattern of passing 15+ parameters between functions.
type RequestContext struct {
    context.Context

    // Immutable (set during ingress)
    RawRequest    *http.Request
    RawBody       []byte
    Endpoint      string
    ClientFormat  translate.Format  // Detected source format
    UserAgent     string
    APIKey        string
    RequestID     string            // Unique per request, for tracing
    StartTime     time.Time

    // Set during resolve
    Provider      string
    Model         string
    IsCombo       bool
    ComboModels   []string
    Stream        bool

    // Set during select
    Connection    *Connection
    Credentials   *executor.Credentials
    TargetFormat  translate.Format

    // Set during translate
    Canonical     *canonical.Request  // The intermediate representation
    TargetBody    []byte              // Translated for upstream

    // Set during execute
    UpstreamResult *executor.Result

    // Accumulated during response
    Usage         *canonical.Usage
    LatencyTTFT   time.Duration
    LatencyTotal  time.Duration
    FinalStatus   string             // "success" | "error" | "fallback"

    // Response writer (for streaming)
    ResponseWriter http.ResponseWriter
    Flusher       http.Flusher
}
```

### 2.5 Store Interface (internal/store/store.go)

```go
package store

import "time"

// Store is the persistence interface. 
// SQLite implements it. Tests can use an in-memory implementation.
type Store interface {
    // Connections
    ListConnections(filter ConnectionFilter) ([]Connection, error)
    GetConnection(id string) (*Connection, error)
    CreateConnection(c *Connection) error
    UpdateConnection(id string, updates map[string]any) error
    DeleteConnection(id string) error

    // Combos
    ListCombos() ([]Combo, error)
    GetComboByName(name string) (*Combo, error)
    CreateCombo(c *Combo) error
    UpdateCombo(id string, c *Combo) error
    DeleteCombo(id string) error

    // Aliases
    GetAlias(alias string) (string, error)
    SetAlias(alias, target string) error
    DeleteAlias(alias string) error
    ListAliases() (map[string]string, error)

    // API Keys
    ListAPIKeys() ([]APIKey, error)
    CreateAPIKey(name string) (*APIKey, error)
    ValidateAPIKey(key string) (bool, error)
    DeleteAPIKey(id string) error

    // Settings
    GetSetting(key string) (string, error)
    SetSetting(key, value string) error
    AllSettings() (map[string]string, error)

    // Usage
    RecordUsage(entry *UsageEntry) error
    QueryUsage(filter UsageFilter) ([]UsageEntry, error)
    UsageSummary(filter UsageFilter) (*UsageSummary, error)

    // Lifecycle
    Migrate() error
    Close() error
}
```

---

## 3. The Provider Selection Algorithm

9router uses a simple priority sort + exclude set. Sage-router should formalize this into a proper weighted selection with sticky round-robin:

```
┌──────────────────────────────────────────────────┐
│          Provider Selection Algorithm             │
├──────────────────────────────────────────────────┤
│                                                  │
│  Input: provider ID, model, exclude set          │
│                                                  │
│  1. Load all active connections for provider     │
│  2. Filter out:                                  │
│     - Connections in exclude set                 │
│     - Connections in Disabled state              │
│     - Connections with active model lock         │
│       for requested model                        │
│     - Connections in Cooldown where              │
│       cooldown has not expired                   │
│  3. Sort remaining by:                           │
│     a. State priority: Active > Idle > Errored   │
│     b. Connection priority (user-defined)        │
│     c. Consecutive use count (ascending)         │
│        → sticky round-robin: prefer least        │
│          recently used, but allow N consecutive   │
│          uses before rotating (configurable)     │
│  4. Return top candidate                         │
│     - If none available:                         │
│       Return { allRateLimited: true,             │
│                earliestRetry: <timestamp> }      │
│                                                  │
└──────────────────────────────────────────────────┘
```

---

## 4. Translator Rewrite Strategy

### Complexity ranking (from 9router analysis):

| Translator pair        | Complexity | Key challenges                          |
|------------------------|------------|---------------------------------------- |
| OpenAI ↔ Canonical     | Low        | Near-identity, normalization only       |
| Ollama ↔ Canonical     | Low        | Minimal differences from OpenAI         |
| Claude ↔ Canonical     | **High**   | System prompt extraction, content block |
|                        |            | types, tool_result restructuring,       |
|                        |            | thinking blocks, max_tokens semantics   |
| Gemini ↔ Canonical     | **High**   | contents/parts model, function          |
|                        |            | declarations vs tools, safety settings, |
|                        |            | streaming candidate format              |
| Responses ↔ Canonical  | Medium     | input[] → messages[], response.*        |
|                        |            | event streaming format                  |
| Kiro ↔ Canonical       | Medium     | Kiro-specific envelope                  |
| Cursor ↔ Canonical     | Medium     | Cursor-specific fields                  |

### Rewrite order (risk-adjusted):

```
Phase 1 (Week 1-2): Foundation + easy wins
  ├── Canonical types (the contract)
  ├── OpenAI ↔ Canonical (near-identity, validates the interface)
  ├── Ollama ↔ Canonical (easy, builds confidence)
  └── SSE reader/writer utilities

Phase 2 (Week 2-3): The hard two
  ├── Claude ↔ Canonical (most complex, most critical)
  └── Gemini ↔ Canonical (second most complex)

Phase 3 (Week 3-4): Remaining formats
  ├── Responses ↔ Canonical
  ├── Kiro ↔ Canonical
  └── Cursor ↔ Canonical
```

### Golden file testing strategy:

For each translator, we capture real request/response pairs from 9router
and use them as golden test fixtures:

```
testdata/claude_to_canonical/
├── 01_simple_chat.input.json        ← Real Claude-format request
├── 01_simple_chat.canonical.json    ← Expected canonical output
├── 02_with_tools.input.json
├── 02_with_tools.canonical.json
├── 03_thinking_blocks.input.json
├── 03_thinking_blocks.canonical.json
├── 04_multi_image.input.json
├── 04_multi_image.canonical.json
├── 05_tool_result_chain.input.json
└── 05_tool_result_chain.canonical.json

testdata/stream_claude_to_canonical/
├── 01_simple.chunks.jsonl           ← One SSE event per line
├── 01_simple.expected.jsonl         ← Expected canonical chunks
├── 02_thinking.chunks.jsonl
└── 02_thinking.expected.jsonl
```

Test function:

```go
func TestClaudeToCanonical(t *testing.T) {
    entries, _ := os.ReadDir("testdata/claude_to_canonical")
    for _, e := range entries {
        if !strings.HasSuffix(e.Name(), ".input.json") { continue }
        name := strings.TrimSuffix(e.Name(), ".input.json")
        t.Run(name, func(t *testing.T) {
            input := readFile(t, "testdata/claude_to_canonical/"+name+".input.json")
            expected := readJSON[canonical.Request](t, "testdata/claude_to_canonical/"+name+".canonical.json")
            
            got, err := claudeTranslator.ToCanonical(input, translate.TranslateOpts{})
            require.NoError(t, err)
            assert.Equal(t, expected, got)
        })
    }
}
```

This means: **every real-world edge case we discover becomes a permanent test fixture.** The test suite grows monotonically and never regresses.

---

## 5. HTTP Client Architecture

9router creates fresh HTTP requests per call. Sage-router should manage a **shared, pooled HTTP client** with connection reuse:

```go
// internal/executor/http.go

var defaultTransport = &http.Transport{
    MaxIdleConns:        100,
    MaxIdleConnsPerHost: 10,
    IdleConnTimeout:     90 * time.Second,
    TLSHandshakeTimeout: 10 * time.Second,
    // Keep-alive enabled by default
}

// One client per proxy configuration (direct + each proxy URL).
// Connections are pooled and reused across requests.
type ClientPool struct {
    mu      sync.RWMutex
    direct  *http.Client
    proxied map[string]*http.Client // key: proxy URL
}

func (p *ClientPool) Get(proxyURL string) *http.Client {
    if proxyURL == "" {
        return p.direct
    }
    // Lazy-create proxied client with reuse
}
```

---

## 6. Usage Tracking — Batch Writer Pattern

9router writes to usage.json on every request. Sage-router should batch:

```go
// internal/usage/tracker.go

type Tracker struct {
    store   store.Store
    buffer  chan *UsageEntry
    done    chan struct{}
}

// NewTracker starts a background goroutine that flushes
// batches every 5 seconds or when buffer reaches 50 entries.
func NewTracker(s store.Store) *Tracker {
    t := &Tracker{
        store:  s,
        buffer: make(chan *UsageEntry, 1000),
        done:   make(chan struct{}),
    }
    go t.flushLoop()
    return t
}

func (t *Tracker) Record(entry *UsageEntry) {
    select {
    case t.buffer <- entry:
    default:
        // Buffer full — drop oldest or write synchronously
        log.Warn("usage buffer full, writing synchronously")
        t.store.RecordUsage(entry)
    }
}

func (t *Tracker) flushLoop() {
    ticker := time.NewTicker(5 * time.Second)
    batch := make([]*UsageEntry, 0, 50)
    for {
        select {
        case entry := <-t.buffer:
            batch = append(batch, entry)
            if len(batch) >= 50 {
                t.flush(batch)
                batch = batch[:0]
            }
        case <-ticker.C:
            if len(batch) > 0 {
                t.flush(batch)
                batch = batch[:0]
            }
        case <-t.done:
            // Drain remaining
            t.flush(batch)
            return
        }
    }
}
```

---

## 7. Dashboard — Preact SPA

### Constraints:
- Total bundle: < 500KB gzipped (9router's is ~2MB)
- Zero SSR — pure client-side SPA
- Talks to /api/* REST endpoints
- Embedded in Go binary via //go:embed

### Tech choices:
- **Preact** (3KB) instead of React (40KB) — API-compatible, 13× smaller
- **Vite** for dev/build — fast, no webpack config complexity
- **Vanilla CSS + CSS Modules** or **UnoCSS** (atomic CSS, 0 runtime)
- **No state management library** — Preact signals (built-in) or simple context
- **No UI component library** — hand-crafted components, stays tiny

### Pages:
```
/                    → Redirect to /dashboard
/dashboard           → Overview (status, active providers, recent usage)
/dashboard/providers → Provider management (connect, test, reorder)
/dashboard/models    → Aliases, combos, model catalog
/dashboard/usage     → Usage charts, cost breakdown, request log
/dashboard/settings  → API keys, security, proxy, import/export
/dashboard/endpoint  → Connection instructions for each CLI tool
```

### Build integration:

```makefile
# Makefile
dashboard:
	cd web/dashboard && npm run build

build: dashboard
	go build -o bin/sage-router ./cmd/sage-router

# embed.go uses //go:embed, so the dashboard must be built first
```

---

## 8. Build & Release Targets

```makefile
# Makefile

VERSION := $(shell git describe --tags --always)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint release

build: dashboard
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/sage-router ./cmd/sage-router

test:
	go test ./... -race -cover -count=1

lint:
	golangci-lint run ./...

# Cross-compile for all platforms
release: dashboard
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/sage-router-linux-amd64 ./cmd/sage-router
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/sage-router-linux-arm64 ./cmd/sage-router
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/sage-router-darwin-amd64 ./cmd/sage-router
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/sage-router-darwin-arm64 ./cmd/sage-router
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/sage-router-windows-amd64.exe ./cmd/sage-router
	cd dist && sha256sum * > checksums.txt

docker:
	docker build -t sage-router:$(VERSION) .
```

### Dockerfile (minimal):

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o sage-router ./cmd/sage-router

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/sage-router /sage-router
EXPOSE 20128
ENTRYPOINT ["/sage-router"]
```

Note: **`FROM scratch`** — the final image contains literally just the binary
and TLS certs. No shell, no OS, no package manager. Image size: ~20MB.

---

## 9. Phased Build Plan

### Phase 1: Walking skeleton (Week 1-2)
- [ ] Go project scaffold, Makefile, CI
- [ ] SQLite store with migrations (connections, settings, API keys)
- [ ] HTTP server with /v1/chat/completions and /v1/models
- [ ] Pipeline: Ingress → Authenticate → Resolve → Execute → Track
- [ ] OpenAI translator (identity — canonical IS openai-shaped)
- [ ] Default executor (generic OpenAI-compatible upstream)
- [ ] API key generation and validation
- [ ] **Milestone: Can proxy OpenAI API calls end-to-end with streaming**

### Phase 2: Translation core (Week 2-4)
- [ ] Claude ↔ Canonical translator (request + streaming response)
- [ ] Gemini ↔ Canonical translator
- [ ] Golden file test suite (capture from 9router traffic)
- [ ] Claude executor (OAuth token management)
- [ ] Gemini-CLI executor
- [ ] Account selection with round-robin
- [ ] Account fallback with exponential backoff + state machine
- [ ] **Milestone: Can route Claude Code through Gemini-CLI account**

### Phase 3: Full provider coverage (Week 4-6)
- [ ] Remaining translators (Responses, Kiro, Cursor, Ollama)
- [ ] Remaining executors (Antigravity, GitHub, iFlow, Codex, Vertex)
- [ ] Combo fallback orchestrator
- [ ] Model alias resolution
- [ ] OAuth flows (device code + authorization code)
- [ ] Token refresh with retry
- [ ] **Milestone: Feature parity with 9router's routing core**

### Phase 4: Dashboard + polish (Week 6-8)
- [ ] Preact dashboard SPA (providers, models, usage, settings)
- [ ] Embed dashboard in Go binary
- [ ] Dashboard JWT auth + first-run password generation
- [ ] Secret encryption at rest (AES-256-GCM)
- [ ] Usage tracking with batch writer
- [ ] Cost calculation
- [ ] `curl | sh` installer script
- [ ] Cross-platform release builds
- [ ] **Milestone: v0.1.0 — usable end-to-end replacement for 9router**

### Phase 5: Hardening (Week 8-10)
- [ ] MITM proxy module (opt-in, --enable-mitm)
- [ ] Proxy pool support (SOCKS5, HTTP proxy)
- [ ] Import/export database
- [ ] Load testing (10K concurrent streams)
- [ ] Security audit (secret handling, auth flows)
- [ ] Documentation site
- [ ] npm wrapper package (`npx sage-router`)
- [ ] Homebrew formula
- [ ] **Milestone: v1.0.0 — production-ready**
