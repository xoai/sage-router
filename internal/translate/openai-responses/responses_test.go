package openairesp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sage-router/internal/translate"
	"sage-router/pkg/canonical"
	"strings"
	"testing"
)

// ── Helpers ──

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "responses", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// jsonEq compares two JSON blobs semantically (key order ignored).
func jsonEq(t *testing.T, want, got []byte) bool {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal got: %v\nbody: %s", err, got)
	}
	wb, _ := json.MarshalIndent(w, "", "  ")
	gb, _ := json.MarshalIndent(g, "", "  ")
	if string(wb) == string(gb) {
		return true
	}
	t.Errorf("JSON mismatch.\n--- want ---\n%s\n--- got ---\n%s", wb, gb)
	return false
}

// ── Format / DetectInbound ──

func TestTranslator_FormatIsResponses(t *testing.T) {
	if got := New().Format(); got != canonical.FormatResponses {
		t.Errorf("Format() = %q, want %q", got, canonical.FormatResponses)
	}
}

func TestTranslator_DetectInbound_MatchesResponsesPath(t *testing.T) {
	tr := New()
	cases := []struct {
		endpoint string
		want     bool
	}{
		{"/v1/responses", true},
		{"/responses", true},
		{"https://api.openai.com/v1/responses", true},
		{"/v1/chat/completions", false},
		{"/v1/messages", false},
		{"", false},
	}
	for _, c := range cases {
		if got := tr.DetectInbound(c.endpoint, nil); got != c.want {
			t.Errorf("DetectInbound(%q) = %v, want %v", c.endpoint, got, c.want)
		}
	}
}

// ── M3.2 FromCanonical ──

