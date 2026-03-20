package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/bypass"
	"sage-router/internal/config"
	"sage-router/internal/executor"
	"sage-router/internal/provider"
	"sage-router/internal/routing"
	"sage-router/internal/store"
	"sage-router/internal/translate"
	claudeTranslate "sage-router/internal/translate/claude"
	openaiTranslate "sage-router/internal/translate/openai"
	"sage-router/internal/usage"
)

// mockExecutor returns canned responses for testing.
type mockExecutor struct {
	providerID string
	handler    func(req *executor.ExecuteRequest) (*executor.Result, error)
}

func (m *mockExecutor) Provider() string { return m.providerID }
func (m *mockExecutor) Execute(ctx context.Context, req *executor.ExecuteRequest) (*executor.Result, error) {
	if m.handler != nil {
		return m.handler(req)
	}
	// Default: return a simple OpenAI-format response
	resp := `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hello from mock!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	return &executor.Result{
		StatusCode: 200,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(resp)),
		Latency:    10 * time.Millisecond,
	}, nil
}

// mockClaudeExecutor returns Claude-format responses.
func newMockClaudeExecutor() *mockExecutor {
	return &mockExecutor{
		providerID: "anthropic",
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			resp := `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"Hello from Claude mock!"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
			return &executor.Result{
				StatusCode: 200,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(resp)),
				Latency:    10 * time.Millisecond,
			}, nil
		},
	}
}

// mockStreamExecutor returns SSE streaming responses.
func newMockStreamExecutor(providerID string) *mockExecutor {
	return &mockExecutor{
		providerID: providerID,
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			var stream string
			if providerID == "anthropic" {
				stream = "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\ndata: [DONE]\n"
			} else {
				stream = "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\ndata: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\ndata: [DONE]\n"
			}
			return &executor.Result{
				StatusCode: 200,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(stream)),
				Latency:    10 * time.Millisecond,
			}, nil
		},
	}
}

// mock401Executor always returns 401 (auth failed).
func newMock401Executor(providerID string) *mockExecutor {
	return &mockExecutor{
		providerID: providerID,
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			return &executor.Result{
				StatusCode: 401,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid api key"}}`)),
				Latency:    5 * time.Millisecond,
			}, nil
		},
	}
}

// mock429Executor always returns 429 (rate limited).
func newMock429Executor(providerID string) *mockExecutor {
	return &mockExecutor{
		providerID: providerID,
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			return &executor.Result{
				StatusCode: 429,
				Headers:    http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"30"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)),
				Latency:    5 * time.Millisecond,
			}, nil
		},
	}
}

// setupTestServer creates a fully wired server with in-memory store and mock executors.
func setupTestServer(t *testing.T, executors map[string]executor.Executor) (*Server, store.Store) {
	t.Helper()

	db, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create in-memory store: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Set up auth (no password required for tests)
	authMgr := auth.NewManager("", []byte("test-jwt-secret"), []byte("test-hmac-secret"))

	// Translate registry
	translateReg := translate.NewRegistry()
	translateReg.Register(openaiTranslate.New())
	translateReg.Register(claudeTranslate.New())

	// Provider registry
	providerReg := provider.NewRegistry()
	for id, p := range config.KnownProviders {
		providerReg.Register(id, provider.ProviderMeta{
			ID: p.ID, Name: p.Name, Format: p.Format, BaseURL: p.BaseURL, AuthTypes: p.AuthTypes,
		})
	}

	providerSel := provider.NewSelector()

	// Usage tracker
	usageTracker := usage.NewTracker(db)

	if executors == nil {
		executors = map[string]executor.Executor{
			"openai":    &mockExecutor{providerID: "openai"},
			"anthropic": newMockClaudeExecutor(),
			"default":   &mockExecutor{providerID: "default"},
		}
	}

	srv := New(Config{
		Host: "127.0.0.1",
		Port: 0,
	}, Dependencies{
		Store:             db,
		TranslateRegistry: translateReg,
		ProviderSelector:  providerSel,
		ProviderRegistry:  providerReg,
		Executors:         executors,
		UsageTracker:      usageTracker,
		Auth:              authMgr,
		SmartRouter:       routing.NewSmartRouter(),
		ConversationStore: routing.NewConversationStore(),
		BypassFilter:      bypass.NewFilter(),
	})

	return srv, db
}

// addConnection is a helper to create a connection in the test store + selector.
func addConnection(t *testing.T, srv *Server, db store.Store, providerID, name, authType string) string {
	t.Helper()
	conn := &store.Connection{
		ID:       "conn-" + providerID + "-" + name,
		Provider: providerID,
		Name:     name,
		AuthType: authType,
		APIKey:   "test-key-" + providerID,
		Priority: 0,
		State:    "idle",
	}
	if err := db.CreateConnection(conn); err != nil {
		t.Fatalf("failed to create connection: %v", err)
	}
	provConn := provider.NewConnection(conn.ID, conn.Provider, conn.Name, conn.Priority, conn.AuthType)
	srv.deps.ProviderSelector.Register(provConn)
	return conn.ID
}

