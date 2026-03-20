package routing

import (
	"strings"
	"testing"
)

func makeHistory(turns ...Turn) *History {
	return &History{
		Turns:     turns,
		LastModel: "anthropic/claude-sonnet-4-6",
	}
}

func makeTurn(role, content string) Turn {
	return Turn{
		Role:    role,
		Content: compress([]byte(content)),
		Model:   "claude-sonnet-4-6",
	}
}

func TestBuildContextBridge_Basic(t *testing.T) {
	h := makeHistory(
		makeTurn("user", "Write a REST API in Go"),
		makeTurn("assistant", "I'll use Gin for the HTTP framework. Let's go with PostgreSQL for the database."),
		makeTurn("user", "Add authentication"),
		makeTurn("assistant", "I chose JWT over session cookies. Here's the middleware in /middleware/auth.go"),
	)

	bridge := BuildContextBridge(h, 4000)
	if bridge == "" {
		t.Fatal("expected non-empty bridge")
	}

	// Should contain the header
	if !strings.Contains(bridge, "Context from previous assistant") {
		t.Error("missing header")
	}

	// Should contain decisions
	if !strings.Contains(bridge, "Gin") {
		t.Error("missing Gin decision")
	}
	if !strings.Contains(bridge, "JWT") || !strings.Contains(bridge, "session cookies") {
		t.Error("missing JWT decision")
	}

	// Should contain file reference
	if !strings.Contains(bridge, "/middleware/auth.go") {
		t.Error("missing file reference")
	}
}

func TestBuildContextBridge_SlidingWindow(t *testing.T) {
	h := makeHistory(
		makeTurn("user", "Turn 1"),
		makeTurn("assistant", "Response 1"),
		makeTurn("user", "Turn 2"),
		makeTurn("assistant", "Response 2"),
		makeTurn("user", "Turn 3 - the latest"),
		makeTurn("assistant", "Response 3 - the latest"),
	)

	bridge := BuildContextBridge(h, 4000)

	// Should contain recent turns
	if !strings.Contains(bridge, "Turn 3 - the latest") {
		t.Error("missing latest user turn in sliding window")
	}
	if !strings.Contains(bridge, "Response 3 - the latest") {
		t.Error("missing latest assistant turn in sliding window")
	}
}

func TestBuildContextBridge_NilHistory(t *testing.T) {
	bridge := BuildContextBridge(nil, 4000)
	if bridge != "" {
		t.Error("nil history should return empty")
	}
}

func TestBuildContextBridge_TooShort(t *testing.T) {
	h := makeHistory(
		makeTurn("user", "Hello"),
	)
	bridge := BuildContextBridge(h, 4000)
	if bridge != "" {
		t.Error("single turn should return empty")
	}
}

func TestBuildContextBridge_Truncation(t *testing.T) {
	// Very small token budget
	h := makeHistory(
		makeTurn("user", strings.Repeat("Long question. ", 500)),
		makeTurn("assistant", strings.Repeat("Long answer. ", 500)),
	)

	bridge := BuildContextBridge(h, 100) // only 100 tokens ≈ 400 chars
	if len(bridge) > 500 {
		t.Errorf("bridge should be truncated, got %d chars", len(bridge))
	}
}

func TestExtractFileReferences(t *testing.T) {
	h := makeHistory(
		makeTurn("assistant", "I modified /internal/server/routes.go and /pkg/canonical/types.go"),
		makeTurn("assistant", "Also updated /cmd/main.go"),
	)

	files := extractFileReferences(h)
	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d: %v", len(files), files)
	}
}

func TestExtractDecisions(t *testing.T) {
	h := makeHistory(
		makeTurn("assistant", "I'll use SQLite for storage. Let's go with bcrypt for password hashing."),
		makeTurn("assistant", "I chose Go over Rust for this project. Decided to use the stdlib HTTP server."),
	)

	decisions := extractDecisions(h)
	if len(decisions) < 2 {
		t.Errorf("expected at least 2 decisions, got %d: %v", len(decisions), decisions)
	}
}

func TestExtractDecisions_IgnoresUserMessages(t *testing.T) {
	h := makeHistory(
		makeTurn("user", "I'll use whatever you suggest"),
	)

	decisions := extractDecisions(h)
	if len(decisions) != 0 {
		t.Errorf("should not extract from user messages, got %v", decisions)
	}
}

func TestBridgeState(t *testing.T) {
	state := BridgeState{
		Active:        true,
		TurnsLeft:     3,
		PreviousModel: "anthropic/claude-sonnet-4-6",
	}

	if !state.Active {
		t.Error("should be active")
	}
	state.TurnsLeft--
	if state.TurnsLeft != 2 {
		t.Errorf("turns left = %d", state.TurnsLeft)
	}
}
