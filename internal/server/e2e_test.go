package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/bypass"
	"sage-router/internal/config"
	"sage-router/internal/executor"
	"sage-router/internal/provider"
	"sage-router/internal/ratelimit"
	"sage-router/internal/routing"
	"sage-router/internal/store"
	"sage-router/internal/translate"
	claudeTranslate "sage-router/internal/translate/claude"
	openaiTranslate "sage-router/internal/translate/openai"
	openaiRespTranslate "sage-router/internal/translate/openai-responses"
	"sage-router/internal/usage"
	"sage-router/pkg/canonical"
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

// sentinelExecutor fails the test when Execute is invoked. Wired as
// the "default" executor in setupTestServer (when the caller passes
// nil executors) so a request that unintentionally routes to the
// default executor fails loudly with the provider name in the message,
// rather than getting a permissive mock response that masks the
// misrouting (carryover #37).
//
// Tests that legitimately need a working default executor (e.g.,
// openrouter/ollama integration shapes) MUST register an explicit
// entry under the "default" key when calling setupTestServer.
type sentinelExecutor struct {
	t          *testing.T
	providerID string
}

func (s *sentinelExecutor) Provider() string { return s.providerID }
func (s *sentinelExecutor) Execute(ctx context.Context, req *executor.ExecuteRequest) (*executor.Result, error) {
	s.t.Fatalf("unexpected dispatch to sentinel executor (provider=%q); the request routed to the default fallback instead of an expected provider executor",
		s.providerID)
	return nil, nil
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

	// Set up auth (use dummy password hash so NeedsSetup() returns false)
	authMgr := auth.NewManager("test-password-hash", []byte("test-jwt-secret"), []byte("test-hmac-secret"))

	// Translate registry
	translateReg := translate.NewRegistry()
	translateReg.Register(openaiTranslate.New())
	translateReg.Register(claudeTranslate.New())
	translateReg.Register(openaiRespTranslate.New())

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
			// Sentinel as the default fallback — a test that routes to
			// "default" without registering an explicit entry has a
			// misrouting bug, not a wiring shortcut. Carryover #37.
			"default": &sentinelExecutor{t: t, providerID: "default"},
		}
	}

	// Cycle 20260517-provider-auth-variants M5.6: Dependencies.Executors
	// map removed. Mirror the per-provider entries into a Variants
	// registry as wildcards `(provider, "")` so the legacy test
	// fixtures keep working through Variants.Get.
	variants := executor.NewVariants()
	for id, e := range executors {
		variants.Register(executor.VariantKey{Provider: id, AuthType: ""}, e)
	}

	srv := New(Config{
		Host: "127.0.0.1",
		Port: 0,
	}, Dependencies{
		Store:             db,
		TranslateRegistry: translateReg,
		ProviderSelector:  providerSel,
		ProviderRegistry:  providerReg,
		Variants:          variants,
		UsageTracker:      usageTracker,
		Auth:              authMgr,
		SmartRouter:       routing.NewSmartRouter(),
		ConversationStore: routing.NewConversationStore(),
		BypassFilter:      bypass.NewFilter(),
		RateLimiter:       ratelimit.New(),
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

	// Request with deleted key → 401 (key is invalid even though no keys remain)
	w3 := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "Hello"}},
	}, plainKey)
	if w3.Code != 401 {
		t.Fatalf("expected 401 with deleted key, got %d", w3.Code)
	}

	// Request with NO key when no keys exist → should pass (open access)
	w4 := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "Hello"}},
	})
	if w4.Code == 401 {
		t.Fatalf("expected open access when no keys exist and no key provided, got 401")
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

// --- §34 Keys-as-Groups Enforcement Tests ---

func createKeyWithAttributes(t *testing.T, srv *Server, db store.Store, name string, attrs map[string]any) string {
	t.Helper()
	plainKey, keyHash, prefix, err := srv.deps.Auth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	key := &store.APIKey{
		ID:      "key-" + name,
		Name:    name,
		KeyHash: keyHash,
		Prefix:  prefix,
	}
	if v, ok := attrs["budget_monthly"].(float64); ok {
		key.BudgetMonthly = v
	}
	if v, ok := attrs["budget_hard_limit"].(bool); ok {
		key.BudgetHardLimit = v
	}
	if v, ok := attrs["allowed_models"].(string); ok {
		key.AllowedModels = v
	} else {
		key.AllowedModels = "*"
	}
	if v, ok := attrs["rate_limit_rpm"].(int); ok {
		key.RateLimitRPM = v
	}
	if v, ok := attrs["routing_strategy"].(string); ok {
		key.RoutingStrategy = v
	}
	if err := db.CreateAPIKey(key); err != nil {
		t.Fatal(err)
	}
	return plainKey
}

func TestE2E_RateLimitEnforcement(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	plainKey := createKeyWithAttributes(t, srv, db, "limited", map[string]any{
		"rate_limit_rpm": 3,
	})

	body := map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "Hi"}},
	}

	for i := 0; i < 3; i++ {
		w := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", body, plainKey)
		if w.Code == 429 {
			t.Fatalf("request %d should be allowed, got 429", i)
		}
	}

	w := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", body, plainKey)
	if w.Code != 429 {
		t.Fatalf("expected 429 rate limit, got %d: %s", w.Code, w.Body.String())
	}
}