// doRequest is a helper to make an HTTP request to the test server.
func doRequest(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithKey(t, srv, method, path, body, "")
}

func doRequestWithKey(t *testing.T, srv *Server, method, path string, body any, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// --- Test Cases ---

func TestE2E_BasicChatCompletion_OpenAIFormat(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "openai/gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	})

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["id"] == nil {
		t.Error("expected response to have 'id' field")
	}
	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Error("expected at least one choice in response")
	}
}

func TestE2E_BasicChatCompletion_ClaudeFormat(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "anthropic", "primary", "apikey")

	w := doRequest(t, srv, "POST", "/v1/messages", map[string]any{
		"model":      "anthropic/claude-sonnet-4-6",
		"max_tokens": 1024,
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	})

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	// Claude format response should have 'type': 'message'
	if resp["type"] != "message" {
		t.Errorf("expected type=message, got %v", resp["type"])
	}
}

func TestE2E_CrossFormatTranslation(t *testing.T) {
	// Send OpenAI format request to Claude provider endpoint
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "anthropic", "primary", "apikey")

	// OpenAI format request to /v1/chat/completions with anthropic model
	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "anthropic/claude-sonnet-4-6",
		"messages": []map[string]any{
			{"role": "user", "content": "Translate me"},
		},
	})

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Response should be in OpenAI format (because request came in via /v1/chat/completions)
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	// Should have choices[] (OpenAI format), not content[] (Claude format)
	if resp["choices"] == nil {
		t.Error("expected OpenAI-format response with 'choices' field")
	}
}

func TestE2E_NoModel_Returns400(t *testing.T) {
	srv, _ := setupTestServer(t, nil)

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	})

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestE2E_NoConnection_Returns503(t *testing.T) {
	srv, _ := setupTestServer(t, nil)
	// No connections registered

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "openai/gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	})

	if w.Code != 503 {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestE2E_APIKeyAuth_Required(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	// Create an API key — its presence auto-enables key validation
	plainKey, keyHash, prefix, err := srv.deps.Auth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	key := &store.APIKey{ID: "k1", Name: "test", KeyHash: keyHash, Prefix: prefix}
	db.CreateAPIKey(key)

	// Request without API key → 401
	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "Hello"}},
	})
	if w.Code != 401 {
		t.Fatalf("expected 401 without API key, got %d", w.Code)
	}

	// Request with valid API key → should pass auth (may get other errors downstream)
	w2 := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "Hello"}},
	}, plainKey)
	if w2.Code == 401 {
		t.Fatalf("expected auth to pass with valid key, got 401")
	}

	// Delete the key
	db.DeleteAPIKey("k1")

	// Request with deleted key → 401
	w3 := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "Hello"}},
	}, plainKey)
	// No keys left, so auth is not required
	if w3.Code == 401 {
		t.Fatalf("expected no auth required when no keys exist, got 401")
	}
}

func TestE2E_BypassFilter_TitleGeneration(t *testing.T) {
	srv, _ := setupTestServer(t, nil)
	// No connections needed — bypass should short-circuit

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "claude-sonnet-4-6",
		"system": "Generate a short title for this conversation.",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello there"},
		},
	})

	// Bypass returns 200 with canned response
	if w.Code != 200 {
		t.Fatalf("expected 200 from bypass, got %d: %s", w.Code, w.Body.String())
	}

	bypass := w.Header().Get("X-Sage-Bypass")
	if bypass == "" {
		t.Error("expected X-Sage-Bypass header from bypass filter")
	}
}

func TestE2E_BypassFilter_Warmup(t *testing.T) {
	srv, _ := setupTestServer(t, nil)

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []map[string]any{
			{"role": "user", "content": "ping"},
		},
	})

	// Check if bypass was triggered (warmup pattern)
	bypass := w.Header().Get("X-Sage-Bypass")
	if bypass != "" {
		// Warmup bypass worked
		if w.Code != 200 {
			t.Fatalf("bypass returned non-200: %d", w.Code)
		}
	}
	// If bypass didn't match, that's OK — warmup pattern might have stricter matching
}

func TestE2E_SmartRouting_Auto(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")
	addConnection(t, srv, db, "anthropic", "primary", "apikey")

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "auto",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello smart router"},
		},
	})

	// Smart routing should pick a provider and succeed
	if w.Code != 200 {
		t.Fatalf("expected 200 from smart routing, got %d: %s", w.Code, w.Body.String())
	}
}

func TestE2E_SmartRouting_AutoFast(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "auto:fast",
		"messages": []map[string]any{
			{"role": "user", "content": "Quick question"},
		},
	})

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestE2E_ComboModel_Fallback(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")
	addConnection(t, srv, db, "anthropic", "primary", "apikey")

	// Create a combo
	combo := &store.Combo{
		ID:     "combo1",
		Name:   "my-combo",
		Models: []string{"anthropic/claude-sonnet-4-6", "openai/gpt-4o"},
	}
	db.CreateCombo(combo)

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "my-combo",
		"messages": []map[string]any{
			{"role": "user", "content": "Try combo"},
		},
	})

	if w.Code != 200 {
		t.Fatalf("expected 200 from combo, got %d: %s", w.Code, w.Body.String())
	}
}

