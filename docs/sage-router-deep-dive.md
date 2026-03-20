# Sage-Router Deep Dive: Canonical Types, Claude Translator & Dashboard UX

---

## Part 1: Canonical Types — The Type System That Holds Everything Together

### 1.1 Design Philosophy

The canonical types are **sage-router's source of truth**. Every translator reads from them and writes to them. If they're incomplete, translators paper over the gaps with hacks. If they're over-specified, translators carry dead fields. The goal is **minimum viable completeness**: every field that any supported provider can produce or consume must have a canonical representation, but nothing more.

Three invariants the types must uphold:

1. **Lossless round-trip**: Claude request → Canonical → Claude request must preserve all semantically meaningful data.
2. **Union semantics**: Canonical is the *union* of all formats, not the intersection. If Claude has "thinking" and Gemini doesn't, thinking is in canonical. Translators that don't support it simply ignore it.
3. **Serialization-stable**: Canonical types must serialize to valid JSON in a deterministic order. This enables golden-file testing with byte-level comparison.

### 1.2 Edge Cases Discovered from 9router Analysis

After reading every translator in 9router, here are the edge cases the canonical types must handle:

**Messages & Content:**
- OpenAI uses `content: "string"` (shorthand) and `content: [{type: "text", text: "..."}]` (array form). Canonical MUST normalize to array form always — this eliminates an entire class of `typeof` checks throughout the codebase.
- Claude `system` is separate from `messages` and can be an array of blocks with `cache_control`. OpenAI puts system in `messages[0]`. Canonical should use a dedicated `System` field.
- Claude merges consecutive same-role messages. OpenAI doesn't. The translator must handle merging/splitting, but canonical stores messages as the client sent them — no implicit merging.

**Tool Calls:**
- OpenAI: `tool_calls[].function.arguments` is a **JSON string**. Claude: `tool_use.input` is a **parsed object**. This is the #1 source of bugs in 9router's translators.
- Claude requires every `tool_use` to have a `tool_result` in the immediately next user message. OpenAI requires every `tool_call` to have a matching `role: "tool"` response. Missing tool responses must be synthesized by the translator.
- Tool call IDs: some providers don't generate them. Canonical must ensure every tool call has an ID (generate if missing).

**Thinking Blocks:**
- Claude uses `content_block_start {type: "thinking"}` + `thinking_delta` + `content_block_stop`.
- OpenAI-compatible providers (DeepSeek, GLM) use `delta.reasoning_content` or `delta.reasoning`.
- Canonical must represent thinking as a first-class content type that can be streamed.
- Claude requires a `signature` field on thinking blocks when sending TO Claude API. This is provider-specific and must NOT be in canonical — it belongs in the Claude translator.

**Images:**
- OpenAI uses `{type: "image_url", image_url: {url: "data:image/png;base64,..."}}`.
- Claude uses `{type: "image", source: {type: "base64", media_type: "image/png", data: "..."}}`.
- Canonical must normalize to a single representation. URL-referenced images and base64-inline images are both valid.

**Streaming:**
- Claude streams as typed events: `message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`.
- OpenAI streams as `chat.completion.chunk` with `delta` fields.
- Canonical chunks must capture: text deltas, thinking deltas, tool call argument deltas, finish reason, and usage.

**Usage:**
- Claude reports `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`.
- OpenAI reports `prompt_tokens`, `completion_tokens`, `total_tokens`, `prompt_tokens_details.cached_tokens`.
- Canonical must store the superset and let translators map in/out.

### 1.3 The Complete Canonical Type Specification