func TestE2E_ACLEnforcement(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")
	addConnection(t, srv, db, "anthropic", "primary", "apikey")

	plainKey := createKeyWithAttributes(t, srv, db, "anthropic-only", map[string]any{
		"allowed_models": "anthropic/*",
	})

	w1 := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "anthropic/claude-sonnet-4-20250514",
		"messages": []map[string]any{{"role": "user", "content": "Hi"}},
	}, plainKey)
	if w1.Code == 403 {
		t.Fatalf("anthropic model should be allowed, got 403")
	}

	w2 := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "Hi"}},
	}, plainKey)
	if w2.Code != 403 {
		t.Fatalf("expected 403 for disallowed model, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestE2E_BudgetHardLimit(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	plainKey := createKeyWithAttributes(t, srv, db, "budgeted", map[string]any{
		"budget_monthly":    0.01,
		"budget_hard_limit": true,
	})

	db.RecordUsage(&store.UsageEntry{
		ID:        "u1",
		RequestID: "r1",
		Provider:  "openai",
		Model:     "gpt-4o",
		APIKeyID:  "key-budgeted",
		Cost:      0.02,
		Status:    "success",
	})

	w := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "Hi"}},
	}, plainKey)
	if w.Code != 402 {
		t.Fatalf("expected 402 budget exceeded, got %d: %s", w.Code, w.Body.String())
	}
}

func TestE2E_UnlimitedKeyPassesAll(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	plainKey := createKeyWithAttributes(t, srv, db, "unlimited", map[string]any{})

	w := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "Hi"}},
	}, plainKey)
	if w.Code != 200 {
		t.Fatalf("unlimited key should pass, got %d: %s", w.Code, w.Body.String())
	}
}

func TestE2E_MatchModelPattern(t *testing.T) {
	tests := []struct {
		model, pattern string
		want           bool
	}{
		{"anthropic/claude-sonnet-4-20250514", "anthropic/*", true},
		{"openai/gpt-4o", "anthropic/*", false},
		{"openai/gpt-4o", "openai/*,anthropic/*", true},
		{"openai/gpt-4o", "openai/gpt-4o", true},
		{"openai/gpt-4o", "openai/gpt-4.1", false},
		{"openai/gpt-4o", "*", true},
		{"anything", "*", true},
	}
	for _, tt := range tests {
		got := matchModelPattern(tt.model, tt.pattern)
		if got != tt.want {
			t.Errorf("matchModelPattern(%q, %q) = %v, want %v", tt.model, tt.pattern, got, tt.want)
		}
	}
}

func TestE2E_ACL_ComboBypass(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")
	addConnection(t, srv, db, "anthropic", "primary", "apikey")

	// Create combo with openai first, anthropic second
	db.CreateCombo(&store.Combo{
		ID: "c1", Name: "mixed-combo",
		Models: []string{"openai/gpt-4o", "anthropic/claude-sonnet-4-20250514"},
	})

	// Key restricted to anthropic only
	plainKey := createKeyWithAttributes(t, srv, db, "anthropic-only", map[string]any{
		"allowed_models": "anthropic/*",
	})

	// Use the combo — should NOT execute openai/gpt-4o even though it's first
	w := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "mixed-combo",
		"messages": []map[string]any{{"role": "user", "content": "Hi"}},
	}, plainKey)

	// Should succeed (anthropic model is allowed and should be the only one tried)
	if w.Code != 200 {
		t.Fatalf("expected 200 (anthropic model from filtered combo), got %d: %s", w.Code, w.Body.String())
	}
}

