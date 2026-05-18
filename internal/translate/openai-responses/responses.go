// Package openairesp implements the OpenAI Responses API translator.
//
// Responses API (POST /v1/responses) is OpenAI's newer endpoint that uses
// a different request/response body shape than Chat Completions. It is
// the only inference endpoint scoped for ChatGPT-subscription OAuth tokens
// (the PKCE access_token's scopes — api.connectors.* — don't include
// model.request, so /chat/completions returns 401). For OpenAI subscription
// connections, the executor routes to /v1/responses and this translator
// builds the request body.
//
// Cycle 20260517-openai-subscription-responses-api.
package openairesp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sage-router/internal/translate"
	"sage-router/pkg/canonical"
	"strings"
)

// Translator implements translate.Translator for the OpenAI Responses API.
type Translator struct{}

// New constructs the translator.
func New() *Translator { return &Translator{} }

// Format returns canonical.FormatResponses ("openai-responses").
func (t *Translator) Format() canonical.Format { return canonical.FormatResponses }

// DetectInbound recognises Responses API requests by their endpoint path.
// The body shape (input array of items rather than messages array) is a
// secondary signal; path is authoritative.
func (t *Translator) DetectInbound(endpoint string, body []byte) bool {
	return strings.Contains(endpoint, "/v1/responses") || strings.HasSuffix(endpoint, "/responses")
}

// ── Sentinel errors ──
//
// ErrToolsUnsupported was removed in cycle 20260517-provider-auth-variants
// M2.3 — the predecessor cycle's AC-T4 premise (tools rejected by upstream)
// was wrong. M0.8 live test against chatgpt.com/backend-api/codex/responses
// confirmed tools ARE accepted (HTTP 200; matches the predecessor's
// `tools_streaming.sse` capture). The sentinel + the synthetic-422 walk
// arm in routes_v1.go:260-267 were dead code.
//
// Forwarding tools-as-emitted via this translator is a future cycle scope;
// for now tool blocks inside messages are dropped silently at the message-
// content dispatch (text-only scope per spec §11). Request-level
// canonical.Request.Tools no longer triggers rejection.

// ── Wire types ──

// outboundRequest is the JSON shape sent to /v1/responses.
type outboundRequest struct {
	Model           string          `json:"model"`
	Input           []inputItem     `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Stream          bool            `json:"stream"`
	Store           bool            `json:"store"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
}

// inputItem is one element of the `input` array. For text-only multi-turn
// it's always {type:"message", role:"user|assistant", content:[...]}.
type inputItem struct {
	Type    string             `json:"type"`
	Role    string             `json:"role,omitempty"`
	Content []inputContentPart `json:"content,omitempty"`
}

// inputContentPart is one block within an input message's content array.
// User content is type=input_text; assistant history is type=output_text
// (matches the shape Responses emits in its response — round-trips).
type inputContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// upstreamResponse is the JSON shape /v1/responses returns for non-streaming.
type upstreamResponse struct {
	ID         string             `json:"id"`
	Object     string             `json:"object"`
	CreatedAt  int64              `json:"created_at"`
	Status     string             `json:"status"`
	Model      string             `json:"model"`
	Output     []outputItem       `json:"output"`
	Usage      *upstreamUsage     `json:"usage,omitempty"`
	Error      *upstreamError     `json:"error,omitempty"`
}

type outputItem struct {
	Type    string              `json:"type"`
	ID      string              `json:"id,omitempty"`
	Status  string              `json:"status,omitempty"`
	Role    string              `json:"role,omitempty"`
	Content []outputContentPart `json:"content,omitempty"`
}

type outputContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type upstreamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type upstreamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   any    `json:"param"`
}

// ── Translator methods ──