```go
package canonical

import "time"

// ── Format Constants ──

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

// ── Role Constants ──

const (
    RoleSystem    = "system"
    RoleUser      = "user"
    RoleAssistant = "assistant"
    RoleTool      = "tool"
)

// ── Content Type Constants ──

const (
    TypeText       = "text"
    TypeImage      = "image"
    TypeToolCall   = "tool_call"
    TypeToolResult = "tool_result"
    TypeThinking   = "thinking"
)

// ── Request ──

// Request is the canonical intermediate representation for all chat/completion requests.
// Design rule: this is the UNION of all provider features. Fields irrelevant
// to a target provider are silently dropped by that provider's translator.
type Request struct {
    Model    string   `json:"model"`
    Messages []Message `json:"messages"`

    // System prompt — separated from messages because Claude treats it specially.
    // OpenAI translators merge this into messages[0] role=system.
    // If both System and a system-role message exist, System takes precedence.
    System []SystemBlock `json:"system,omitempty"`

    // Tool definitions
    Tools      []Tool  `json:"tools,omitempty"`
    ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

    // Generation parameters
    Stream      bool     `json:"stream"`
    MaxTokens   int      `json:"max_tokens,omitempty"`
    Temperature *float64 `json:"temperature,omitempty"`
    TopP        *float64 `json:"top_p,omitempty"`
    Stop        []string `json:"stop,omitempty"`

    // Extended thinking
    Thinking *ThinkingConfig `json:"thinking,omitempty"`

    // Response format constraint (JSON mode)
    ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

    // Internal metadata — never serialized to upstream providers.
    // Used by pipeline stages for routing decisions.
    Meta *RequestMeta `json:"-"`
}

// SystemBlock is a single block within the system prompt.
// Claude supports array-of-blocks with cache_control; OpenAI uses a single string.
type SystemBlock struct {
    Text string `json:"text"`
    // CacheControl is provider-specific (Claude). Translators add/remove as needed.
    // Canonical carries it so that Claude→Claude passthrough preserves it.
    CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl for Claude's prompt caching feature.
type CacheControl struct {
    Type string `json:"type"`           // "ephemeral"
    TTL  string `json:"ttl,omitempty"`  // "1h"
}

// RequestMeta holds internal routing data that never leaves sage-router.
type RequestMeta struct {
    RequestID    string
    SourceFormat Format
    Endpoint     string
    UserAgent    string
    ReceivedAt   time.Time
}

// ── Messages ──

// Message is a single conversational turn.
// INVARIANT: Content is ALWAYS an array, never a bare string.
// Translators normalize string content to []Content{{Type: "text", Text: s}}
// during ToCanonical(). This eliminates typeof checks everywhere downstream.
type Message struct {
    Role    string    `json:"role"`
    Content []Content `json:"content"`
}

// Content is a polymorphic content block within a message.
// Only fields relevant to the Type are populated.
//
// Type "text":       Text
// Type "image":      ImageSource
// Type "thinking":   Text (contains the thinking content)
// Type "tool_call":  ToolCallID, ToolName, Arguments
// Type "tool_result": ToolCallID, Text (result content), IsError
type Content struct {
    Type string `json:"type"`

    // ── Text & Thinking ──
    Text string `json:"text,omitempty"`

    // ── Image ──
    ImageSource *ImageSource `json:"image_source,omitempty"`

    // ── Tool Call (assistant → requests tool execution) ──
    ToolCallID string `json:"tool_call_id,omitempty"`
    ToolName   string `json:"tool_name,omitempty"`
    // Arguments is ALWAYS a JSON string, never a parsed object.
    // This matches OpenAI's wire format and avoids parse/serialize asymmetry.
    // Claude translator: JSON.stringify(input) when reading, JSON.parse when writing.
    Arguments string `json:"arguments,omitempty"`

    // ── Tool Result (tool → reports result back) ──
    // Uses ToolCallID to match the originating tool call.
    // Text holds the result content.
    IsError bool `json:"is_error,omitempty"`
}

// ImageSource represents an image in a message.
type ImageSource struct {
    // Inline base64 image
    MediaType string `json:"media_type,omitempty"` // "image/png", "image/jpeg"
    Data      string `json:"data,omitempty"`        // base64-encoded bytes

    // URL-referenced image (mutually exclusive with Data)
    URL string `json:"url,omitempty"`
}

// ── Tools ──

// Tool describes a callable function the model can invoke.
type Tool struct {
    Type        string `json:"type,omitempty"`        // "function" (default) or provider-specific
    Name        string `json:"name"`
    Description string `json:"description,omitempty"`
    // Parameters is the JSON Schema object describing the function's input.
    // Stored as raw JSON to avoid lossy parsing of arbitrary schemas.
    Parameters json.RawMessage `json:"parameters"`
}

// ToolChoice controls how the model selects tools.
type ToolChoice struct {
    Type string `json:"type"`           // "auto" | "none" | "any" | "required" | "tool"
    Name string `json:"name,omitempty"` // When Type == "tool", the specific tool name
}

// ── Thinking ──

// ThinkingConfig controls Claude's extended thinking feature.
type ThinkingConfig struct {
    Type         string `json:"type,omitempty"`          // "enabled" | "disabled"
    BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// ── Response Format ──

// ResponseFormat constrains the model's output format.
type ResponseFormat struct {
    Type       string          `json:"type"`                  // "json_object" | "json_schema" | "text"
    JSONSchema json.RawMessage `json:"json_schema,omitempty"` // For "json_schema" type
}

// ── Streaming Chunks ──

// Chunk is a single incremental event in a streaming response.
// This is the canonical streaming unit that all response translators
// produce and consume.
type Chunk struct {
    // ID is stable across all chunks in a single response.
    ID    string `json:"id"`
    Model string `json:"model,omitempty"`

    // Exactly one of these fields is populated per chunk:
    Delta        *Delta  `json:"delta,omitempty"`
    FinishReason string  `json:"finish_reason,omitempty"` // "stop" | "length" | "tool_calls"
    Usage        *Usage  `json:"usage,omitempty"`

    // Role is set on the first chunk only.
    Role string `json:"role,omitempty"`
}

// Delta carries the incremental content of a streaming chunk.
// Only one field is populated per delta.
type Delta struct {
    // Regular text content
    Text string `json:"text,omitempty"`

    // Thinking/reasoning content (extended thinking)
    Thinking string `json:"thinking,omitempty"`

    // Tool call (incremental): accumulate across chunks
    ToolCallID string `json:"tool_call_id,omitempty"`
    ToolName   string `json:"tool_name,omitempty"`
    Arguments  string `json:"arguments,omitempty"` // Incremental JSON fragment
}

// ── Usage ──

// Usage tracks token consumption for a completed request.
// Superset of all provider usage fields.
type Usage struct {
    // Standard counts
    PromptTokens     int `json:"prompt_tokens"`
    CompletionTokens int `json:"completion_tokens"`
    TotalTokens      int `json:"total_tokens"`

    // Claude-specific: decomposed prompt tokens
    InputTokens            int `json:"input_tokens,omitempty"`
    OutputTokens           int `json:"output_tokens,omitempty"`
    CacheReadInputTokens   int `json:"cache_read_input_tokens,omitempty"`
    CacheCreationTokens    int `json:"cache_creation_input_tokens,omitempty"`
}
```

