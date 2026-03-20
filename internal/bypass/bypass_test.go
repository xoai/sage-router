package bypass

import (
	"encoding/json"
	"testing"
)

func TestFilter_TitleGeneration(t *testing.T) {
	f := NewFilter()

	// Match: system prompt mentions title + branch/session
	req := &Req{
		Model:        "claude-haiku-4-5-20251001",
		SystemText:   "Generate a succinct session title and branch name.",
		LastUserMsg:  "Fix N+1 query in tasks endpoint",
		MessageCount: 1,
	}
	result := f.Check(req)
	if result == nil {
		t.Fatal("expected title_generation match")
	}
	if result.PatternName != "title_generation" {
		t.Errorf("pattern = %q, want title_generation", result.PatternName)
	}

	// Verify response is valid JSON with title field
	var resp map[string]any
	if err := json.Unmarshal(result.Response, &resp); err != nil {
		t.Fatalf("invalid response JSON: %v", err)
	}
	content := resp["content"].([]any)[0].(map[string]any)["text"].(string)
	var titleResp map[string]any
	if err := json.Unmarshal([]byte(content), &titleResp); err != nil {
		t.Fatalf("content is not valid JSON: %v — got: %s", err, content)
	}
	if titleResp["title"] == nil {
		t.Error("response should have title field")
	}
}

func TestFilter_TitleGenFromMessage(t *testing.T) {
	f := NewFilter()

	req := &Req{
		Model:        "claude-haiku-4-5-20251001",
		SystemText:   "",
		LastUserMsg:  "generate a title for this conversation",
		MessageCount: 2,
	}
	result := f.Check(req)
	if result == nil {
		t.Fatal("expected match on user message pattern")
	}
	if result.PatternName != "title_generation" {
		t.Errorf("pattern = %q", result.PatternName)
	}
}

func TestFilter_TitleGenSkipsLongConversation(t *testing.T) {
	f := NewFilter()

	// Title gen should not match deep in a conversation
	req := &Req{
		Model:        "claude-sonnet-4-6",
		SystemText:   "Generate a session title",
		LastUserMsg:  "Now fix the bug",
		MessageCount: 10,
	}
	if f.Check(req) != nil {
		t.Error("should not match with MessageCount > 3")
	}
}

func TestFilter_Warmup(t *testing.T) {
	f := NewFilter()

	for _, msg := range []string{"warmup", "Warmup", "ping", "test"} {
		req := &Req{
			Model:        "claude-sonnet-4-6",
			LastUserMsg:  msg,
			MessageCount: 1,
		}
		result := f.Check(req)
		if result == nil {
			t.Errorf("expected warmup match for %q", msg)
			continue
		}
		if result.PatternName != "warmup" {
			t.Errorf("pattern = %q for %q", result.PatternName, msg)
		}
	}
}

func TestFilter_NoMatchNormalRequest(t *testing.T) {
	f := NewFilter()

	req := &Req{
		Model:        "claude-sonnet-4-6",
		SystemText:   "You are a helpful coding assistant.",
		LastUserMsg:  "Write a function to calculate fibonacci numbers in Go.",
		HasTools:     true,
		MessageCount: 5,
	}
	if f.Check(req) != nil {
		t.Error("normal request should not match any bypass pattern")
	}
}

func TestFilter_Disabled(t *testing.T) {
	f := NewFilter()
	f.SetEnabled(false)

	req := &Req{
		Model:       "claude-haiku-4-5-20251001",
		LastUserMsg: "warmup",
	}
	if f.Check(req) != nil {
		t.Error("disabled filter should not match")
	}
}

func TestFilter_NilReq(t *testing.T) {
	f := NewFilter()
	if f.Check(nil) != nil {
		t.Error("nil request should return nil")
	}
}