// FromCanonical builds a /v1/responses request body from a canonical Request.
//
// Mapping (spec §"Request mapping (FromCanonical)"):
//   - Request.System (multiple SystemBlocks) → `instructions` (joined with \n\n)
//   - Request.Messages with role=user → input items {type:message, role:user,
//     content:[{type:input_text, text}]}
//   - Request.Messages with role=assistant → input items {type:message,
//     role:assistant, content:[{type:output_text, text}]}
//   - Request.MaxTokens / .Temperature / .TopP → corresponding outbound
//     fields (max_output_tokens, not max_tokens)
//
// Three upstream-mandatory invariants (M0-validated 2026-05-17 — see
// .sage/work/20260517-provider-auth-variants/m0-baseline.md Finding 4):
//
//   - `stream` MUST be true — backend rejects non-streaming with
//     400 "Stream must be set to true". This translator FORCES stream:true
//     regardless of req.Stream; the response handler buffers SSE for
//     non-streaming clients (see routes_v1.go::responsesResponseToOpenAI).
//
//   - `instructions` MUST be non-empty — backend rejects absence with
//     400 "Instructions are required". This translator emits a sensible
//     default ("You are a helpful assistant.") when canonReq.System has
//     no non-empty blocks.
//
//   - `store` MUST be false — backend rejects absent or true with
//     400 "Store must be set to false". sage-router does not opt into
//     the Responses API's conversation-persistence semantics anyway;
//     stateless mode keeps semantics closer to Chat Completions.
func (t *Translator) FromCanonical(req *canonical.Request, opts translate.TranslateOpts) ([]byte, error) {
	out := outboundRequest{
		Model:  req.Model,
		Stream: true,  // FORCED — backend rejects non-streaming.
		Store:  false, // FORCED — backend rejects store=true.
		// Temperature + TopP omitted at M2 e2e fold — codex backend
		// rejected `max_output_tokens` with 400 "Unsupported parameter"
		// against gpt-5.4. M0.8 baseline (which worked HTTP 200) sent
		// only model + instructions + stream + store + input. To stay
		// inside the verified-working envelope we omit ALL optional
		// generation params for now. Future cycle can re-introduce them
		// after live capture proves which fields the backend accepts.
	}
	_ = req.MaxTokens   // intentionally dropped — codex backend rejects max_output_tokens
	_ = req.Temperature // intentionally dropped (paranoia; not yet tested)
	_ = req.TopP        // intentionally dropped (paranoia; not yet tested)

	// System blocks → instructions (concatenate with \n\n). Default to
	// "You are a helpful assistant." if no non-empty system blocks — the
	// backend rejects absent/empty instructions with 400.
	if len(req.System) > 0 {
		parts := make([]string, 0, len(req.System))
		for _, sb := range req.System {
			if sb.Text != "" {
				parts = append(parts, sb.Text)
			}
		}
		out.Instructions = strings.Join(parts, "\n\n")
	}
	if out.Instructions == "" {
		out.Instructions = "You are a helpful assistant."
	}

	// Messages → input items. Each message becomes one item; multi-block
	// content collapses into multiple content parts within that item.
	out.Input = make([]inputItem, 0, len(req.Messages))
	for _, msg := range req.Messages {
		item := inputItem{Type: "message", Role: msg.Role}
		partType := "input_text"
		if msg.Role == canonical.RoleAssistant {
			// Assistant history uses output_text (matches the shape
			// Responses emits in its `output` array — round-trip safe).
			partType = "output_text"
		}
		for _, c := range msg.Content {
			if c.Type == canonical.TypeText && c.Text != "" {
				item.Content = append(item.Content, inputContentPart{
					Type: partType,
					Text: c.Text,
				})
			}
			// Image / tool blocks: dropped silently for now (text-only
			// scope of this cycle per spec). A future cycle handling
			// multimodal+tools would extend this dispatch.
		}
		// Only emit messages that have at least one content part — empty
		// messages would be rejected by upstream.
		if len(item.Content) > 0 {
			out.Input = append(out.Input, item)
		}
	}

	return json.Marshal(out)
}