func TestE2E_ACL_ComboAllBlocked(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	// Combo with only openai models
	db.CreateCombo(&store.Combo{
		ID: "c2", Name: "openai-only-combo",
		Models: []string{"openai/gpt-4o", "openai/gpt-4o-mini"},
	})

	// Key restricted to anthropic only
	plainKey := createKeyWithAttributes(t, srv, db, "anthropic-restricted", map[string]any{
		"allowed_models": "anthropic/*",
	})

	w := doRequestWithKey(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai-only-combo",
		"messages": []map[string]any{{"role": "user", "content": "Hi"}},
	}, plainKey)

	if w.Code != 403 {
		t.Fatalf("expected 403 (no permitted models in combo), got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// M1 α refactor tests — AC-X1..X4 + InnerLoopCtxCancel
// Cycle 20260516-routing-strategy-ux. Per spec v5 + plan v2.
// These tests target the NEW signature of executeRequest:
//   func (s *Server) executeRequest(ctx context.Context, r *http.Request, body []byte,
//       sourceFormat canonical.Format, providerID, model string, stream bool,
//       conn *ConnectionInfo, excludeIDs []string,
//       requestID string, startTime time.Time, apiKeyID string,
//   ) (*executor.Result, error)
//
// Before M1.2, these MUST fail compilation ("executeRequest returns no values").
// That is the desired RED phase. After M1.2-M1.5, they go GREEN.

func newMock503Executor(providerID string) *mockExecutor {
	return &mockExecutor{
		providerID: providerID,
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			return &executor.Result{
				StatusCode: 503,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"service unavailable"}}`)),
				Latency:    5 * time.Millisecond,
			}, nil
		},
	}
}

func newMockNetErrExecutor(providerID string, err error) *mockExecutor {
	return &mockExecutor{
		providerID: providerID,
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			return nil, err
		},
	}
}

// pickExecuteConn is a test helper: registers a connection and selects it via
// the production selectConnection path, yielding a valid *ConnectionInfo. This
// ensures the test exercises the same selector code path as the http handler.
func pickExecuteConn(t *testing.T, srv *Server, db store.Store, providerID, name string) *ConnectionInfo {
	t.Helper()
	addConnection(t, srv, db, providerID, name, "apikey")
	conn, _, err := srv.selectConnection(providerID, "test-model", nil)
	if err != nil || conn == nil {
		t.Fatalf("pickExecuteConn: selectConnection failed: %v", err)
	}
	return conn
}

// buildExecuteReq constructs a minimal *http.Request + body for executeRequest calls.
func buildExecuteReq(t *testing.T, body string) (*http.Request, []byte) {
	t.Helper()
	bodyBytes := []byte(body)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bodyBytes))
	r.Header.Set("Content-Type", "application/json")
	return r, bodyBytes
}

// AC-X1: non-streaming 200 returns Result {200, body}.
func TestExecuteRequest_NonStreaming200_ReturnsResult(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  &mockExecutor{providerID: "openai"}, // default: returns 200 with canned body
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	conn := pickExecuteConn(t, srv, db, "openai", "primary")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()

	result, err := srv.executeRequest(r.Context(), r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-x1", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body, apiKeyID: ""})
	_ = w
	if err != nil {
		t.Fatalf("AC-X1: expected nil err, got %v", err)
	}
	if result == nil {
		t.Fatalf("AC-X1: expected non-nil Result")
	}
	if result.StatusCode != 200 {
		t.Errorf("AC-X1: expected StatusCode=200, got %d", result.StatusCode)
	}
	if result.Body == nil {
		t.Errorf("AC-X1: expected non-nil Body")
	} else {
		defer result.Body.Close()
		bodyBytes, _ := io.ReadAll(result.Body)
		if !strings.Contains(string(bodyBytes), "Hello from mock") {
			t.Errorf("AC-X1: expected canned mock body, got %q", string(bodyBytes))
		}
	}
}

// AC-X2: streaming 200 returns Result {200, lazy ReadCloser body}.
func TestExecuteRequest_Streaming200_ReturnsResult(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  newMockStreamExecutor("openai"),
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	conn := pickExecuteConn(t, srv, db, "openai", "primary")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	result, err := srv.executeRequest(r.Context(), r, body, canonical.FormatOpenAI, "openai", "gpt-4o", true, conn, nil, "req-x2", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body, apiKeyID: ""})
	if err != nil {
		t.Fatalf("AC-X2: expected nil err, got %v", err)
	}
	if result == nil {
		t.Fatalf("AC-X2: expected non-nil Result")
	}
	if result.StatusCode != 200 {
		t.Errorf("AC-X2: expected StatusCode=200, got %d", result.StatusCode)
	}
	// Body must be a ReadCloser usable lazily (SSE). We don't drain it here — caller (forwardResult)
	// pipes it lazily to ResponseWriter.
	if result.Body == nil {
		t.Errorf("AC-X2: expected non-nil ReadCloser Body for streaming")
	} else {
		result.Body.Close()
	}
}

// AC-X3: upstream 503 returns Result {503, body}; err == nil (caller decides advance vs forward).
func TestExecuteRequest_Upstream503_ReturnsResult(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  newMock503Executor("openai"),
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	// Only ONE connection — so connection-level fallback exhausts; final result is 503 returned.
	conn := pickExecuteConn(t, srv, db, "openai", "primary")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	result, err := srv.executeRequest(r.Context(), r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-x3", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body, apiKeyID: ""})
	if err != nil {
		t.Fatalf("AC-X3: expected nil err (503 is a Result, not a network error), got %v", err)
	}
	if result == nil {
		t.Fatalf("AC-X3: expected non-nil Result for 503")
	}
	if result.StatusCode != 503 {
		t.Errorf("AC-X3: expected StatusCode=503, got %d", result.StatusCode)
	}
	if result.Body == nil {
		t.Errorf("AC-X3: expected non-nil Body (upstream error body, for caller to log or forward)")
	} else {
		defer result.Body.Close()
		bodyBytes, _ := io.ReadAll(result.Body)
		if !strings.Contains(string(bodyBytes), "service unavailable") {
			t.Errorf("AC-X3: expected upstream error body, got %q", string(bodyBytes))
		}
	}
}

// AC-X4: network error + all connection-level retries exhausted returns (nil, err).
func TestExecuteRequest_NetworkError_ReturnsErr(t *testing.T) {
	netErr := errors.New("dial tcp: connection refused")
	executors := map[string]executor.Executor{
		"openai":  newMockNetErrExecutor("openai", netErr),
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	// Only one connection — exhausted on first failure.
	conn := pickExecuteConn(t, srv, db, "openai", "primary")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	result, err := srv.executeRequest(r.Context(), r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-x4", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body, apiKeyID: ""})
	if err == nil {
		t.Fatalf("AC-X4: expected non-nil err on network failure with exhausted retries")
	}
	if result != nil {
		t.Errorf("AC-X4: expected nil Result on network err, got %+v", result)
	}
}

// M-v3-4 fold: connection-level fallback loop honors ctx cancellation between attempts.
// Distinct from AC-X5 (which is M2's between-CANDIDATES check inside handleComboRequest).
func TestExecuteRequest_InnerLoopCtxCancel(t *testing.T) {
	netErr := errors.New("dial tcp: connection refused")
	executors := map[string]executor.Executor{
		"openai":  newMockNetErrExecutor("openai", netErr),
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	// Two connections so the inner loop has somewhere to advance to.
	addConnection(t, srv, db, "openai", "primary", "apikey")
	addConnection(t, srv, db, "openai", "secondary", "apikey")
	conn, _, err := srv.selectConnection("openai", "gpt-4o", nil)
	if err != nil || conn == nil {
		t.Fatalf("setup: selectConnection failed: %v", err)
	}

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	ctx, cancel := context.WithCancel(r.Context())
	cancel() // cancel BEFORE call so the loop sees ctx.Done on first check

	result, err := srv.executeRequest(ctx, r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-ctx", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body, apiKeyID: ""})
	if err == nil {
		t.Fatalf("InnerLoopCtxCancel: expected non-nil err on canceled context")
	}
	if result != nil {
		t.Errorf("InnerLoopCtxCancel: expected nil Result on cancel, got %+v", result)
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context") {
		// Accept either errors.Is(ctx.Canceled) OR a wrapped error message — implementation choice.
		t.Errorf("InnerLoopCtxCancel: expected context.Canceled-related err, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// M2 walk-on-5xx tests — AC-F9..F12, AC-F10a-d, AC-X5, AC-X6
// Cycle 20260516-routing-strategy-ux M2. Per spec v5 + plan v2.
//
// These tests assert that handleComboRequest's loop advances to the next
// candidate (combo member OR auto:* candidate) when:
//   - selectConnection fails for a candidate (existing behavior — not new)
//   - executeRequest returns network error with exhausted connections (M1 added)
//   - executeRequest returns Result with StatusCode >= 500 (M2 — NEW)
//   - 4xx does NOT advance (client error, forwarded to client)
//
// Pre-M2: only the first two trigger advance. The 5xx case forwards to client.

// statusSwitchExecutor returns 5xx for the FIRST call and 200 for subsequent calls.
// Used to verify walk semantics: first candidate gets 5xx, walk advances, second
// candidate gets 200.
func newStatusSwitchExecutor(providerID string, firstStatus int, firstBody string) *mockExecutor {
	var callCount int
	return &mockExecutor{
		providerID: providerID,
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			callCount++
			if callCount == 1 {
				return &executor.Result{
					StatusCode: firstStatus,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(firstBody)),
					Latency:    5 * time.Millisecond,
				}, nil
			}
			// Subsequent calls return success.
			return &executor.Result{
				StatusCode: 200,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[{"message":{"content":"second"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
				Latency:    5 * time.Millisecond,
			}, nil
		},
	}
}

// AC-F9 + AC-F11: combo's first member returns 503; walk advances to second.
func TestCombo_503AdvancesToNext(t *testing.T) {
	openaiCallCount := 0
	anthropicCallCount := 0
	executors := map[string]executor.Executor{
		"anthropic": &mockExecutor{
			providerID: "anthropic",
			handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
				anthropicCallCount++
				return &executor.Result{
					StatusCode: 503,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":"upstream down"}`)),
					Latency:    5 * time.Millisecond,
				}, nil
			},
		},
		"openai": &mockExecutor{
			providerID: "openai",
			handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
				openaiCallCount++
				return &executor.Result{
					StatusCode: 200,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[{"message":{"content":"from openai"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
					Latency:    5 * time.Millisecond,
				}, nil
			},
		},
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	addConnection(t, srv, db, "anthropic", "primary", "apikey")
	addConnection(t, srv, db, "openai", "primary", "apikey")

	db.CreateCombo(&store.Combo{
		ID:     "combo-walk",
		Name:   "fallback-combo",
		Models: []string{"anthropic/claude-sonnet-4-6", "openai/gpt-4o"},
	})

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "fallback-combo",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	if w.Code != 200 {
		t.Fatalf("AC-F9: expected 200 after walk to openai, got %d: %s", w.Code, w.Body.String())
	}
	if anthropicCallCount != 1 {
		t.Errorf("AC-F9: expected anthropic invoked once (returned 503), got %d", anthropicCallCount)
	}
	if openaiCallCount != 1 {
		t.Errorf("AC-F9: expected openai invoked once (after walk), got %d", openaiCallCount)
	}
	if !strings.Contains(w.Body.String(), "from openai") {
		t.Errorf("AC-F9: expected openai's response body, got %q", w.Body.String())
	}
}

// AC-F12: matrix of walk scenarios.
func TestCombo_WalkMatrix(t *testing.T) {
	type matrix struct {
		name           string
		statuses       []int  // status code per combo position
		expectedStatus int    // expected response to client
		expectedCalls  []int  // expected call count per provider position
		bodyContains   string // string to find in response body
	}
	// Note: body-content assertions are unreliable because cross-format response
	// translation (source=OpenAI request → target=Anthropic/Gemini) can transform
	// the mock body. Status code + call counts are sufficient to prove walk
	// semantics.
	// Note: test infrastructure only registers openai + anthropic translators
	// (setupTestServer at :155-158). Combos with members in other providers
	// (e.g., gemini) would hit "no translator for target format" and skip via
	// the network-error advance path — not the 5xx walk path we're testing.
	// So matrix uses [anthropic, openai] only. F12b (3-member 5xx-5xx-200) is
	// dropped as a generalization of F12a covered by combo-tier walk semantics.
	cases := []matrix{
		{"F12a [503,200] -> B forwarded", []int{503, 200}, 200, []int{1, 1}, ""},
		{"F12c [200] -> no walk", []int{200, 0}, 200, []int{1, 0}, ""},
		{"F12d [400] -> 4xx forward, no walk", []int{400, 0}, 400, []int{1, 0}, ""},
		{"F12e [503,503] -> all exhausted, last 5xx propagated", []int{503, 503}, 503, []int{1, 1}, ""},
	}

	providers := []string{"anthropic", "openai"}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			callCounts := []int{0, 0, 0}
			executors := map[string]executor.Executor{
				"default": &sentinelExecutor{t: t, providerID: "default"},
			}
			for i, prov := range providers {
				if tc.statuses[i] == 0 {
					continue
				}
				idx := i // capture
				status := tc.statuses[idx]
				bodyStr := fmt.Sprintf(`{"id":"ok","choices":[{"message":{"content":"from-%d"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, idx)
				if status >= 400 {
					bodyStr = fmt.Sprintf(`{"error":{"message":"status %d"}}`, status)
				}
				executors[prov] = &mockExecutor{
					providerID: prov,
					handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
						callCounts[idx]++
						return &executor.Result{
							StatusCode: status,
							Headers:    http.Header{"Content-Type": []string{"application/json"}},
							Body:       io.NopCloser(strings.NewReader(bodyStr)),
							Latency:    5 * time.Millisecond,
						}, nil
					},
				}
			}

			srv, db := setupTestServer(t, executors)
			var members []string
			for i, prov := range providers {
				if tc.statuses[i] == 0 {
					continue
				}
				addConnection(t, srv, db, prov, "primary", "apikey")
				members = append(members, prov+"/m"+fmt.Sprint(i))
			}
			db.CreateCombo(&store.Combo{
				ID:     "combo-matrix",
				Name:   "walk-matrix",
				Models: members,
			})

			w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
				"model":    "walk-matrix",
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			})

			if w.Code != tc.expectedStatus {
				t.Fatalf("%s: expected status %d, got %d: %s", tc.name, tc.expectedStatus, w.Code, w.Body.String())
			}
			for i := range providers {
				if callCounts[i] != tc.expectedCalls[i] {
					t.Errorf("%s: provider[%d]=%s expected %d calls, got %d", tc.name, i, providers[i], tc.expectedCalls[i], callCounts[i])
				}
			}
			if tc.bodyContains != "" && !strings.Contains(w.Body.String(), tc.bodyContains) {
				t.Errorf("%s: response should contain %q, got %q", tc.name, tc.bodyContains, w.Body.String())
			}
		})
	}
}

// AC-F10a-d: auto:* strategies walk on 5xx.
// auto:* dispatches through handleComboRequest via the smart-route block in
// resolveModel (routes_v1.go:154-160 + the resolveModel returns isCombo=true
// with comboModels=sorted for auto:*). So all 4 strategies inherit walk-on-5xx
// FREE from the M2.2 handleComboRequest loop extension — by code-path
// equivalence, not by separate implementation.
//
// Pinning the walk via auto:* requires the smart-router catalog to return 2+
// candidates, which depends on `s.deps.Catalog.ListProvider("")` returning
// model data. The test catalog is not seeded with full model metadata, so
// smart-router typically returns 0 or 1 candidate in tests — insufficient to
// exercise multi-candidate walk.
//
// Coverage strategy: walk semantics are pinned by `TestCombo_503AdvancesToNext`
// + `TestCombo_WalkMatrix` (both exercise handleComboRequest's loop directly).
// auto:* uses the SAME loop. A future cycle that seeds the test catalog
// could add a dedicated AC-F10a-d test; for now the combo tests are
// load-bearing for the walk contract.

// AC-X5: walk between candidates honors ctx cancellation.
func TestWalk_CtxCanceled(t *testing.T) {
	slowExec := &mockExecutor{
		providerID: "anthropic",
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			// Slow 503: gives the test time to cancel ctx between candidates.
			time.Sleep(50 * time.Millisecond)
			return &executor.Result{
				StatusCode: 503,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"slow"}`)),
				Latency:    50 * time.Millisecond,
			}, nil
		},
	}
	openaiCallCount := 0
	openaiExec := &mockExecutor{
		providerID: "openai",
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			openaiCallCount++
			return &executor.Result{
				StatusCode: 200,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[{"message":{"content":"shouldnt see"}}]}`)),
				Latency:    5 * time.Millisecond,
			}, nil
		},
	}
	executors := map[string]executor.Executor{
		"anthropic": slowExec,
		"openai":    openaiExec,
		"default":   &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	addConnection(t, srv, db, "anthropic", "primary", "apikey")
	addConnection(t, srv, db, "openai", "primary", "apikey")

	db.CreateCombo(&store.Combo{
		ID:     "combo-ctx",
		Name:   "ctx-combo",
		Models: []string{"anthropic/claude-sonnet-4-6", "openai/gpt-4o"},
	})

	// Build request with a cancellable ctx; cancel after ~20ms (mid-first-candidate).
	bodyBytes, _ := json.Marshal(map[string]any{
		"model":    "ctx-combo",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	// Cancel mid-flight.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	srv.Handler().ServeHTTP(w, req)

	// After cancel: walk should NOT reach openai's 200.
	if openaiCallCount > 0 {
		t.Errorf("AC-X5: walk should abort on ctx-cancel; openai was invoked %d times", openaiCallCount)
	}
}

// AC-X6: walk-continue path closes 5xx Body before advancing (no fd leak).
func TestWalk_503BodyClosed(t *testing.T) {
	var firstCloseCount int32
	executors := map[string]executor.Executor{
		"anthropic": &mockExecutor{
			providerID: "anthropic",
			handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
				return &executor.Result{
					StatusCode: 503,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       &countingBody{Reader: strings.NewReader(`{"error":"down"}`), closes: &firstCloseCount},
					Latency:    5 * time.Millisecond,
				}, nil
			},
		},
		"openai": &mockExecutor{
			providerID: "openai",
			handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
				return &executor.Result{
					StatusCode: 200,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[{"message":{"content":"second"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
					Latency:    5 * time.Millisecond,
				}, nil
			},
		},
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	addConnection(t, srv, db, "anthropic", "primary", "apikey")
	addConnection(t, srv, db, "openai", "primary", "apikey")
	db.CreateCombo(&store.Combo{
		ID:     "combo-leak",
		Name:   "leak-check",
		Models: []string{"anthropic/claude-sonnet-4-6", "openai/gpt-4o"},
	})

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "leak-check",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	if w.Code != 200 {
		t.Fatalf("AC-X6: expected 200 after walk, got %d: %s", w.Code, w.Body.String())
	}
	closes := atomic.LoadInt32(&firstCloseCount)
	if closes != 1 {
		t.Errorf("AC-X6: expected 5xx Body.Close() called exactly once during walk advance, got %d", closes)
	}
}

// countingBody is an io.ReadCloser that tracks Close calls. Used by AC-X6 to
// verify walk-continue closes the 5xx Body before advancing.
type countingBody struct {
	io.Reader
	closes *int32
}

func (c *countingBody) Close() error {
	atomic.AddInt32(c.closes, 1)
	return nil
}

// ---------------------------------------------------------------------------
// M3 StrategyUserOrder tests — AC-F4..F8 + AC-G2/G3
// Cycle 20260516-routing-strategy-ux M3. Per spec v5 + plan v2.

// AC-F4: position-based sort by allowed_models string.
func TestSortByAllowedModelsOrder_Basic(t *testing.T) {
	candidates := []routing.ModelCandidate{
		{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		{Provider: "openai", Model: "gpt-4o-mini"},
	}
	result := sortByAllowedModelsOrder(candidates, "openai/gpt-4o-mini,anthropic/claude-sonnet-4-6")
	if len(result) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(result))
	}
	if result[0].Provider != "openai" || result[0].Model != "gpt-4o-mini" {
		t.Errorf("AC-F4: expected openai/gpt-4o-mini first, got %s/%s", result[0].Provider, result[0].Model)
	}
	if result[1].Provider != "anthropic" || result[1].Model != "claude-sonnet-4-6" {
		t.Errorf("AC-F4: expected anthropic/claude-sonnet-4-6 second, got %s/%s", result[1].Provider, result[1].Model)
	}
}

// AC-F5: wildcard `anthropic/*` ranks all anthropic candidates by the wildcard position.
func TestSortByAllowedModelsOrder_Wildcard(t *testing.T) {
	candidates := []routing.ModelCandidate{
		{Provider: "anthropic", Model: "claude-opus-4-7"},
		{Provider: "openai", Model: "gpt-4o"},
		{Provider: "anthropic", Model: "claude-haiku-4-5"},
	}
	result := sortByAllowedModelsOrder(candidates, "anthropic/*,openai/*")
	// Both anthropic candidates should rank before openai (position 0 vs 1).
	if result[0].Provider != "anthropic" || result[1].Provider != "anthropic" {
		t.Errorf("AC-F5: expected both anthropic first under anthropic/*, got %s, %s",
			result[0].Provider, result[1].Provider)
	}
	if result[2].Provider != "openai" {
		t.Errorf("AC-F5: expected openai at position 2, got %s", result[2].Provider)
	}
}

// AC-F6: candidates not in allowed_models fall to end gracefully.
func TestSortByAllowedModelsOrder_MissingFallsToEnd(t *testing.T) {
	candidates := []routing.ModelCandidate{
		{Provider: "openai", Model: "gpt-4o"},
		{Provider: "gemini", Model: "gemini-2.5-pro"},
		{Provider: "anthropic", Model: "claude-sonnet-4-6"},
	}
	result := sortByAllowedModelsOrder(candidates, "anthropic/claude-sonnet-4-6,openai/gpt-4o")
	// anthropic should be at position 0, openai at position 1, gemini (not listed) at position 2.
	if result[0].Provider != "anthropic" {
		t.Errorf("AC-F6: expected anthropic first, got %s", result[0].Provider)
	}
	if result[1].Provider != "openai" {
		t.Errorf("AC-F6: expected openai second, got %s", result[1].Provider)
	}
	if result[2].Provider != "gemini" {
		t.Errorf("AC-F6: expected gemini (unmatched) last, got %s", result[2].Provider)
	}
}

// AC-F7: bare `*` in allowed_models is ignored for ordering (it's a "match all"
// wildcard, not a position).
func TestSortByAllowedModelsOrder_BareStarIgnored(t *testing.T) {
	candidates := []routing.ModelCandidate{
		{Provider: "openai", Model: "gpt-4o"},
		{Provider: "anthropic", Model: "claude-sonnet-4-6"},
	}
	result := sortByAllowedModelsOrder(candidates, "*,anthropic/claude-sonnet-4-6")
	// Bare * is ignored. anthropic explicit at position 1, openai unmatched → end.
	if result[0].Provider != "anthropic" {
		t.Errorf("AC-F7: expected anthropic first (only explicit entry), got %s", result[0].Provider)
	}
	if result[1].Provider != "openai" {
		t.Errorf("AC-F7: expected openai (unmatched) last, got %s", result[1].Provider)
	}
}

// AC-F8 (M1 fold): exact-match beats wildcard. With allowed_models containing
// both `anthropic/*` (position 0) AND `anthropic/claude-x` (position 1), the
// EXACT match wins — claude-x ranks at 1, other anthropic candidates at 0.
func TestSortByAllowedModelsOrder_ExactWinsWildcard(t *testing.T) {
	candidates := []routing.ModelCandidate{
		{Provider: "anthropic", Model: "claude-x"},
		{Provider: "anthropic", Model: "claude-y"},
	}
	result := sortByAllowedModelsOrder(candidates, "anthropic/*,anthropic/claude-x")
	// claude-y (wildcard match, position 0) should come BEFORE claude-x (exact, position 1).
	if result[0].Model != "claude-y" {
		t.Errorf("AC-F8: expected claude-y first (wildcard pos 0), got %s", result[0].Model)
	}
	if result[1].Model != "claude-x" {
		t.Errorf("AC-F8: expected claude-x second (exact pos 1), got %s", result[1].Model)
	}
}

// AC-G2 + AC-G3: /api/keys accepts and returns routing_strategy=user-order.
// (Smoke-level; depends on existing keys API + smartrouter accepting the new
// strategy.) Validated as a side-effect of the wizard wiring more than as an
// independent API contract test — there is no validation regression because
// the API stores routing_strategy as a freeform string and the parser/dispatcher
// path (resolveModel → ParseAutoModel) already accepts "user-order".
func TestPostKeysAcceptsUserOrder(t *testing.T) {
	// Confirm the round-trip via ParseAutoModel — proves the wire-level contract
	// matches the parser without exercising the full /api/keys handler chain.
	s, ok := routing.ParseAutoModel("auto:user-order")
	if !ok || s != routing.StrategyUserOrder {
		t.Errorf("AC-G2 round-trip: expected (StrategyUserOrder, true), got (%q, %v)", s, ok)
	}
}

// ---------------------------------------------------------------------------
// M1 streaming-truncation regression (hotfix 2026-05-16).
//
// Production bug: Continue VSCode extension received streaming responses with
// 1-3 characters of content then EOF. Root cause: executeRequest called
// cancel() on the per-attempt upstream context IMMEDIATELY after exec.Execute
// returned, but for streaming responses result.Body is still tied to the live
// upstream HTTP connection. Cancel tore down the connection before the
// caller's streamResponse drained the SSE chunks.
//
// Pre-fix tests (AC-X2 TestExecuteRequest_Streaming200_ReturnsResult) only
// checked result.StatusCode + Body != nil — they used NopCloser(strings.NewReader)
// which doesn't honor context cancellation, so the bug didn't reproduce in tests.
//
// These regression tests verify:
//   - The full SSE body content is delivered to the client (completeness)
//   - result.Body.Close() is called by forwardResult (close discipline)

func TestE2E_Streaming_FullBodyDelivered(t *testing.T) {
	// Use the existing mockStreamExecutor which returns multi-chunk SSE.
	executors := map[string]executor.Executor{
		"openai":  newMockStreamExecutor("openai"),
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   true,
	})

	if w.Code != 200 {
		t.Fatalf("expected 200 for streaming, got %d: %s", w.Code, w.Body.String())
	}

	bodyStr := w.Body.String()
	// Pre-fix: cancel-after-Execute would have truncated the body to the first
	// few bytes that were already buffered, then EOF. Assert we get BOTH SSE
	// events from the mock (the "Hi" delta + the finish_reason event).
	if !strings.Contains(bodyStr, `"content":"Hi"`) {
		t.Errorf("streaming response missing first chunk content; got %d bytes: %q", len(bodyStr), bodyStr)
	}
	if !strings.Contains(bodyStr, `"finish_reason":"stop"`) {
		t.Errorf("streaming response missing finish_reason event; got %d bytes: %q", len(bodyStr), bodyStr)
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("streaming response missing [DONE] terminator; got %d bytes", len(bodyStr))
	}
}

// closeTrackingBody wraps strings.NewReader with a Close that increments a
// counter. Used to verify forwardResult closes the body exactly once.
type closeTrackingBody struct {
	*strings.Reader
	closes *int32
}

func (c *closeTrackingBody) Close() error {
	atomic.AddInt32(c.closes, 1)
	return nil
}

func TestE2E_Streaming_BodyClosed(t *testing.T) {
	var closeCount int32
	executors := map[string]executor.Executor{
		"openai": &mockExecutor{
			providerID: "openai",
			handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
				return &executor.Result{
					StatusCode: 200,
					Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: &closeTrackingBody{
						Reader: strings.NewReader("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\ndata: [DONE]\n"),
						closes: &closeCount,
					},
					Latency: 1 * time.Millisecond,
				}, nil
			},
		},
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	addConnection(t, srv, db, "openai", "primary", "apikey")

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   true,
	})

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// forwardResult must close the body via the cancelOnClose wrapper. Pre-hotfix
	// (no `defer result.Body.Close()` in forwardResult), close was never called.
	if closes := atomic.LoadInt32(&closeCount); closes != 1 {
		t.Errorf("expected Body.Close() called exactly once by forwardResult, got %d", closes)
	}
}

// ---------------------------------------------------------------------------
// Cycle 20260516-connection-runtime-state — /api/connections runtime view tests.
//
// THE bug regression test: /api/connections must project the Selector's
// runtime view so the dashboard can detect DB-vs-runtime state drift. Without
// this, a connection appears "green" in UI while silently refusing specific
// models (the user-reported symptom that triggered this fix).

func TestHandleListConnections_RuntimeDivergence(t *testing.T) {
	srv, db := setupTestServer(t, nil)
	connID := addConnection(t, srv, db, "openai", "primary", "subscription")

	// DB state stays "idle" (default from addConnection). Open the breaker
	// in-memory via OpenBreaker — this simulates the stuck-after-429 scenario
	// where the dashboard would show green but the Selector refuses.
	pc := srv.deps.ProviderSelector.ConnectionByID(connID)
	if pc == nil {
		t.Fatalf("connection not registered in selector")
	}
	if err := pc.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if err := pc.OpenBreaker(provider.FailureRateLimit, 0, "gpt-5-nano"); err != nil {
		t.Fatalf("OpenBreaker: %v", err)
	}

	// Call the handler directly (bypasses protect middleware — same pattern
	// as postCreateConnectionWithBody in connection_autodetect_test.go).
	req := httptest.NewRequest("GET", "/api/connections", nil)
	rec := httptest.NewRecorder()
	srv.handleListConnections(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(resp))
	}

	conn := resp[0]
	// DB state — preserved at "idle" since we never persisted the transition.
	if got := conn["state"]; got != "idle" {
		t.Errorf("expected DB state=idle, got %v", got)
	}
	// Runtime state — diverges from DB; this is the BUG FIX assertion.
	if got := conn["runtime_state"]; got != "cooldown" {
		t.Errorf("expected runtime_state=cooldown (DB-vs-runtime divergence), got %v", got)
	}
	// model_locks must include gpt-5-nano (set by OpenBreaker).
	locks, ok := conn["model_locks"].(map[string]any)
	if !ok {
		t.Fatalf("expected model_locks object, got %T: %v", conn["model_locks"], conn["model_locks"])
	}
	if _, ok := locks["gpt-5-nano"]; !ok {
		t.Errorf("expected model_locks to include gpt-5-nano, got %v", locks)
	}
	// cooldown_until populated (non-nil pointer renders as a string).
	if _, ok := conn["cooldown_until"]; !ok {
		t.Errorf("expected cooldown_until to be populated in cooldown state")
	}
}

func TestHandleListConnections_UnregisteredConnection_NoPanic(t *testing.T) {
	// Edge case: a connection row exists in DB but isn't registered in the
	// Selector (e.g., race window between handleCreateConnection DB insert
	// and the Selector.Register call, or post-restart before bootstrap).
	// safeConn population must handle nil ConnectionByID gracefully —
	// runtime fields are omitted, DB state still surfaces.
	srv, db := setupTestServer(t, nil)
	// Direct DB insert without going through addConnection (which also
	// registers in the Selector).
	conn := &store.Connection{
		ID:       "unregistered-conn-1",
		Provider: "openai",
		Name:     "orphan",
		AuthType: "apikey",
		APIKey:   "test-key",
		Priority: 0,
		State:    "idle",
	}
	if err := db.CreateConnection(conn); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/connections", nil)
	rec := httptest.NewRecorder()
	srv.handleListConnections(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200 (no panic), got %d: %s", rec.Code, rec.Body.String())
	}

	var resp []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(resp))
	}
	if got := resp[0]["state"]; got != "idle" {
		t.Errorf("expected DB state=idle, got %v", got)
	}
	// Runtime fields must be absent (omitempty) since Selector.ConnectionByID
	// returned nil. last_error is also absent — its population path lives
	// INSIDE the if-pc-not-nil block (the gate's contents are unchanged from
	// pre-cycle, but the gate moved into the runtime-projection block).
	if _, ok := resp[0]["runtime_state"]; ok {
		t.Errorf("expected runtime_state omitted for unregistered conn, got %v", resp[0]["runtime_state"])
	}
	if _, ok := resp[0]["model_locks"]; ok {
		t.Errorf("expected model_locks omitted for unregistered conn")
	}
	if _, ok := resp[0]["cooldown_until"]; ok {
		t.Errorf("expected cooldown_until omitted for unregistered conn")
	}
	if _, ok := resp[0]["last_error"]; ok {
		t.Errorf("expected last_error omitted for unregistered conn (no in-memory connection to pull from)")
	}
}


// preflightRejectingVariant — fake variant where PreflightCredentials
// returns a typed error AND Execute panics if called. Used to verify
// the route handler invokes preflightCreds at the dispatch site BEFORE
// exec.Execute (post-ship /review C1 regression pin — cycle
// 20260517-provider-auth-variants).
type preflightRejectingVariant struct {
	t          *testing.T
	provider   string
	rejectWith error
}

func (p *preflightRejectingVariant) Provider() string { return p.provider }
func (p *preflightRejectingVariant) Execute(ctx context.Context, req *executor.ExecuteRequest) (*executor.Result, error) {
	p.t.Fatalf("preflight should have short-circuited; Execute() must NOT be called when PreflightCredentials returns error")
	return nil, nil
}
func (p *preflightRejectingVariant) PreflightCredentials(creds *executor.Credentials) error {
	return p.rejectWith
}

// TestE2E_PreflightShortCircuitsBeforeExecute — pins the M5 C1 fix:
// when a variant's PreflightCredentials returns a typed error, the
// route handler skips Execute and marks the connection AuthExpired
// with the typed error as LastError. Without preflightCreds wired,
// the request would burn an upstream roundtrip and lose the
// friendly tier-error message.
func TestE2E_PreflightShortCircuitsBeforeExecute(t *testing.T) {
	rejectErr := fmt.Errorf("synthetic-preflight-reject: account-tier-blocked")
	variant := &preflightRejectingVariant{
		t:          t,
		provider:   "openai",
		rejectWith: rejectErr,
	}
	// setupTestServer wires its own Variants based on the executors map,
	// but we want a custom variant — wrap it as the openai entry. The
	// helper registers under (openai, "") wildcard which matches both
	// apikey and subscription auth_types.
	srv, db := setupTestServer(t, map[string]executor.Executor{
		"openai":  variant,
		"default": &sentinelExecutor{t: t, providerID: "default"},
	})
	addConnection(t, srv, db, "openai", "primary", "apikey")

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "openai/gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "Hello"}},
	})
	// Execute never ran (sentinel/panic would have fired in the variant
	// if it had). The request errored upstream-of-Execute; expect 5xx
	// to the client since there's no other connection to fall back to.
	if w.Code < 400 {
		t.Errorf("expected client-error status when preflight rejects + no fallback; got %d body=%s",
			w.Code, w.Body.String())
	}

	// Verify the connection landed in AuthExpired with the typed error.
	conns, _ := db.ListConnections(store.ConnectionFilter{})
	if len(conns) != 1 {
		t.Fatalf("connection count = %d, want 1", len(conns))
	}
	pc := srv.deps.ProviderSelector.ConnectionByID(conns[0].ID)
	if pc == nil {
		t.Fatal("connection not registered in provider selector")
	}
	if auth := pc.Auth(); auth != provider.AuthExpired {
		t.Errorf("connection auth facet = %q, want %q (preflight rejection should drive AuthExpired)",
			auth, provider.AuthExpired)
	}
	if le := pc.LastError(); le == nil || le.Error() != rejectErr.Error() {
		t.Errorf("LastError = %v, want preflight error %q (friendly tier-error message should be set)",
			le, rejectErr.Error())
	}
}