### 1.4 Validation Rules (pkg/canonical/validate.go)

```go
// Validate checks a canonical Request for structural correctness.
// Called after ToCanonical() and before FromCanonical().
func Validate(req *Request) error {
    if req.Model == "" {
        return errors.New("model is required")
    }

    for i, msg := range req.Messages {
        switch msg.Role {
        case RoleSystem, RoleUser, RoleAssistant, RoleTool:
            // valid
        default:
            return fmt.Errorf("message[%d]: unknown role %q", i, msg.Role)
        }

        // Content must be array (our invariant)
        if msg.Content == nil {
            return fmt.Errorf("message[%d]: content must not be nil", i)
        }

        for j, c := range msg.Content {
            switch c.Type {
            case TypeText, TypeThinking:
                // Text may be empty (e.g., empty assistant turn before tool_calls)
            case TypeImage:
                if c.ImageSource == nil {
                    return fmt.Errorf("message[%d].content[%d]: image requires image_source", i, j)
                }
            case TypeToolCall:
                if c.ToolCallID == "" {
                    return fmt.Errorf("message[%d].content[%d]: tool_call requires tool_call_id", i, j)
                }
                if c.ToolName == "" {
                    return fmt.Errorf("message[%d].content[%d]: tool_call requires tool_name", i, j)
                }
            case TypeToolResult:
                if c.ToolCallID == "" {
                    return fmt.Errorf("message[%d].content[%d]: tool_result requires tool_call_id", i, j)
                }
            default:
                return fmt.Errorf("message[%d].content[%d]: unknown type %q", i, j, c.Type)
            }
        }
    }

    // Structural: every tool_call must have a matching tool_result
    // (translators fix this, but validation catches drift)
    toolCallIDs := map[string]bool{}
    toolResultIDs := map[string]bool{}
    for _, msg := range req.Messages {
        for _, c := range msg.Content {
            if c.Type == TypeToolCall {
                toolCallIDs[c.ToolCallID] = true
            }
            if c.Type == TypeToolResult {
                toolResultIDs[c.ToolCallID] = true
            }
        }
    }
    for id := range toolCallIDs {
        if !toolResultIDs[id] {
            return fmt.Errorf("tool_call %q has no matching tool_result", id)
        }
    }

    return nil
}
```

---

## Part 2: Claude Translator — The Hardest Piece

### 2.1 Why Claude Is the Hardest

Claude's API differs from OpenAI in six fundamental ways that each require careful handling:

| Aspect | OpenAI | Claude | Translation Complexity |
|--------|--------|--------|----------------------|
| System prompt | `messages[0]` with `role: "system"` | Separate `system` field, array of blocks | Must extract/inject, handle cache_control |
| Content format | String or array, mixed | Always array of typed blocks | Must normalize |
| Tool definitions | `{type: "function", function: {...}}` | `{name, description, input_schema}` | Field renaming + schema key rename |
| Tool call args | `arguments: "json string"` | `input: {parsed object}` | Parse/stringify at boundary |
| Tool results | `role: "tool"` separate message | `tool_result` block inside user message | Must restructure message sequence |
| Thinking | `reasoning_content` in delta | Dedicated `content_block` type with signatures | Block lifecycle management |