// ToCanonical parses a /v1/responses REQUEST body into a canonical Request.
// Used when a client posts directly to sage-router's /v1/responses endpoint
// (AC-E6 round-trip). For the primary cycle use case (chat-completions
// inbound → /v1/responses outbound), this method is unused — the openai
// translator's ToCanonical handles the inbound parse.
func (t *Translator) ToCanonical(body []byte, opts translate.TranslateOpts) (*canonical.Request, error) {
	var in outboundRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("openai-responses: parse request body: %w", err)
	}

	req := &canonical.Request{
		Model:       in.Model,
		Stream:      in.Stream,
		MaxTokens:   in.MaxOutputTokens,
		Temperature: in.Temperature,
		TopP:        in.TopP,
	}
	if in.Instructions != "" {
		req.System = []canonical.SystemBlock{{Text: in.Instructions}}
	}
	req.Messages = make([]canonical.Message, 0, len(in.Input))
	for _, item := range in.Input {
		if item.Type != "message" {
			// Non-message input items (tool_call results, function calls,
			// images) are not modeled in this cycle's scope; skip silently.
			continue
		}
		cm := canonical.Message{Role: item.Role}
		for _, p := range item.Content {
			// Both input_text and output_text carry plain text in this
			// scope. Multimodal (input_image, input_file) deferred.
			if p.Text != "" {
				cm.Content = append(cm.Content, canonical.TextContent(p.Text))
			}
		}
		if len(cm.Content) > 0 {
			req.Messages = append(req.Messages, cm)
		}
	}
	return req, nil
}

// ── Streaming ──

// streamEvent is the minimal JSON shape /v1/responses SSE events share.
// Each event carries a `type` field; specific events carry additional
// fields parsed via secondary unmarshalls below.
type streamEvent struct {
	Type           string          `json:"type"`
	SequenceNumber int             `json:"sequence_number"`
	Delta          string          `json:"delta,omitempty"`           // response.output_text.delta
	ItemID         string          `json:"item_id,omitempty"`         // response.output_text.delta / .done
	Response       *upstreamResponse `json:"response,omitempty"`      // response.created / .in_progress / .completed / .failed
	Message        string          `json:"message,omitempty"`         // top-level "error" event
	Code           string          `json:"code,omitempty"`            // top-level "error" event
}

// StreamChunkToCanonical parses one SSE event payload from /v1/responses
// streaming output into zero-or-more canonical Chunks. The data parameter
// is the JSON payload after the SSE `data: ` prefix is stripped (the
// pkg/sse reader strips it for us).
//
// Event dispatch:
//   - response.created             → role-announce chunk; primes state
//   - response.output_text.delta   → text delta chunk
//   - response.completed           → usage + FinishReason chunk
//   - response.failed              → FinishReason="error" chunk
//   - error (top-level)            → FinishReason="error" chunk
//   - all other event types        → nil (skipped — forward-compat per
//     spec R3: SSE event-vocabulary growth)
//
// Returning a Go-level error is reserved for malformed JSON. Upstream
// error events become canonical chunks with FinishReason="error" so the
// pipeline can forward them as a normal stream-completion to the client.
func (t *Translator) StreamChunkToCanonical(data []byte, state *translate.StreamState) ([]canonical.Chunk, error) {
	var ev streamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("openai-responses: parse stream chunk: %w", err)
	}

	switch ev.Type {
	case "response.created":
		// Prime state from the response object skeleton and emit a
		// role-announce chunk so the source-translator (chat-completions)
		// can emit the leading `{"role":"assistant"}` delta.
		if ev.Response != nil {
			if state.MessageID == "" {
				state.MessageID = ev.Response.ID
			}
			if state.Model == "" {
				state.Model = ev.Response.Model
			}
		}
		return []canonical.Chunk{{
			ID:    state.MessageID,
			Model: state.Model,
			Role:  canonical.RoleAssistant,
		}}, nil

	case "response.output_text.delta":
		if ev.Delta == "" {
			return nil, nil
		}
		// Defense against out-of-order SSE (R3 follow-up per review M7):
		// if delta arrives before response.created (load-balancer
		// reorder, edge proxy buffering), state.MessageID is empty.
		// Prime with a deterministic placeholder + log so the chunk
		// still flows downstream — most clients tolerate the missing
		// id, but emitting empty propagates poorly.
		if state.MessageID == "" {
			state.MessageID = "chatcmpl-resp-pending"
			slog.Warn("openai-responses: delta arrived before response.created (out-of-order SSE); priming MessageID placeholder")
		}
		return []canonical.Chunk{{
			ID:    state.MessageID,
			Model: state.Model,
			Delta: &canonical.Delta{Text: ev.Delta},
		}}, nil

	case "response.completed":
		// Emit a single terminal chunk carrying usage + FinishReason.
		out := canonical.Chunk{
			ID:           state.MessageID,
			Model:        state.Model,
			FinishReason: mapFinishReason(ev.Response),
		}
		if ev.Response != nil && ev.Response.Usage != nil {
			usage := &canonical.Usage{
				InputTokens:      ev.Response.Usage.InputTokens,
				OutputTokens:     ev.Response.Usage.OutputTokens,
				TotalTokens:      ev.Response.Usage.TotalTokens,
				PromptTokens:     ev.Response.Usage.InputTokens,
				CompletionTokens: ev.Response.Usage.OutputTokens,
			}
			state.Usage = usage
			out.Usage = usage
		}
		state.FinishReason = out.FinishReason
		return []canonical.Chunk{out}, nil

	case "response.failed":
		// Upstream said it failed mid-stream. Emit an error-finish chunk
		// so the client sees a clean terminator. The upstream error
		// payload (message/code) is logged but not propagated into the
		// canonical Chunk shape (no error field today; would extend
		// canonical.Chunk if cross-translator error fidelity becomes
		// load-bearing).
		state.FinishReason = "error"
		return []canonical.Chunk{{
			ID:           state.MessageID,
			Model:        state.Model,
			FinishReason: "error",
		}}, nil

	case "error":
		// Top-level error event (not wrapped in response.*).
		state.FinishReason = "error"
		return []canonical.Chunk{{
			ID:           state.MessageID,
			Model:        state.Model,
			FinishReason: "error",
		}}, nil

	default:
		// Forward-compat (R3): unknown event types are dropped silently.
		// Covered: response.in_progress, response.output_item.added,
		// response.content_part.added, response.output_text.done,
		// response.content_part.done, response.output_item.done, plus
		// any newly-added event types from future OpenAI versions.
		return nil, nil
	}
}

