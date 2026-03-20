// Package bypass implements a pre-pipeline filter that intercepts known
// low-value request patterns and returns canned responses without routing
// to an upstream provider. This saves cost and latency for requests that
// don't benefit from expensive models (e.g., title generation, warmup pings).
//
// ADR §26: Skip/Bypass Pattern Configuration.
package bypass

import (
	"encoding/json"
	"strings"
	"time"
)

// Pattern defines a request pattern to intercept.
type Pattern struct {
	Name        string            // human-readable name for logging
	Match       func(req *Req) bool // returns true if this pattern matches
	Response    func(req *Req) []byte // generates the canned response
}

// Req is a lightweight view of the incoming request for pattern matching.
// Avoids importing canonical to keep the bypass package dependency-free.
type Req struct {
	Model       string
	SystemText  string   // concatenated system prompt text
	LastUserMsg string   // last user message text
	HasTools    bool
	MessageCount int
}

// Result is returned when a pattern matches.
type Result struct {
	PatternName string
	Response    []byte
}

// Filter holds a list of bypass patterns and checks requests against them.
type Filter struct {
	patterns []Pattern
	enabled  bool
}

// NewFilter creates a filter with the default built-in patterns.
func NewFilter() *Filter {
	return &Filter{
		patterns: defaultPatterns(),
		enabled:  true,
	}
}

// Check tests a request against all patterns. Returns a Result if matched, nil otherwise.
func (f *Filter) Check(req *Req) *Result {
	if !f.enabled || req == nil {
		return nil
	}
	for _, p := range f.patterns {
		if p.Match(req) {
			return &Result{
				PatternName: p.Name,
				Response:    p.Response(req),
			}
		}
	}
	return nil
}

// SetEnabled turns the bypass filter on or off.
func (f *Filter) SetEnabled(enabled bool) {
	f.enabled = enabled
}

// defaultPatterns returns the built-in bypass patterns.
func defaultPatterns() []Pattern {
	return []Pattern{
		titleGenerationPattern(),
		warmupPattern(),
	}
}

// ── Built-in Patterns ──

// titleGenerationPattern matches Claude Code's session title/branch generation requests.
// These use Haiku and ask for a short JSON with title + branch fields.
// We return a generic title to save a round-trip.
func titleGenerationPattern() Pattern {
	return Pattern{
		Name: "title_generation",
		Match: func(req *Req) bool {
			sys := strings.ToLower(req.SystemText)
			msg := strings.ToLower(req.LastUserMsg)

			// Match system prompt patterns from Claude Code's title generator
			hasTitlePrompt := strings.Contains(sys, "title") &&
				(strings.Contains(sys, "branch") || strings.Contains(sys, "session") ||
					strings.Contains(sys, "conversation") || strings.Contains(sys, "generate"))

			// Also match if the user message asks for a title
			hasTitleRequest := strings.Contains(msg, "generate a title") ||
				strings.Contains(msg, "generate a short title") ||
				strings.Contains(msg, "title for this conversation")

			// Must be a short conversation (1-3 messages) — title gen happens early
			return (hasTitlePrompt || hasTitleRequest) && req.MessageCount <= 3
		},
		Response: func(req *Req) []byte {
			// Return a plausible title in Claude format
			title := "Coding session"
			branch := "claude/coding-session"

			// Try to extract a better title from the user message
			if len(req.LastUserMsg) > 0 {
				words := strings.Fields(req.LastUserMsg)
				if len(words) >= 3 && len(words) <= 20 {
					// Use first few words as title
					n := len(words)
					if n > 5 {
						n = 5
					}
					title = strings.Join(words[:n], " ")
					branch = "claude/" + strings.ToLower(strings.Join(words[:n], "-"))
				}
			}

			body := map[string]any{
				"title":  title,
				"branch": branch,
			}
			content, _ := json.Marshal(body)

			return buildClaudeResponse(string(content), req.Model)
		},
	}
}

// warmupPattern matches warmup/ping requests that some tools send on startup.
func warmupPattern() Pattern {
	return Pattern{
		Name: "warmup",
		Match: func(req *Req) bool {
			msg := strings.ToLower(strings.TrimSpace(req.LastUserMsg))
			return msg == "warmup" || msg == "ping" || msg == "test"
		},
		Response: func(req *Req) []byte {
			return buildClaudeResponse("pong", req.Model)
		},
	}
}

// buildClaudeResponse creates a minimal Claude-format response.
// This works for both Claude and OpenAI clients since sage-router
// translates the response format in the stream stage.
func buildClaudeResponse(content, model string) []byte {
	resp := map[string]any{
		"id":    "msg_bypass_" + time.Now().Format("20060102150405"),
		"type":  "message",
		"role":  "assistant",
		"model": model,
		"content": []map[string]any{
			{"type": "text", "text": content},
		},
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	}
	b, _ := json.Marshal(resp)
	return b
}
