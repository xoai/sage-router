package server

import (
	"encoding/json"
	"testing"

	"sage-router/internal/routing"
	"sage-router/pkg/canonical"
)

func TestPostRequestHook_UpdatesAffinity(t *testing.T) {
	router := routing.NewSmartRouter()
	store := routing.NewConversationStore()

	s := &Server{
		deps: Dependencies{
			SmartRouter:       router,
			ConversationStore: store,
		},
	}

	reqCtx := &requestContext{
		firstMsg:    "hello world",
		requestBody: []byte(`{"messages":[{"role":"user","content":"hello world"}]}`),
	}

	// First request
	s.postRequestHook(reqCtx, "anthropic", "claude-sonnet-4-6", &canonical.Usage{
		CompletionTokens: 100,
	})

	entry := router.Affinity.Get("hello world")
	if entry == nil {
		t.Fatal("expected affinity entry after postRequestHook")
	}
	if entry.Provider != "anthropic" {
		t.Errorf("expected provider anthropic, got %s", entry.Provider)
	}
	if entry.Model != "claude-sonnet-4-6" {
		t.Errorf("expected model claude-sonnet-4-6, got %s", entry.Model)
	}
	if entry.TurnCount != 1 {
		t.Errorf("expected turn count 1, got %d", entry.TurnCount)
	}

	// Second request increments turn count
	s.postRequestHook(reqCtx, "anthropic", "claude-sonnet-4-6", nil)
	entry = router.Affinity.Get("hello world")
	if entry.TurnCount != 2 {
		t.Errorf("expected turn count 2, got %d", entry.TurnCount)
	}
}

func TestPostRequestHook_StoresConversation(t *testing.T) {
	store := routing.NewConversationStore()

	s := &Server{
		deps: Dependencies{
			SmartRouter:       routing.NewSmartRouter(),
			ConversationStore: store,
		},
	}

	reqCtx := &requestContext{
		firstMsg:    "test conversation",
		requestBody: []byte(`{"messages":[{"role":"user","content":"what is Go?"}]}`),
	}

	s.postRequestHook(reqCtx, "openai", "gpt-4o", &canonical.Usage{
		CompletionTokens: 50,
	})

	history := store.GetHistory("test conversation")
	if history == nil {
		t.Fatal("expected conversation history after postRequestHook")
	}
	if len(history.Turns) != 2 {
		t.Errorf("expected 2 turns (user + assistant), got %d", len(history.Turns))
	}
	if history.Turns[0].Role != "user" {
		t.Errorf("expected first turn role user, got %s", history.Turns[0].Role)
	}
	if history.Turns[1].Role != "assistant" {
		t.Errorf("expected second turn role assistant, got %s", history.Turns[1].Role)
	}
	if history.LastModel != "openai/gpt-4o" {
		t.Errorf("expected last model openai/gpt-4o, got %s", history.LastModel)
	}
}

func TestPostRequestHook_BridgeLifecycle(t *testing.T) {
	router := routing.NewSmartRouter()

	s := &Server{
		deps: Dependencies{
			SmartRouter:       router,
			ConversationStore: routing.NewConversationStore(),
		},
	}

	reqCtx := &requestContext{
		firstMsg:    "bridge test",
		requestBody: []byte(`{"messages":[{"role":"user","content":"bridge test"}]}`),
	}

	// Simulate existing affinity with active bridge
	router.Affinity.Set("bridge test", "anthropic", "claude-sonnet-4-6")
	entry := router.Affinity.Get("bridge test")
	entry.BridgeActive = true
	entry.BridgeTurnsLeft = 3

	// Turn 1: decrement
	s.postRequestHook(reqCtx, "anthropic", "claude-sonnet-4-6", nil)
	entry = router.Affinity.Get("bridge test")
	if entry.BridgeTurnsLeft != 2 {
		t.Errorf("expected 2 turns left, got %d", entry.BridgeTurnsLeft)
	}
	if !entry.BridgeActive {
		t.Error("bridge should still be active")
	}

	// Turn 2
	s.postRequestHook(reqCtx, "anthropic", "claude-sonnet-4-6", nil)
	entry = router.Affinity.Get("bridge test")
	if entry.BridgeTurnsLeft != 1 {
		t.Errorf("expected 1 turn left, got %d", entry.BridgeTurnsLeft)
	}

	// Turn 3: bridge expires
	s.postRequestHook(reqCtx, "anthropic", "claude-sonnet-4-6", nil)
	entry = router.Affinity.Get("bridge test")
	if entry.BridgeTurnsLeft != 0 {
		t.Errorf("expected 0 turns left, got %d", entry.BridgeTurnsLeft)
	}
	if entry.BridgeActive {
		t.Error("bridge should be inactive after 3 turns")
	}
}