// mapFinishReason translates a Responses API response.status into the
// canonical FinishReason vocabulary expected by chat-completions clients.
func mapFinishReason(resp *upstreamResponse) string {
	if resp == nil {
		return "stop"
	}
	switch resp.Status {
	case "completed":
		return "stop"
	case "incomplete":
		// Could discriminate by resp.IncompleteReason if/when we model it.
		// For now, treat as length cutoff — matches Responses' most common
		// incomplete case (max_output_tokens exhausted).
		return "length"
	case "failed":
		return "error"
	case "cancelled":
		return "stop"
	default:
		return "stop"
	}
}

// CanonicalToStreamChunk is unused — this translator is outbound. The
// client-facing SSE comes from the source translator (typically openai
// chat-completions). Per spec §"Streaming pipeline".
func (t *Translator) CanonicalToStreamChunk(chunk canonical.Chunk, state *translate.StreamState) ([]byte, error) {
	return nil, errors.New("openai-responses: CanonicalToStreamChunk not implemented (translator is outbound only)")
}

// ── Response-body helpers (used by M5 wiring for non-streaming) ──

// ParseUpstreamResponse parses a successful /v1/responses non-streaming
// response body. Returns the assistant text (concatenation of all
// output_text blocks across all assistant message items) plus the usage
// summary, both ready for re-serialization into chat-completions format
// by M5 wiring.
//
// If the body carries an error (status code != 2xx OR body has an "error"
// envelope), returns (0-text, nil-usage, error). The error is wrapped so
// callers can use errors.Is(err, ErrTierMissingScopes) to discriminate
// the friendly tier-error path.
func ParseUpstreamResponse(body []byte) (text string, usage *canonical.Usage, err error) {
	// Sniff for error envelope first — non-2xx responses still have a
	// JSON body and the error.code carries the actionable signal.
	if errMsg, isErr := sniffTierError(body); isErr {
		return "", nil, errMsg
	}

	// Cycle 20260517-provider-auth-variants M2 e2e fold: the codex
	// backend ALWAYS streams (translator forces stream:true). When a
	// client requests non-streaming, sage-router collects the SSE body
	// here and synthesizes a chat-completions response. Detect SSE by
	// the standard `event:` or `data:` line prefix and route through
	// the streaming-buffer helper.
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		return parseUpstreamSSE(body)
	}

	var resp upstreamResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", nil, fmt.Errorf("openai-responses: parse upstream body: %w", err)
	}
	if resp.Error != nil {
		return "", nil, fmt.Errorf("openai-responses: upstream error: %s (%s)", resp.Error.Message, resp.Error.Type)
	}
	// AC-P4 (review M2 follow-up): status="failed" without an error envelope
	// must surface as an error — silently emitting empty text + usage would
	// render a blank assistant message client-side.
	if resp.Status == "failed" {
		return "", nil, fmt.Errorf("openai-responses: upstream status=failed without error envelope")
	}

	var sb strings.Builder
	for _, item := range resp.Output {
		if item.Type != "message" || item.Role != canonical.RoleAssistant {
			continue
		}
		for _, p := range item.Content {
			switch p.Type {
			case "output_text":
				sb.WriteString(p.Text)
			case "refusal":
				// AC-P3 (review M1 follow-up): the model declined to answer.
				// Surface the refusal text as plain content so the client
				// sees SOMETHING (rather than a silent empty reply) AND
				// log a warning so operators notice trend changes.
				if p.Text != "" {
					sb.WriteString(p.Text)
				}
				slog.Warn("openai-responses: upstream refusal output", "text_len", len(p.Text))
			}
		}
	}

	if resp.Usage != nil {
		usage = &canonical.Usage{
			InputTokens:      resp.Usage.InputTokens,
			OutputTokens:     resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
		}
	}
	return sb.String(), usage, nil
}