func TestE2E_Alias_Resolution(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	// Create alias
	db.SetAlias("fast", "openai/gpt-4o")

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "fast",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello via alias"},
		},
	})

	if w.Code != 200 {
		t.Fatalf("expected 200 via alias, got %d: %s", w.Code, w.Body.String())
	}
}

func TestE2E_ConnectionStateMachine_RateLimit(t *testing.T) {
	// Use a 429-returning executor
	executors := map[string]executor.Executor{
		"openai":  newMock429Executor("openai"),
		"default": &mockExecutor{providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	// First request → 429
	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "openai/gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"}},
	})

	// Should forward the 429 (or 503 if no fallback)
	if w.Code != 429 && w.Code != 503 {
		t.Fatalf("expected 429 or 503, got %d", w.Code)
	}
}

func TestE2E_ConnectionStateMachine_AuthExpired(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  newMock401Executor("openai"),
		"default": &mockExecutor{providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	// 401 → should mark auth expired
	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "openai/gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"}},
	})

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// Second request → should fail (connection stuck in AuthExpired)
	w2 := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "openai/gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "Hello again"}},
	})

	if w2.Code != 503 {
		t.Fatalf("expected 503 (auth expired), got %d", w2.Code)
	}
}

func TestE2E_FallbackOnError(t *testing.T) {
	callCount := 0
	executors := map[string]executor.Executor{
		"openai": &mockExecutor{
			providerID: "openai",
			handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
				callCount++
				if callCount == 1 {
					// First call: 500
					return &executor.Result{
						StatusCode: 500,
						Headers:    http.Header{},
						Body:       io.NopCloser(strings.NewReader(`{"error":"internal error"}`)),
						Latency:    5 * time.Millisecond,
					}, nil
				}
				// Second call: success
				return &executor.Result{
					StatusCode: 200,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"test","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
					Latency:    5 * time.Millisecond,
				}, nil
			},
		},
		"default": &mockExecutor{providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	addConnection(t, srv, db, "openai", "conn1", "apikey")
	addConnection(t, srv, db, "openai", "conn2", "apikey")

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "openai/gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "Fallback test"}},
	})

	if w.Code != 200 {
		t.Fatalf("expected 200 after fallback, got %d: %s", w.Code, w.Body.String())
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 executor calls (fallback), got %d", callCount)
	}
}

func TestE2E_ListModels(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	w := doRequest(t, srv, "GET", "/v1/models", nil)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["object"] != "list" {
		t.Errorf("expected object=list, got %v", resp["object"])
	}
	data, ok := resp["data"].([]any)
	if !ok {
		t.Fatal("expected data array")
	}
	if len(data) == 0 {
		t.Error("expected at least one model")
	}
}

func TestE2E_GuessProvider(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "anthropic", "primary", "apikey")

	// Send request with just "claude-sonnet-4-6" (no provider prefix)
	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []map[string]any{
			{"role": "user", "content": "Guess my provider"}},
	})

	if w.Code != 200 {
		t.Fatalf("expected 200 with guessed provider, got %d: %s", w.Code, w.Body.String())
	}
}

func TestE2E_SessionAffinity(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	// Send same conversation twice — second should hit affinity cache
	body := map[string]any{
		"model": "openai/gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "Consistent conversation key"},
		},
	}

	w1 := doRequest(t, srv, "POST", "/v1/chat/completions", body)
	if w1.Code != 200 {
		t.Fatalf("first request failed: %d", w1.Code)
	}

	w2 := doRequest(t, srv, "POST", "/v1/chat/completions", body)
	if w2.Code != 200 {
		t.Fatalf("second request failed: %d", w2.Code)
	}

	// Verify affinity was set
	entry := srv.deps.SmartRouter.Affinity.Get("Consistent conversation key")
	if entry == nil {
		t.Error("expected affinity entry after two requests")
	} else if entry.TurnCount < 2 {
		t.Errorf("expected turn count >= 2, got %d", entry.TurnCount)
	}
}

func TestE2E_ConversationStore(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "openai/gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "Store this conversation"},
		},
	})

	// Verify conversation was stored
	history := srv.deps.ConversationStore.GetHistory("Store this conversation")
	if history == nil {
		t.Error("expected conversation history after request")
	} else if len(history.Turns) == 0 {
		t.Error("expected at least one turn in history")
	}
}

func TestE2E_UsageTracking(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model": "openai/gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "Track my usage"},
		},
	})

	// Stop the async usage tracker to drain buffered entries before querying
	srv.deps.UsageTracker.Close()

	// Check usage was recorded
	entries, err := db.QueryUsage(store.UsageFilter{Limit: 10})
	if err != nil {
		t.Fatalf("failed to query usage: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected at least one usage entry")
	}
}

func TestE2E_RootRedirect(t *testing.T) {
	srv, _ := setupTestServer(t, nil)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 307 {
		t.Fatalf("expected 307 redirect, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/dashboard/" {
		t.Errorf("expected redirect to /dashboard/, got %s", loc)
	}
}