func TestPostRequestHook_NilInputs(t *testing.T) {
	s := &Server{
		deps: Dependencies{
			SmartRouter:       routing.NewSmartRouter(),
			ConversationStore: routing.NewConversationStore(),
		},
	}

	// Should not panic
	s.postRequestHook(nil, "anthropic", "claude-sonnet-4-6", nil)
	s.postRequestHook(&requestContext{firstMsg: ""}, "anthropic", "claude-sonnet-4-6", nil)
	s.postRequestHook(&requestContext{firstMsg: "test"}, "anthropic", "claude-sonnet-4-6", nil)
}

func TestExtractLastUserMsg(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "string content",
			body: `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"how are you?"}]}`,
			want: "how are you?",
		},
		{
			name: "array content",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hello world"}]}]}`,
			want: "hello world",
		},
		{
			name: "no user messages",
			body: `{"messages":[{"role":"assistant","content":"hi"}]}`,
			want: "",
		},
		{
			name: "empty",
			body: `{}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLastUserMsg([]byte(tt.body))
			if got != tt.want {
				t.Errorf("extractLastUserMsg() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectRequestConstraints(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantImages   bool
		wantTools    bool
		wantThinking bool
	}{
		{
			name:       "basic text",
			body:       `{"model":"test","messages":[{"role":"user","content":"hello"}]}`,
			wantImages: false, wantTools: false, wantThinking: false,
		},
		{
			name:       "with tools",
			body:       `{"model":"test","tools":[{"type":"function"}],"messages":[{"role":"user","content":"hello"}]}`,
			wantImages: false, wantTools: true, wantThinking: false,
		},
		{
			name:       "with thinking",
			body:       `{"model":"test","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hello"}]}`,
			wantImages: false, wantTools: false, wantThinking: true,
		},
		{
			name:       "with images",
			body:       `{"model":"test","messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`,
			wantImages: true, wantTools: false, wantThinking: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := detectRequestConstraints([]byte(tt.body))
			if c.NeedsImages != tt.wantImages {
				t.Errorf("NeedsImages = %v, want %v", c.NeedsImages, tt.wantImages)
			}
			if c.NeedsTools != tt.wantTools {
				t.Errorf("NeedsTools = %v, want %v", c.NeedsTools, tt.wantTools)
			}
			if c.NeedsThinking != tt.wantThinking {
				t.Errorf("NeedsThinking = %v, want %v", c.NeedsThinking, tt.wantThinking)
			}
		})
	}
}

// Ensure JSON helper functions don't break the pipeline.
func TestExtractModelAndStream(t *testing.T) {
	body := []byte(`{"model":"anthropic/claude-sonnet-4-6","stream":true}`)
	model, stream := extractModelAndStream(body)
	if model != "anthropic/claude-sonnet-4-6" {
		t.Errorf("expected anthropic/claude-sonnet-4-6, got %s", model)
	}
	if !stream {
		t.Error("expected stream=true")
	}
}

func TestGuessProvider(t *testing.T) {
	tests := map[string]string{
		"claude-sonnet-4-6":     "anthropic",
		"claude-haiku-4-5":     "anthropic",
		"gpt-4o":               "openai",
		"o3-mini":              "openai",
		"gemini-2.5-pro":       "gemini",
		"llama-3.1-70b":        "ollama",
		"unknown-model":        "openai",
	}

	for model, want := range tests {
		got := guessProvider(model)
		if got != want {
			t.Errorf("guessProvider(%s) = %s, want %s", model, got, want)
		}
	}
}

// Suppress unused import warning
var _ = json.Marshal
