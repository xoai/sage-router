package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDefault_OverrideCapabilities — Models Discovery M3.5.
// DefaultExecutor is the OpenAI-compat path shared by openai /
// openrouter / github-copilot (when routed via OpenAI-compat) /
// ollama / custom endpoints. Identity body for the M3 baseline —
// per plan §3.5 the interface is wired so a future patch (e.g.,
// GPT-5 capability inheritance) doesn't need to rewire callers.
func TestDefault_OverrideCapabilities(t *testing.T) {
	e := NewDefaultExecutor("openai", "", nil)

	// Compile-time + runtime interface check.
	var _ CapabilityOverrider = e

	cases := []struct {
		name  string
		model string
		base  Capabilities
	}{
		{"gpt-4o", "gpt-4o", Capabilities{SupportsImages: true, SupportsTools: true}},
		{"gpt-4.1", "gpt-4.1", Capabilities{SupportsImages: true, SupportsTools: true}},
		{"o3", "o3", Capabilities{SupportsTools: true, SupportsThinking: true}},
		{"future gpt-5", "gpt-5", Capabilities{SupportsImages: true, SupportsTools: true, SupportsThinking: true}},
		{"empty model", "", Capabilities{}},
		{"all false", "gpt-3.5-turbo", Capabilities{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.OverrideCapabilities(c.model, c.base)
			if got != c.base {
				t.Errorf("identity violated: got %+v, want %+v", got, c.base)
			}
		})
	}
}

// TestDefaultExecutor_URLConstruction_DefaultEndpoint — AC-B2..B4 of
// 20260515-chat-routing-fix. The contract at default.go:22 says baseURL
// is the provider's API ROOT (e.g. "https://api.openai.com/v1"). The
// executor appends "/chat/completions" as the default endpoint when
// req.Endpoint == "". This test pins that contract via a real HTTP
// roundtrip to a captured httptest.NewServer — production-path-faithful
// (per plan-review M1 commitment, NOT a private helper).
//
// All three openai/openrouter/ollama default-executor variants share
// the same logic; per-provider behavior is only in main.go's wiring
// (verified separately by Task 11's full repo run).
func TestDefaultExecutor_URLConstruction_DefaultEndpoint(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	pool := NewClientPool()
	// The baseURL is `<server.URL>/v1` mirroring the production wiring
	// for openai (`https://api.openai.com/v1` after the AC-B1 fix).
	exec := NewDefaultExecutor("openai", server.URL+"/v1", pool)

	creds := &Credentials{AuthType: "apikey", APIKey: "sk-test"}
	result, err := exec.Execute(context.Background(), &ExecuteRequest{
		Model:       "gpt-4.1-nano",
		Body:        []byte(`{"model":"gpt-4.1-nano","messages":[]}`),
		Credentials: creds,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	defer result.Body.Close()

	want := "/v1/chat/completions"
	if capturedPath != want {
		t.Errorf("URL path: got %q want %q (URL doubling regression?)", capturedPath, want)
	}
}

// TestDefaultExecutor_URLConstruction_CustomEndpoint — AC-B5. When
// req.Endpoint is set, the executor appends it to baseURL verbatim
// (no default-endpoint append). Pins the contract for any future
// per-connection custom-endpoint feature.
func TestDefaultExecutor_URLConstruction_CustomEndpoint(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	pool := NewClientPool()
	exec := NewDefaultExecutor("openai", server.URL+"/v1", pool)

	creds := &Credentials{AuthType: "apikey", APIKey: "sk-test"}
	result, err := exec.Execute(context.Background(), &ExecuteRequest{
		Model:       "gpt-4.1-nano",
		Body:        []byte(`{}`),
		Credentials: creds,
		Endpoint:    "/some/future/path",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	defer result.Body.Close()

	want := "/v1/some/future/path"
	if capturedPath != want {
		t.Errorf("URL path: got %q want %q", capturedPath, want)
	}
}

// TestDefaultExecutor_OpenAISubscription_RoutesToResponses removed in
// cycle 20260517-provider-auth-variants M2.6.1 — the openai+subscription
// → /responses endpoint routing was deleted from DefaultExecutor (it was
// wrong-path; pointed at api.openai.com/v1/responses which ChatGPT
// subscribers can't reach). (openai, subscription) now routes via
// CodexSubscriptionExecutor to chatgpt.com/backend-api/codex/responses;
// see internal/executor/codex_subscription_test.go::TestCodexSubscription_GoldenURL.

// TestDefaultExecutor_SubscriptionPrefersExchangedToken removed in cycle
// 20260517-provider-auth-variants M2.6.6 — ExchangedToken plumbing was
// the wrong-path RFC 8693 chain (memory `f32bbc73`). The variant
// abstraction now routes (openai, subscription) to CodexSubscriptionExecutor
// which uses the PKCE access_token directly. See
// internal/executor/codex_subscription_test.go::TestCodexSubscription_GoldenHeaders
// for the per-variant header pin.

// TestDefaultExecutor_AnySubscription_StillChatCompletions: per M2.6.1
// the DefaultExecutor's openai+subscription /responses routing was
// removed entirely; the subscription branch now uses the bare
// /chat/completions endpoint with Bearer auth. This test pins that
// behavior (mirrors the original NonOpenAISubscription test, now
// generalized — the path is provider-agnostic for any wildcard
// `(provider, "")` variant registered with DefaultExecutor).
func TestDefaultExecutor_AnySubscription_StillChatCompletions(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	pool := NewClientPool()
	exec := NewDefaultExecutor("openrouter", server.URL+"/v1", pool)

	creds := &Credentials{AuthType: "subscription", AccessToken: "sub-token"}
	result, err := exec.Execute(context.Background(), &ExecuteRequest{
		Model:       "anthropic/claude-3",
		Body:        []byte(`{}`),
		Credentials: creds,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	defer result.Body.Close()

	want := "/v1/chat/completions"
	if capturedPath != want {
		t.Errorf("non-openai subscription URL path: got %q want %q (special-case is openai-only)", capturedPath, want)
	}
}