// parseUpstreamSSE scans an SSE response body (codex backend always
// streams; translator forces stream:true) and synthesizes the same
// (text, usage) tuple ParseUpstreamResponse returns for a JSON body.
//
// It walks `data: {...}` lines, dispatches on the `type` field, and:
//   - accumulates `response.output_text.delta` deltas
//   - extracts final usage from `response.completed`
//   - surfaces top-level `error` / `response.failed` as errors
//
// Non-event lines (`event: foo`, blank separators, comments) are skipped.
// Cycle 20260517-provider-auth-variants M2 e2e fold.
func parseUpstreamSSE(body []byte) (text string, usage *canonical.Usage, err error) {
	var sb strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // codex events can be large

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev streamEvent
		if jerr := json.Unmarshal([]byte(payload), &ev); jerr != nil {
			// Malformed event — skip; the next valid event usually carries
			// the same info (delta accumulators are idempotent across drops).
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			sb.WriteString(ev.Delta)
		case "response.completed":
			if ev.Response != nil && ev.Response.Usage != nil {
				usage = &canonical.Usage{
					InputTokens:      ev.Response.Usage.InputTokens,
					OutputTokens:     ev.Response.Usage.OutputTokens,
					TotalTokens:      ev.Response.Usage.TotalTokens,
					PromptTokens:     ev.Response.Usage.InputTokens,
					CompletionTokens: ev.Response.Usage.OutputTokens,
				}
			}
		case "response.failed":
			msg := "openai-responses: SSE response.failed"
			if ev.Response != nil && ev.Response.Error != nil {
				msg = fmt.Sprintf("openai-responses: upstream error: %s (%s)",
					ev.Response.Error.Message, ev.Response.Error.Type)
			}
			return "", nil, fmt.Errorf("%s", msg)
		case "error":
			return "", nil, fmt.Errorf("openai-responses: top-level SSE error event")
		}
	}
	if serr := scanner.Err(); serr != nil {
		return "", nil, fmt.Errorf("openai-responses: SSE scan: %w", serr)
	}
	return sb.String(), usage, nil
}

// ErrTierMissingScopes is returned by ParseUpstreamResponse when the
// upstream body carries the "Missing scopes: api.responses.write"
// signature. Callers translate this into a friendly, actionable error
// for the dashboard's "needs re-auth" banner (AC-U2).
var ErrTierMissingScopes = errors.New("openai-responses: ChatGPT subscription does not include API access — upgrade to Plus or Pro to use this connection")

// sniffTierError checks the body for the well-known "Missing scopes:
// api.responses.write" substring that OpenAI returns on tier-limited
// accounts. Substring-match avoids fragility against the surrounding
// JSON keys + parenthetical wording variations across OpenAI versions.
func sniffTierError(body []byte) (error, bool) {
	if bytes.Contains(body, []byte("Missing scopes: api.responses.write")) {
		return ErrTierMissingScopes, true
	}
	return nil, false
}