### 2.2 Claude-to-Canonical Request Translation

**Input**: Raw Claude API request body (bytes)
**Output**: `*canonical.Request`

```
Claude Request Structure:
{
  "model": "claude-sonnet-4-20250514",
  "system": [{"type": "text", "text": "You are..."}],     ← extract to canonical.System
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "Hi"}]},
    {"role": "assistant", "content": [
      {"type": "thinking", "thinking": "...", "signature": "..."},  ← strip signature
      {"type": "text", "text": "Hello!"},
      {"type": "tool_use", "id": "toolu_01", "name": "read_file",
       "input": {"path": "/foo"}}                          ← stringify input
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_01",
       "content": "file contents..."}                      ← map tool_use_id → tool_call_id
    ]}
  ],
  "tools": [{"name": "read_file", "input_schema": {...}}],  ← rename to parameters
  "max_tokens": 8192,
  "thinking": {"type": "enabled", "budget_tokens": 4096}
}
```

**Translation steps (pseudocode):**

```
func claudeToCanonical(raw []byte) (*Request, error):
    var claude ClaudeRequest
    json.Unmarshal(raw, &claude)

    req := &Request{
        Model:     claude.Model,
        Stream:    claude.Stream,
        MaxTokens: claude.MaxTokens,
    }

    // 1. System — direct mapping
    for _, block := range claude.System:
        req.System = append(req.System, SystemBlock{
            Text:         block.Text,
            CacheControl: block.CacheControl,  // preserve for Claude→Claude passthrough
        })

    // 2. Messages — block-by-block conversion
    for _, msg := range claude.Messages:
        canonical := Message{Role: msg.Role, Content: []Content{}}

        for _, block := range msg.Content:
            switch block.Type:
            case "text":
                canonical.Content = append(canonical.Content, Content{
                    Type: TypeText,
                    Text: block.Text,
                })

            case "thinking", "redacted_thinking":
                canonical.Content = append(canonical.Content, Content{
                    Type: TypeThinking,
                    Text: block.Thinking,
                    // NOTE: signature is stripped. It's provider-specific.
                    // Claude FromCanonical() re-adds it.
                })

            case "image":
                canonical.Content = append(canonical.Content, Content{
                    Type: TypeImage,
                    ImageSource: &ImageSource{
                        MediaType: block.Source.MediaType,
                        Data:      block.Source.Data,
                    },
                })

            case "tool_use":
                // CRITICAL: Claude input is a parsed object.
                // Canonical stores it as a JSON string (OpenAI convention).
                argsBytes, _ := json.Marshal(block.Input)
                canonical.Content = append(canonical.Content, Content{
                    Type:       TypeToolCall,
                    ToolCallID: block.ID,
                    ToolName:   block.Name,
                    Arguments:  string(argsBytes),
                })

            case "tool_result":
                // Claude tool_result.content can be string or array of blocks
                resultText := extractToolResultText(block.Content)
                canonical.Content = append(canonical.Content, Content{
                    Type:       TypeToolResult,
                    ToolCallID: block.ToolUseID,  // map field name
                    Text:       resultText,
                    IsError:    block.IsError,
                })

        req.Messages = append(req.Messages, canonical)

    // 3. Tools — rename input_schema → parameters
    for _, tool := range claude.Tools:
        req.Tools = append(req.Tools, Tool{
            Name:        tool.Name,
            Description: tool.Description,
            Parameters:  tool.InputSchema,  // raw JSON, no parsing
        })

    // 4. Tool choice — map type names
    if claude.ToolChoice != nil:
        req.ToolChoice = mapClaudeToolChoice(claude.ToolChoice)

    // 5. Thinking — direct mapping (strip signature concern)
    if claude.Thinking != nil:
        req.Thinking = &ThinkingConfig{
            Type:         claude.Thinking.Type,
            BudgetTokens: claude.Thinking.BudgetTokens,
        }

    return req, nil
```

### 2.3 Canonical-to-Claude Request Translation

**Input**: `*canonical.Request`
**Output**: Raw Claude API request body (bytes)

This is the reverse, but with additional Claude-specific concerns:

```
Canonical → Claude special handling:

1. System: canonical.System → claude.system (array of blocks)
   - Re-add cache_control to last block

2. Messages: Must enforce Claude's strict sequencing rules:
   a. Messages must alternate user/assistant (merge consecutive same-role)
   b. tool_result MUST be in a user message immediately after the
      assistant message containing the matching tool_use
   c. tool_use and text AFTER tool_use in same assistant message:
      remove the trailing text (Claude doesn't allow it)
   d. If thinking enabled and assistant has tool_use but no thinking block:
      inject a placeholder thinking block with valid signature
   e. Last message should be role=user (for generation requests)

3. Tools: parameters → input_schema (rename)

4. Tool call arguments: JSON string → parsed object
   - json.Unmarshal(content.Arguments) → input field

5. Tool choice: map "required" → "any", "none" → "auto"

6. MaxTokens: ensure max_tokens > thinking.budget_tokens
   (Claude API requirement)
```

### 2.4 Claude Streaming Response Translation

This is the trickiest part. Claude's streaming protocol uses a **block lifecycle**:

```
message_start          → emit Chunk{Role: "assistant"}
content_block_start    → track block type (text/thinking/tool_use)
content_block_delta    → emit Chunk{Delta: ...} based on block type
content_block_stop     → close current block
message_delta          → extract usage + finish_reason
message_stop           → emit final Chunk{FinishReason: ...}
```

**Stream state machine:**

```
                    ┌──────────────────────────────┐
                    │         StreamState           │
                    ├──────────────────────────────┤
                    │ phase: idle|started|streaming │
                    │ activeBlockType: ""           │
                    │ activeBlockIndex: -1          │
                    │ toolCallIndex: 0              │
                    │ toolCalls: map[int]ToolAccum  │
                    │ messageID: ""                 │
                    │ model: ""                     │
                    │ finishReason: ""              │
                    │ usage: nil                    │
                    └──────────────────────────────┘

Events and transitions:

message_start:
  → phase = started
  → store messageID, model
  → emit Chunk{ID: messageID, Role: "assistant"}

content_block_start (type=text):
  → activeBlockType = "text"
  → (no emission — wait for first delta)

content_block_start (type=thinking):
  → activeBlockType = "thinking"
  → (no emission)

content_block_start (type=tool_use):
  → activeBlockType = "tool_use"
  → store tool ID and name in toolCalls map
  → emit Chunk{Delta: {ToolCallID: id, ToolName: name}}

content_block_start (type=server_tool_use):
  → activeBlockType = "server_tool" (skip all deltas for this block)

content_block_delta (text_delta):
  → emit Chunk{Delta: {Text: delta.text}}

content_block_delta (thinking_delta):
  → emit Chunk{Delta: {Thinking: delta.thinking}}

content_block_delta (input_json_delta):
  → emit Chunk{Delta: {Arguments: delta.partial_json}}

content_block_stop:
  → activeBlockType = ""
  → (no emission for text/thinking — their content is already streamed)

message_delta:
  → extract usage (input_tokens, output_tokens, cache_*)
  → extract stop_reason → map to finish_reason

message_stop:
  → emit Chunk{FinishReason: mapped_reason, Usage: extracted_usage}
  → phase = idle
```

### 2.5 Canonical-to-Claude Streaming Response (Reverse Direction)

When the CLIENT speaks Claude format but the UPSTREAM was OpenAI format:

```
Canonical Chunk → Claude SSE events:

Chunk{Role: "assistant"}:
  → emit message_start event

Chunk{Delta: {Thinking: "..."}}:
  → if no thinking block started: emit content_block_start{type: "thinking"}
  → emit content_block_delta{type: "thinking_delta", thinking: text}

Chunk{Delta: {Text: "..."}}:
  → if thinking block was open: emit content_block_stop for it
  → if no text block started: emit content_block_start{type: "text"}
  → emit content_block_delta{type: "text_delta", text: text}

Chunk{Delta: {ToolCallID, ToolName: "...", Arguments: ""}}:
  → close any open text block
  → emit content_block_start{type: "tool_use", id: id, name: name}

Chunk{Delta: {Arguments: "..."}} (continuation):
  → emit content_block_delta{type: "input_json_delta", partial_json: args}

Chunk{FinishReason: "stop"}:
  → close any open blocks
  → emit message_delta{delta: {stop_reason: "end_turn"}, usage: {...}}
  → emit message_stop
```

### 2.6 Golden Test Cases for Claude Translator

Each test case is an input/output JSON pair. These are the **minimum required** set:

| # | Test Case | Key Edge Case Covered |
|---|-----------|----------------------|
| 01 | Simple text chat | Basic message conversion |
| 02 | Multi-turn conversation | Role alternation |
| 03 | System prompt (string) | System extraction |
| 04 | System prompt (array with cache_control) | Preserve cache metadata |
| 05 | User message with image (base64) | Image source mapping |
| 06 | Assistant with tool_use | input→arguments stringify |
| 07 | User with tool_result | tool_use_id→tool_call_id rename |
| 08 | Multi-tool call + results | Multiple tool pairs in sequence |
| 09 | Tool use with missing result | Synthesize empty result |
| 10 | Thinking blocks enabled | Thinking content + strip signature |
| 11 | Thinking + tool_use (no prior thinking) | Inject placeholder thinking block |
| 12 | Text after tool_use in assistant | Strip trailing text |
| 13 | Consecutive same-role messages | Merge into one |
| 14 | Empty assistant content (prefill) | Preserve as empty |
| 15 | JSON mode (response_format) | Inject schema into system prompt |
| 16 | Tool choice: "any" → "required" | Bidirectional mapping |
| 17 | max_tokens < thinking.budget_tokens | Auto-adjust max_tokens |
| 18 | Built-in tools (web_search) | Pass through vs. strip |

**For streaming, additional golden tests:**

| # | Stream Test Case | Key Edge Case |
|---|-----------------|---------------|
| S01 | Simple text response | Basic message lifecycle |
| S02 | Thinking then text | Block transition |
| S03 | Text then tool_use | Mid-stream type switch |
| S04 | Multiple tool calls | Concurrent accumulation |
| S05 | server_tool_use blocks | Skip silently |
| S06 | Usage in message_delta | Token extraction with cache |
| S07 | Empty content (keepalive) | Don't emit |
| S08 | Abrupt stream end (no message_stop) | Graceful cleanup |

---

## Part 3: Dashboard UX — Best-in-Class Developer Tool Experience

### 3.1 UX Philosophy

Developer tools have a unique UX contract: users are **technically sophisticated but time-poor**. They don't need hand-holding but they punish wasted steps. The best developer dashboards (Vercel, Linear, Raycast) share these traits:

1. **Instant feedback** — every action completes in <200ms or shows a progress indicator
2. **Information density without clutter** — show everything relevant, nothing extraneous
3. **Keyboard-first** — every action reachable without touching the mouse
4. **Zero-config start, progressive disclosure** — works out of the box, reveals complexity on demand
5. **Terminal-grade typography** — monospace where it matters, crisp contrast, no decorative fluff

### 3.2 Aesthetic Direction: "Terminal Luxe"

A blend of terminal utility and refined minimalism. Think: Linear's precision meets Vercel's confidence meets a well-configured Neovim.

**Color system:**
```css
:root {
  /* Background layers (dark mode default — developers prefer it) */
  --bg-base:    #0a0a0b;    /* Deep black, not pure #000 */
  --bg-surface: #111113;    /* Cards and panels */
  --bg-raised:  #1a1a1e;    /* Hover states, modals */
  --bg-overlay: #232328;    /* Dropdowns, tooltips */

  /* Content */
  --text-primary:   #ededef;  /* High contrast, not pure white */
  --text-secondary: #8b8b8e;  /* Labels, descriptions */
  --text-tertiary:  #5c5c60;  /* Disabled, timestamps */

  /* Accent — single strong color, used sparingly */
  --accent:         #3b82f6;  /* Blue — trust, technical, calm */
  --accent-hover:   #2563eb;
  --accent-subtle:  #3b82f610; /* 6% opacity for backgrounds */

  /* Status */
  --status-active:  #22c55e;  /* Green dot — provider active */
  --status-warning: #eab308;  /* Yellow — cooldown/degraded */
  --status-error:   #ef4444;  /* Red — failed/disabled */

  /* Borders — barely visible, structural only */
  --border:         #ffffff0a; /* 4% white */
  --border-hover:   #ffffff14; /* 8% white */
}
```

**Typography:**
```css
/* Monospace for data: tokens, costs, keys, model names */
--font-mono: "JetBrains Mono", "SF Mono", "Fira Code", monospace;
/* Sans for UI labels and prose */
--font-sans: "Geist", -apple-system, system-ui, sans-serif;

/* Scale */
--text-xs:  0.75rem;   /* 12px — timestamps, badges */
--text-sm:  0.8125rem; /* 13px — table cells, secondary */
--text-base: 0.875rem; /* 14px — primary content */
--text-lg:  1rem;      /* 16px — section headers */
--text-xl:  1.25rem;   /* 20px — page titles */
```

### 3.3 First-Run Experience (The Critical 60 Seconds)

The moment a developer first runs `sage-router`, their terminal shows:

```
  ┌─────────────────────────────────────────┐
  │                                         │
  │   Sage Router v1.0.0                    │
  │                                         │
  │   Dashboard:  http://localhost:20128    │
  │   API:        http://localhost:20128/v1 │
  │                                         │
  │   Password:   Kx7mP2nQ                 │
  │   (change in Settings → Security)       │
  │                                         │
  └─────────────────────────────────────────┘
```

When they open the dashboard, they see a **single-page onboarding flow** — not a settings screen:

```
┌─────────────────────────────────────────────────────┐
│                                                     │
│  Welcome to Sage Router                             │
│                                                     │
│  Connect your first AI provider to start routing.   │
│                                                     │
│  ┌──────────────────┐  ┌──────────────────┐        │
│  │  ☁ Claude Code   │  │  ☁ Gemini CLI    │        │
│  │  Free via OAuth   │  │  Free via OAuth   │        │
│  │                   │  │                   │        │
│  │  [Connect →]      │  │  [Connect →]      │        │
│  └──────────────────┘  └──────────────────┘        │
│                                                     │
│  ┌──────────────────┐  ┌──────────────────┐        │
│  │  🔑 OpenAI       │  │  🔑 Anthropic    │        │
│  │  API Key          │  │  API Key          │        │
│  │                   │  │                   │        │
│  │  [Add Key →]      │  │  [Add Key →]      │        │
│  └──────────────────┘  └──────────────────┘        │
│                                                     │
│  ┌──────────────────────────────────────────┐      │
│  │  + Browse all 40+ providers               │      │
│  └──────────────────────────────────────────┘      │
│                                                     │
└─────────────────────────────────────────────────────┘
```

After connecting one provider, the onboarding auto-advances to the **endpoint page** with copy-paste instructions for their specific CLI tool. Detected from User-Agent if the dashboard was opened from a terminal.

### 3.4 Page Structure

```
Sidebar (60px collapsed, 220px expanded)
├── ⚡ Overview          ← Status at a glance
├── 🔌 Providers         ← Connect, manage, reorder
├── 🔀 Models            ← Aliases + Combos
├── 📊 Usage             ← Charts, costs, request log
├── ⚙ Settings           ← Keys, security, proxy, import/export
└── 📋 Connect           ← Per-tool setup instructions
```

**Six pages total**. 9router has 12+. Fewer pages, more density per page.

### 3.5 Key Page: Providers

This is where users spend 80% of their dashboard time. The design must handle:
- 1 provider (first-time user) to 20+ providers (power user)
- Multiple accounts per provider with priority ordering
- Real-time status (active, cooldown timer, error, disabled)
- Quick actions: test, reorder, enable/disable, delete

```
┌──────────────────────────────────────────────────────────────┐
│ Providers                                            [+ Add] │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌ Claude Code ──────────────────────────────────────────┐  │
│  │ ● user@gmail.com           Active    Used 3/5 today   │  │
│  │   Priority 1 · OAuth · Last used 2min ago             │  │
│  │                                                        │  │
│  │ ● work@company.com         Cooldown  1m 30s remaining │  │
│  │   Priority 2 · OAuth · Rate limited on sonnet-4       │  │
│  │                                     [Test] [⋮]        │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌ OpenAI ───────────────────────────────────────────────┐  │
│  │ ● Production Key           Active    $2.41 today      │  │
│  │   Priority 1 · API Key · sk-...7xQ                    │  │
│  │                                     [Test] [⋮]        │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌ Gemini CLI ───────────────────────────────────────────┐  │
│  │ ● dev@gmail.com            ⚠ Token expiring soon      │  │
│  │   Priority 1 · OAuth · Expires in 12min               │  │
│  │                            [Refresh] [Test] [⋮]       │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
│  Drag providers to reorder priority. Higher = tried first.   │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

**Design principles for this page:**
- Status indicators use colored dots (green/yellow/red), not text badges — faster to scan
- Cooldown shows a **live countdown timer** (updates every second), not a static timestamp
- Cost display is per-day by default (most useful time window)
- Drag-to-reorder handles both provider-level and account-level priority
- The `[⋮]` menu contains: Edit, Disable, Delete, View Logs
- Each provider card is collapsible — power users with 10+ providers can collapse inactive ones

### 3.6 Key Page: Overview (Status at a Glance)

The overview is a **single-screen health dashboard** with no scrolling required:

```
┌──────────────────────────────────────────────────────────────┐
│ Overview                                                     │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  STATUS              TODAY                 THIS MONTH        │
│  ● 5 active          142 requests          3,847 requests   │
│  ⚠ 1 cooldown        $1.24 cost            $18.72 cost      │
│  ✕ 0 errors          98.6% success          97.2% success   │
│                                                              │
│  ┌─ Requests (24h) ──────────────────────────────────────┐  │
│  │  ▁▂▃▅▇█▇▅▃▂▁▁▁▁▂▃▅▇█▇▅▃▂                            │  │
│  │  12am        6am        12pm        6pm        now     │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
│  PROVIDER HEALTH                                             │
│  Claude Code    ●●○    2 active, 1 cooldown (47s)           │
│  OpenAI         ●      1 active                              │
│  Gemini CLI     ●      1 active                              │
│  iFlow          ●      1 active                              │
│                                                              │
│  RECENT REQUESTS                                             │
│  2s ago   cc/sonnet-4    ✓  1.2s  3.2K tok  $0.012         │
│  15s ago  openai/gpt-4o  ✓  0.8s  1.1K tok  $0.003         │
│  32s ago  cc/sonnet-4    ⚠  →ag   2.1s  fallback            │
│  1m ago   combo/fast     ✓  0.6s  0.8K tok  $0.001         │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### 3.7 Interaction Patterns