func TestFromCanonical_SingleTurnUser(t *testing.T) {
	tr := New()
	req := &canonical.Request{
		Model:     "gpt-4o",
		System:    []canonical.SystemBlock{{Text: "You are a helpful assistant."}},
		Messages:  []canonical.Message{{Role: canonical.RoleUser, Content: []canonical.Content{canonical.TextContent("What is 2+2?")}}},
		MaxTokens: 1024,
		Stream:    false,
	}
	got, err := tr.FromCanonical(req, translate.TranslateOpts{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("FromCanonical: %v", err)
	}
	want := mustReadFixture(t, "single_turn_user.input.json")
	jsonEq(t, want, got)
}

func TestFromCanonical_MultiTurnAssistantHistory(t *testing.T) {
	tr := New()
	req := &canonical.Request{
		Model:  "gpt-4o",
		System: []canonical.SystemBlock{{Text: "You are a helpful assistant."}},
		Messages: []canonical.Message{
			{Role: canonical.RoleUser, Content: []canonical.Content{canonical.TextContent("What is the capital of France?")}},
			{Role: canonical.RoleAssistant, Content: []canonical.Content{canonical.TextContent("The capital of France is Paris.")}},
			{Role: canonical.RoleUser, Content: []canonical.Content{canonical.TextContent("What is its population?")}},
		},
		MaxTokens: 1024,
		Stream:    false,
	}
	got, err := tr.FromCanonical(req, translate.TranslateOpts{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("FromCanonical: %v", err)
	}
	want := mustReadFixture(t, "multi_turn.input.json")
	jsonEq(t, want, got)
}

func TestFromCanonical_MultipleSystemBlocksJoinedWithDoubleNewline(t *testing.T) {
	tr := New()
	req := &canonical.Request{
		Model:    "gpt-4o",
		System:   []canonical.SystemBlock{{Text: "First instruction."}, {Text: "Second instruction."}},
		Messages: []canonical.Message{{Role: "user", Content: []canonical.Content{canonical.TextContent("hi")}}},
	}
	got, _ := tr.FromCanonical(req, translate.TranslateOpts{})
	var out outboundRequest
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := "First instruction.\n\nSecond instruction."
	if out.Instructions != want {
		t.Errorf("Instructions = %q, want %q", out.Instructions, want)
	}
}

func TestFromCanonical_StoreAlwaysFalse(t *testing.T) {
	tr := New()
	req := &canonical.Request{Model: "gpt-4o", Messages: []canonical.Message{{Role: "user", Content: []canonical.Content{canonical.TextContent("hi")}}}}
	got, _ := tr.FromCanonical(req, translate.TranslateOpts{})
	var raw map[string]any
	_ = json.Unmarshal(got, &raw)
	if v, ok := raw["store"].(bool); !ok || v != false {
		t.Errorf("store = %v, want false (sage-router opts out of conversation persistence)", raw["store"])
	}
}

// TestFromCanonical_StreamForcedTrue — cycle 20260517-provider-auth-variants
// M2.3: the codex backend rejects non-streaming requests with 400 "Stream
// must be set to true" (M0.8 live test). The translator FORCES stream:true
// regardless of req.Stream; the response handler buffers SSE for non-streaming
// clients. Previously this test was named StreamThreadsThrough and pinned
// pass-through behavior.
func TestFromCanonical_StreamForcedTrue(t *testing.T) {
	tr := New()
	// req.Stream=false MUST be overridden to true in the outbound body.
	req := &canonical.Request{Model: "gpt-4o", Stream: false, Messages: []canonical.Message{{Role: "user", Content: []canonical.Content{canonical.TextContent("hi")}}}}
	got, _ := tr.FromCanonical(req, translate.TranslateOpts{})
	var raw map[string]any
	_ = json.Unmarshal(got, &raw)
	if v, _ := raw["stream"].(bool); v != true {
		t.Errorf("stream = %v, want true (FORCED — backend rejects non-streaming)", raw["stream"])
	}
}

// TestFromCanonical_InstructionsDefaulted — cycle 20260517-provider-auth-variants
// M2.3: backend rejects empty/absent instructions with 400 "Instructions are
// required" (M0.8 live test). When canonReq.System has no non-empty blocks,
// the translator emits a sensible default.
func TestFromCanonical_InstructionsDefaulted(t *testing.T) {
	tr := New()
	cases := []struct {
		name   string
		system []canonical.SystemBlock
	}{
		{"nil system", nil},
		{"empty slice", []canonical.SystemBlock{}},
		{"empty block", []canonical.SystemBlock{{Text: ""}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &canonical.Request{
				Model:    "gpt-4o",
				System:   c.system,
				Messages: []canonical.Message{{Role: "user", Content: []canonical.Content{canonical.TextContent("hi")}}},
			}
			got, err := tr.FromCanonical(req, translate.TranslateOpts{})
			if err != nil {
				t.Fatalf("FromCanonical: %v", err)
			}
			var raw map[string]any
			_ = json.Unmarshal(got, &raw)
			if v, _ := raw["instructions"].(string); v != "You are a helpful assistant." {
				t.Errorf("instructions = %q, want %q (default for empty System)", v, "You are a helpful assistant.")
			}
		})
	}
}

// TestFromCanonical_InstructionsPreservedWhenNonEmpty — when canonReq.System
// has at least one non-empty block, the translator uses it (does NOT fall
// back to the default).
func TestFromCanonical_InstructionsPreservedWhenNonEmpty(t *testing.T) {
	tr := New()
	req := &canonical.Request{
		Model:    "gpt-4o",
		System:   []canonical.SystemBlock{{Text: "You are a poet."}},
		Messages: []canonical.Message{{Role: "user", Content: []canonical.Content{canonical.TextContent("hi")}}},
	}
	got, _ := tr.FromCanonical(req, translate.TranslateOpts{})
	var raw map[string]any
	_ = json.Unmarshal(got, &raw)
	if v, _ := raw["instructions"].(string); v != "You are a poet." {
		t.Errorf("instructions = %q, want %q (user-supplied System preserved)", v, "You are a poet.")
	}
}

// TestFromCanonical_ToolsPassThrough — cycle 20260517-provider-auth-variants
// M2.3: the predecessor cycle's AC-T4 rejection (ErrToolsUnsupported) was
// based on a wrong premise. M0.8 live evidence + predecessor's
// tools_streaming.sse capture both confirm the codex backend ACCEPTS tools
// (HTTP 200). Request-level canonical.Request.Tools no longer triggers
// rejection; the body goes through. Tool definitions themselves are
// dropped silently for now (text-only scope of this cycle) — future cycle
// will forward them properly.
func TestFromCanonical_ToolsPassThrough(t *testing.T) {
	tr := New()
	req := &canonical.Request{
		Model:    "gpt-4o",
		Tools:    []canonical.Tool{{Name: "get_weather", Parameters: json.RawMessage(`{}`)}},
		Messages: []canonical.Message{{Role: "user", Content: []canonical.Content{canonical.TextContent("weather?")}}},
	}
	got, err := tr.FromCanonical(req, translate.TranslateOpts{})
	if err != nil {
		t.Fatalf("FromCanonical with tools must NOT error: got %v", err)
	}
	if len(got) == 0 {
		t.Fatal("empty body")
	}
	// Sanity: body is JSON with the required force-stream/store/instructions.
	var raw map[string]any
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if v, _ := raw["stream"].(bool); v != true {
		t.Errorf("tools-pass-through must still force stream:true; got %v", raw["stream"])
	}
}

// ── M3.3 ToCanonical (round-trip — AC-E6) ──

func TestToCanonical_ParsesSingleTurnUser(t *testing.T) {
	body := mustReadFixture(t, "single_turn_user.input.json")
	tr := New()
	req, err := tr.ToCanonical(body, translate.TranslateOpts{})
	if err != nil {
		t.Fatalf("ToCanonical: %v", err)
	}
	if req.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", req.Model)
	}
	// MaxTokens dropped at M2 e2e fold — fixture no longer carries
	// max_output_tokens (codex backend rejects it). ToCanonical reads
	// whatever the body has; with the field absent, MaxTokens=0.
	if req.MaxTokens != 0 {
		t.Errorf("MaxTokens = %d, want 0 (fixture no longer has max_output_tokens after M2 e2e fold)", req.MaxTokens)
	}
	if len(req.System) != 1 || req.System[0].Text != "You are a helpful assistant." {
		t.Errorf("System mismatch: %+v", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("Messages mismatch: %+v", req.Messages)
	}
	if got := req.Messages[0].Content[0].Text; got != "What is 2+2?" {
		t.Errorf("user content = %q, want 'What is 2+2?'", got)
	}
}

func TestToCanonical_ParsesMultiTurnWithAssistant(t *testing.T) {
	body := mustReadFixture(t, "multi_turn.input.json")
	tr := New()
	req, err := tr.ToCanonical(body, translate.TranslateOpts{})
	if err != nil {
		t.Fatalf("ToCanonical: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("Messages len = %d, want 3", len(req.Messages))
	}
	if req.Messages[0].Role != "user" || req.Messages[1].Role != "assistant" || req.Messages[2].Role != "user" {
		t.Errorf("role sequence = %s/%s/%s, want user/assistant/user",
			req.Messages[0].Role, req.Messages[1].Role, req.Messages[2].Role)
	}
	// Assistant message body should round-trip (output_text → text)
	if got := req.Messages[1].Content[0].Text; got != "The capital of France is Paris." {
		t.Errorf("assistant turn lost in round-trip: %q", got)
	}
}

func TestFromCanonical_RoundTripsSingleTurn(t *testing.T) {
	// FromCanonical(ToCanonical(x)) == x, byte-equivalent. Defends
	// AC-E6 round-trip stability when a client posts directly to
	// sage-router's /v1/responses with same-format target.
	tr := New()
	original := mustReadFixture(t, "single_turn_user.input.json")
	req, err := tr.ToCanonical(original, translate.TranslateOpts{})
	if err != nil {
		t.Fatalf("ToCanonical: %v", err)
	}
	got, err := tr.FromCanonical(req, translate.TranslateOpts{})
	if err != nil {
		t.Fatalf("FromCanonical: %v", err)
	}
	jsonEq(t, original, got)
}

// ── M3.3 ParseUpstreamResponse + M3.4 Tier-error ──

func TestParseUpstreamResponse_ExtractsAssistantText(t *testing.T) {
	body := mustReadFixture(t, "single_turn_user.expected.json")
	text, usage, err := ParseUpstreamResponse(body)
	if err != nil {
		t.Fatalf("ParseUpstreamResponse: %v", err)
	}
	if text != "2 + 2 equals 4." {
		t.Errorf("text = %q, want '2 + 2 equals 4.'", text)
	}
	if usage == nil {
		t.Fatal("usage = nil")
	}
	if usage.PromptTokens != 18 || usage.CompletionTokens != 9 || usage.TotalTokens != 27 {
		t.Errorf("usage = %+v, want {input:18, output:9, total:27}", usage)
	}
}

func TestParseUpstreamResponse_MultiTurnConcatenatesAssistant(t *testing.T) {
	body := mustReadFixture(t, "multi_turn.expected.json")
	text, _, err := ParseUpstreamResponse(body)
	if err != nil {
		t.Fatalf("ParseUpstreamResponse: %v", err)
	}
	if !strings.Contains(text, "Paris has a population") {
		t.Errorf("text missing expected substring; got %q", text)
	}
}

func TestParseUpstreamResponse_TierErrorReturnsSentinel(t *testing.T) {
	body := mustReadFixture(t, "tier_error.expected.json")
	_, _, err := ParseUpstreamResponse(body)
	if !errors.Is(err, ErrTierMissingScopes) {
		t.Errorf("got err=%v, want ErrTierMissingScopes (the dashboard banner relies on this discriminator)", err)
	}
}

// AC-P3 / review M1: refusal output items must surface as plain text
// (so the client sees something) AND emit a slog.Warn so operators see
// trend changes. Without this, a refusal renders as a blank assistant
// reply.
func TestParseUpstreamResponse_RefusalSurfacesAsText(t *testing.T) {
	body := []byte(`{
		"id":"resp_refuse_1","object":"response","status":"completed","model":"gpt-4o",
		"output":[{"type":"message","role":"assistant","status":"completed",
			"content":[{"type":"refusal","text":"I cannot help with that request."}]}],
		"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}
	}`)
	text, usage, err := ParseUpstreamResponse(body)
	if err != nil {
		t.Fatalf("refusal must NOT be a hard error; got %v", err)
	}
	if !strings.Contains(text, "I cannot help") {
		t.Errorf("refusal text not surfaced; got %q", text)
	}
	if usage == nil || usage.TotalTokens != 12 {
		t.Errorf("usage missing or wrong; got %+v", usage)
	}
}

// AC-P4 / review M2: status="failed" with no error envelope must error,
// not silently emit empty text. Otherwise clients render blank message.
func TestParseUpstreamResponse_StatusFailedSurfacesError(t *testing.T) {
	body := []byte(`{
		"id":"resp_fail_1","object":"response","status":"failed","model":"gpt-4o",
		"output":[],
		"usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}
	}`)
	_, _, err := ParseUpstreamResponse(body)
	if err == nil {
		t.Fatal("status=failed without error envelope must surface as error (got nil)")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error should mention 'failed'; got %q", err.Error())
	}
}

func TestParseUpstreamResponse_GenericErrorWrappedNotSwallowed(t *testing.T) {
	body := []byte(`{"error":{"message":"server boom","type":"server_error","code":"internal"}}`)
	_, _, err := ParseUpstreamResponse(body)
	if err == nil {
		t.Fatal("expected error for upstream error envelope")
	}
	if errors.Is(err, ErrTierMissingScopes) {
		t.Error("must not classify generic server error as tier error")
	}
	if !strings.Contains(err.Error(), "server boom") {
		t.Errorf("error should propagate upstream message; got %q", err.Error())
	}
}

// ── CanonicalToStreamChunk: not implemented (outbound-only translator) ──

func TestCanonicalToStreamChunk_NotImplemented(t *testing.T) {
	tr := New()
	_, err := tr.CanonicalToStreamChunk(canonical.Chunk{}, translate.NewStreamState())
	if err == nil {
		t.Error("expected not-implemented error for outbound-only translator")
	}
}

// ── M4 StreamChunkToCanonical ──

// extractSSEDataPayloads reads the streaming.raw.sse fixture and returns the
// ordered list of `data: {...}` JSON payloads (one per event). Mirrors what
// the SSE reader at pkg/sse delivers to a translator: one parsed event's
// data line at a time.
func extractSSEDataPayloads(t *testing.T, fixture string) [][]byte {
	t.Helper()
	raw := mustReadFixture(t, fixture)
	var out [][]byte
	for _, line := range strings.Split(string(raw), "\n") {
		const prefix = "data: "
		if strings.HasPrefix(line, prefix) {
			out = append(out, []byte(strings.TrimPrefix(line, prefix)))
		}
	}
	return out
}

func TestStreamChunkToCanonical_FullSSEReplayProducesDeltasAndCompletion(t *testing.T) {
	tr := New()
	state := translate.NewStreamState()
	payloads := extractSSEDataPayloads(t, "streaming.raw.sse")
	if len(payloads) == 0 {
		t.Fatal("no data payloads in streaming.raw.sse")
	}

	var allChunks []canonical.Chunk
	var deltaTexts []string
	var sawUsage *canonical.Usage
	var sawFinishReason string
	for i, p := range payloads {
		chunks, err := tr.StreamChunkToCanonical(p, state)
		if err != nil {
			t.Fatalf("payload[%d]: %v", i, err)
		}
		for _, c := range chunks {
			allChunks = append(allChunks, c)
			if c.Delta != nil && c.Delta.Text != "" {
				deltaTexts = append(deltaTexts, c.Delta.Text)
			}
			if c.Usage != nil {
				sawUsage = c.Usage
			}
			if c.FinishReason != "" {
				sawFinishReason = c.FinishReason
			}
		}
	}

	// The fixture has 3 output_text.delta events (Hello, !, " How can I help you today?").
	wantDeltas := []string{"Hello", "!", " How can I help you today?"}
	if len(deltaTexts) != len(wantDeltas) {
		t.Errorf("delta count = %d, want %d (got %+v)", len(deltaTexts), len(wantDeltas), deltaTexts)
	}
	for i := range wantDeltas {
		if i >= len(deltaTexts) {
			break
		}
		if deltaTexts[i] != wantDeltas[i] {
			t.Errorf("delta[%d] = %q, want %q", i, deltaTexts[i], wantDeltas[i])
		}
	}
	if sawUsage == nil {
		t.Error("response.completed did not emit a usage chunk")
	} else if sawUsage.TotalTokens != 20 {
		t.Errorf("usage.TotalTokens = %d, want 20", sawUsage.TotalTokens)
	}
	if sawFinishReason != "stop" {
		t.Errorf("FinishReason = %q, want 'stop' (mapped from response.completed status)", sawFinishReason)
	}

	// State carries forward through the stream.
	if state.MessageID != "resp_synthetic_stream_001" {
		t.Errorf("state.MessageID = %q, want resp_synthetic_stream_001 (set on response.created)", state.MessageID)
	}
	if state.Model != "gpt-4o" {
		t.Errorf("state.Model = %q, want gpt-4o (set on response.created)", state.Model)
	}
}

func TestStreamChunkToCanonical_CreatedEventEmitsAssistantRole(t *testing.T) {
	tr := New()
	state := translate.NewStreamState()
	payload := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"resp_x","object":"response","status":"in_progress","model":"gpt-4o","output":[]}}`)
	chunks, err := tr.StreamChunkToCanonical(payload, state)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks len = %d, want 1 (role announcement)", len(chunks))
	}
	if chunks[0].Role != canonical.RoleAssistant {
		t.Errorf("Role = %q, want %q", chunks[0].Role, canonical.RoleAssistant)
	}
	if chunks[0].ID != "resp_x" {
		t.Errorf("ID = %q, want resp_x", chunks[0].ID)
	}
}

func TestStreamChunkToCanonical_SkipsNonContentEvents(t *testing.T) {
	tr := New()
	state := translate.NewStreamState()
	state.MessageID = "msg"
	state.Model = "gpt-4o"
	skipTypes := []string{
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
	}
	for _, evType := range skipTypes {
		payload := []byte(`{"type":"` + evType + `","sequence_number":1}`)
		chunks, err := tr.StreamChunkToCanonical(payload, state)
		if err != nil {
			t.Errorf("[%s] unexpected error: %v", evType, err)
		}
		if chunks != nil {
			t.Errorf("[%s] expected nil chunks (skip semantic), got %d chunks", evType, len(chunks))
		}
	}
}

func TestStreamChunkToCanonical_UnknownEventTypeIsSkipped(t *testing.T) {
	// Forward-compat: SSE event-vocabulary growth (R3 in spec) — new event
	// types added by OpenAI should be silently dropped, not error.
	tr := New()
	state := translate.NewStreamState()
	chunks, err := tr.StreamChunkToCanonical([]byte(`{"type":"response.reasoning.delta","delta":"thinking..."}`), state)
	if err != nil {
		t.Errorf("unknown type should not error; got %v", err)
	}
	if chunks != nil {
		t.Errorf("unknown type should return nil chunks; got %+v", chunks)
	}
}

func TestStreamChunkToCanonical_MalformedJSONErrors(t *testing.T) {
	tr := New()
	_, err := tr.StreamChunkToCanonical([]byte(`not-json`), translate.NewStreamState())
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
}

// ── M4.3 Error event handling ──

func TestStreamChunkToCanonical_ResponseFailedEmitsErrorFinish(t *testing.T) {
	tr := New()
	state := translate.NewStreamState()
	state.MessageID = "resp_x"
	state.Model = "gpt-4o"
	payload := []byte(`{"type":"response.failed","sequence_number":5,"response":{"id":"resp_x","status":"failed","error":{"message":"server overload","type":"server_error","code":"internal_error"}}}`)
	chunks, err := tr.StreamChunkToCanonical(payload, state)
	if err != nil {
		t.Fatalf("response.failed should not return Go-level error; surface via chunk: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("response.failed should emit at least one chunk")
	}
	last := chunks[len(chunks)-1]
	if last.FinishReason == "" {
		t.Errorf("response.failed should set FinishReason (got %+v)", last)
	}
	if last.FinishReason != "error" {
		t.Errorf("FinishReason on failure = %q, want 'error'", last.FinishReason)
	}
}

func TestStreamChunkToCanonical_TopLevelErrorEvent(t *testing.T) {
	// Per OpenAI streaming SSE: a top-level `event: error` carries
	// {type:"error", message, code} (not wrapped in a response object).
	tr := New()
	state := translate.NewStreamState()
	payload := []byte(`{"type":"error","sequence_number":1,"message":"upstream rate limit","code":"rate_limit_exceeded"}`)
	chunks, err := tr.StreamChunkToCanonical(payload, state)
	if err != nil {
		t.Fatalf("top-level error should surface as chunk: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("error event should emit a finish chunk")
	}
	if chunks[len(chunks)-1].FinishReason != "error" {
		t.Errorf("FinishReason = %q, want 'error'", chunks[len(chunks)-1].FinishReason)
	}
}
