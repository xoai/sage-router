package compress

import (
	"encoding/json"
	"strings"
	"testing"

	"sage-router/pkg/canonical"
)

// M4 T4 — the Compress stage: tool-result-only, gated, deterministic,
// fail-closed (AC4, AC5, AC6, AC12).

func mustCompressor(t *testing.T) *Compressor {
	t.Helper()
	c, err := NewCompressor()
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	return c
}

// AC12 — a nil Compressor is inert: zero Result, no mutation, no panic.
func TestCompress_NilCompressorInert(t *testing.T) {
	var c *Compressor
	req := &canonical.Request{Messages: []canonical.Message{
		{Role: canonical.RoleTool, Content: []canonical.Content{
			{Type: canonical.TypeToolResult, ToolCallID: "t1", Text: "huge tool output"},
		}},
	}}
	res := c.Compress(req, 1000)
	if res.Compressed || res.TokensBefore != 0 {
		t.Errorf("nil Compressor: got %+v, want a zero Result", res)
	}
	if req.Messages[0].Content[0].Text != "huge tool output" {
		t.Error("nil Compressor mutated the request")
	}
}

// AC4 + AC5 — Compress mutates ONLY tool-result text; System, plain text,
// and tool-call content are byte-identical afterwards.
func TestCompress_OnlyToolResultMutated(t *testing.T) {
	c := mustCompressor(t)
	noisy := "\x1b[31mERROR\x1b[0m\n" + strings.Repeat("dup\n", 12)
	req := &canonical.Request{
		System: []canonical.SystemBlock{{Text: "system prompt " + strings.Repeat("x", 200)}},
		Messages: []canonical.Message{
			{Role: canonical.RoleUser, Content: []canonical.Content{
				{Type: canonical.TypeText, Text: "please run tests\n" + noisy},
			}},
			{Role: canonical.RoleAssistant, Content: []canonical.Content{
				{Type: canonical.TypeToolCall, ToolCallID: "t1", ToolName: "bash", Arguments: noisy},
			}},
			{Role: canonical.RoleTool, Content: []canonical.Content{
				{Type: canonical.TypeToolResult, ToolCallID: "t1", Text: noisy},
			}},
		},
	}
	sysBefore := req.System[0].Text
	userBefore := req.Messages[0].Content[0].Text
	argsBefore := req.Messages[1].Content[0].Arguments
	toolBefore := req.Messages[2].Content[0].Text

	c.Compress(req, 0) // contextWindow 0 → "unknown" → compression allowed

	if req.System[0].Text != sysBefore {
		t.Error("Compress mutated a System block — disjointness invariant broken (AC5)")
	}
	if req.Messages[0].Content[0].Text != userBefore {
		t.Error("Compress mutated plain (TypeText) user content")
	}
	if req.Messages[1].Content[0].Arguments != argsBefore {
		t.Error("Compress mutated TypeToolCall arguments")
	}
	if req.Messages[2].Content[0].Text == toolBefore {
		t.Error("Compress did not change the tool-result content (ANSI + dup lines must compress)")
	}
}

// AC6 — the 70%-of-context pre-flight gate.
func TestCompress_PreflightGate(t *testing.T) {
	c := mustCompressor(t)
	mk := func() *canonical.Request {
		return &canonical.Request{Messages: []canonical.Message{
			{Role: canonical.RoleTool, Content: []canonical.Content{
				{Type: canonical.TypeToolResult, ToolCallID: "t1",
					Text: "\x1b[31mx\x1b[0m\n" + strings.Repeat("dup\n", 60)},
			}},
		}}
	}
	if res := c.Compress(mk(), 1_000_000); res.Compressed {
		t.Error("gate: a request far under 70% of a huge window must not compress")
	}
	if res := c.Compress(mk(), 10); !res.Compressed {
		t.Error("gate: a request over 70% of a tiny window must compress")
	}
	if res := c.Compress(mk(), 0); !res.Compressed {
		t.Error("gate: contextWindow 0 (catalog miss / unknown) must still allow compression")
	}
}

func TestCompress_Deterministic(t *testing.T) {
	c := mustCompressor(t)
	mk := func() *canonical.Request {
		return &canonical.Request{Messages: []canonical.Message{
			{Role: canonical.RoleTool, Content: []canonical.Content{
				{Type: canonical.TypeToolResult, ToolCallID: "t1", Text: strings.Repeat("line\n", 100)},
			}},
		}}
	}
	a, b := mk(), mk()
	c.Compress(a, 0)
	c.Compress(b, 0)
	if a.Messages[0].Content[0].Text != b.Messages[0].Content[0].Text {
		t.Error("Compress is not deterministic")
	}
}

// AC3 — a tool-result that is a JSON document is left byte-identical:
// the line-oriented filters would corrupt it, so Compress skips JSON blocks.
func TestCompress_JSONToolResultUntouched(t *testing.T) {
	c := mustCompressor(t)
	// A pretty-printed JSON array with 3+ identical adjacent lines —
	// collapse-repeats would otherwise fold them and inject a bare marker
	// line, corrupting the structure (the Gate-3 finding).
	jsonResult := "{\n  \"items\": [\n    1,\n    1,\n    1,\n    1\n  ],\n  \"status\": \"ok\"\n}"
	if !json.Valid([]byte(jsonResult)) {
		t.Fatal("test premise: the fixture must be valid JSON")
	}
	req := &canonical.Request{Messages: []canonical.Message{
		{Role: canonical.RoleTool, Content: []canonical.Content{
			{Type: canonical.TypeToolResult, ToolCallID: "t1", Text: jsonResult},
		}},
	}}
	c.Compress(req, 0)
	got := req.Messages[0].Content[0].Text
	if got != jsonResult {
		t.Errorf("Compress corrupted a JSON tool-result:\n in:  %q\n out: %q", jsonResult, got)
	}
	if !json.Valid([]byte(got)) {
		t.Error("Compress produced invalid JSON from a JSON tool-result")
	}
}

func TestCompress_TokensBeforeReported(t *testing.T) {
	c := mustCompressor(t)
	req := &canonical.Request{Messages: []canonical.Message{
		{Role: canonical.RoleUser, Content: []canonical.Content{
			{Type: canonical.TypeText, Text: "hello world, this is a test of the token estimate"},
		}},
	}}
	if res := c.Compress(req, 0); res.TokensBefore <= 0 {
		t.Errorf("TokensBefore = %d, want > 0", res.TokensBefore)
	}
}