**Command Palette (Cmd+K / Ctrl+K)**

Like Linear and Raycast, a command palette for keyboard-first navigation:

```
┌────────────────────────────────────────┐
│ 🔍 Type a command...                   │
├────────────────────────────────────────┤
│ → Add provider                         │
│ → Create combo                         │
│ → Copy API key                         │
│ → Test all connections                 │
│ → Go to Usage                          │
│ → Go to Settings                       │
│ → Export database                       │
└────────────────────────────────────────┘
```

**Toast Notifications**

Non-blocking, bottom-right corner, auto-dismiss after 3s:
- ✓ "Claude Code connected" (green)
- ⚠ "Account rate limited, using fallback" (yellow)
- ✗ "Token refresh failed for gemini-cli" (red)

**Inline Editing**

Model aliases and combo names are edited inline (click to edit) rather than opening a modal. Saves one interaction step.

**Copy-to-Clipboard**

Every code snippet, API key, and model name has a one-click copy button. The button shows "Copied ✓" for 1.5s then reverts.

### 3.8 Technical Implementation

```
web/dashboard/
├── package.json          # preact + @preact/signals + wouter (1.5KB router)
├── vite.config.js        # Build to web/dashboard/dist/
├── index.html
├── src/
│   ├── main.jsx          # Mount app
│   ├── app.jsx           # Router + layout
│   ├── signals/          # Global state (Preact signals, no Redux)
│   │   ├── providers.js  # Provider list + status
│   │   ├── usage.js      # Usage data
│   │   └── settings.js   # Settings cache
│   ├── api/
│   │   └── client.js     # Typed fetch wrapper with error handling
│   ├── pages/
│   │   ├── overview.jsx
│   │   ├── providers.jsx
│   │   ├── models.jsx
│   │   ├── usage.jsx
│   │   ├── settings.jsx
│   │   └── connect.jsx
│   ├── components/
│   │   ├── sidebar.jsx
│   │   ├── command-palette.jsx
│   │   ├── toast.jsx
│   │   ├── status-dot.jsx
│   │   ├── countdown.jsx  # Live cooldown timer
│   │   ├── sparkline.jsx  # Tiny inline chart
│   │   └── copy-button.jsx
│   └── styles/
│       ├── tokens.css     # CSS variables (color, type, spacing)
│       └── reset.css      # Minimal reset
```

**Bundle budget:**
| Package | Size (gzipped) |
|---------|---------------|
| Preact | 3.5KB |
| @preact/signals | 1.2KB |
| wouter (router) | 1.5KB |
| Application code | ~30KB |
| CSS | ~8KB |
| **Total** | **~45KB** |

Compare: 9router's Next.js dashboard is ~2MB. This is a **44× reduction**.

### 3.9 Light Mode Support

Dark mode is default (developers overwhelmingly prefer it), but light mode is available via a toggle in settings. The color system inverts cleanly:

```css
[data-theme="light"] {
  --bg-base:    #ffffff;
  --bg-surface: #f8f9fa;
  --bg-raised:  #f0f1f3;
  --text-primary:   #111113;
  --text-secondary: #6b6b6e;
  --border:         #0000000a;
}
```

### 3.10 Responsive Design

The dashboard works on three breakpoints:
- **Desktop (>1024px)**: Full sidebar + content
- **Tablet (768-1024px)**: Collapsed sidebar (icons only) + full content
- **Mobile (<768px)**: Bottom tab bar + stacked content (for phone checking while traveling)

Mobile is deprioritized in v1 but the layout is responsive from day one so it doesn't need a rewrite later.
