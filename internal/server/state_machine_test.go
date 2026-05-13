package server

import (
	"sync"
	"testing"

	"sage-router/internal/auth"
	"sage-router/internal/provider"
)

func TestIsModelRejection(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		// Positive — model-level patterns.
		{"openai model not available", `{"error":{"message":"The model gpt-9 is not available"}}`, true},
		{"openai model not supported", `{"error":"model claude-foo not supported"}`, true},
		{"anthropic forbidden model", `{"type":"forbidden","error":{"message":"model access denied"}}`, true},
		{"capital case", `Model claude-opus-9 is UNAVAILABLE`, true},
		{"model invalid", `model claude-something is invalid for this account`, true},

		// Negative — generic auth or other errors.
		{"generic auth fail", `{"error":"invalid token"}`, false},
		{"rate limit", `{"error":"too many requests"}`, false},
		{"empty body", ``, false},
		{"random garbage", `<html>500 Internal Server Error</html>`, false},
		{"talks about user, not model", `user is not available right now`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isModelRejection([]byte(tc.body))
			if got != tc.want {
				t.Errorf("isModelRejection(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestIsModelRejection_TruncatesLargeBodies(t *testing.T) {
	// Construct a body where the matching phrase appears AFTER the truncation
	// window, so we should NOT match. Padding chosen well over maxScan (4 KiB).
	padding := make([]byte, 8*1024)
	for i := range padding {
		padding[i] = 'x'
	}
	body := append(padding, []byte("model not available")...)
	if isModelRejection(body) {
		t.Error("isModelRejection should not scan past the 4 KiB prefix")
	}

	// And a body where the phrase IS in the first 4 KiB → match.
	body2 := append([]byte("model not available"), padding...)
	if !isModelRejection(body2) {
		t.Error("isModelRejection should match phrase in the first 4 KiB")
	}
}

// fakeServerForStateMachine builds a minimal Server with just a
// ProviderSelector wired for markConnectionResult tests.
func fakeServerForStateMachine(t *testing.T, conn *provider.Connection) *Server {
	t.Helper()
	sel := provider.NewSelector()
	sel.Register(conn)
	return &Server{deps: Dependencies{ProviderSelector: sel}}
}

func TestMarkConnectionResult_401InvalidatesCredentialAndMarksAuthExpired(t *testing.T) {
	c := provider.NewConnection("c1", "openai", "test", 0, "subscription")
	// Seed the credential cache via the test-only setter so we can verify
	// InvalidateCredential drops it.
	c.SetCredentialForTest(&auth.Credential{Provider: "openai", AccessToken: "tok"})
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	s := fakeServerForStateMachine(t, c)
	s.markConnectionResult("c1", "gpt-5", 401, []byte(`{"error":"unauthorized"}`), nil)

	if got, want := c.State(), provider.StateAuthExpired; got != want {
		t.Errorf("state = %v, want %v", got, want)
	}
	if c.HasCredential() {
		t.Error("expected cred cache to be cleared after 401")
	}
}

func TestMarkConnectionResult_403WithModelRejectionAddsToDenylist(t *testing.T) {
	c := provider.NewConnection("c1", "openai", "test", 0, "subscription")
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	s := fakeServerForStateMachine(t, c)
	s.markConnectionResult("c1", "gpt-9",
		403,
		[]byte(`{"error":{"message":"The model gpt-9 is not available with this token"}}`),
		nil,
	)

	// Model-tier rejections should NOT take the whole connection out of
	// service — the token is still good for every other model it has
	// access to. The connection returns to Idle, only the rejected
	// model is denylisted. AuthExpired is reserved for actual auth
	// failures (token revoked / invalid / expired).
	if got, want := c.State(), provider.StateIdle; got != want {
		t.Errorf("state = %v, want %v", got, want)
	}
	if c.CanServeModel("gpt-9") {
		t.Error("CanServeModel(gpt-9) should be false after model-rejection 403")
	}
	if !c.CanServeModel("gpt-4o") {
		t.Error("CanServeModel(gpt-4o) should still be true — denylist is per-model")
	}
	// Cred cache should remain intact — model rejection doesn't mean
	// the token is bad. (HasCredential is true iff a cred is cached.)
	// We don't seed one in this test, so the assertion is just that we
	// didn't call InvalidateCredential erroneously, which would log noise.
}

func TestMarkConnectionResult_403WithoutModelRejectionStillAuthExpires(t *testing.T) {
	// Regression: a 403 whose body does NOT match isModelRejection (a
	// real auth failure rather than a model-tier limit) must still take
	// the connection through AuthExpired + InvalidateCredential.
	c := provider.NewConnection("c1", "openai", "test", 0, "subscription")
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	s := fakeServerForStateMachine(t, c)
	s.markConnectionResult("c1", "gpt-5",
		403,
		[]byte(`{"error":{"message":"insufficient_permissions"}}`),
		nil,
	)

	if got, want := c.State(), provider.StateAuthExpired; got != want {
		t.Errorf("state = %v, want %v (generic 403 should still AuthExpire)", got, want)
	}
}

func TestMarkConnectionResult_429StillRateLimits(t *testing.T) {
	// Regression: rate-limit path is unchanged.
	c := provider.NewConnection("c1", "openai", "test", 0, "apikey")
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	s := fakeServerForStateMachine(t, c)
	s.markConnectionResult("c1", "gpt-5", 429, nil, nil)

	if got, want := c.State(), provider.StateCooldown; got != want {
		t.Errorf("state = %v, want %v", got, want)
	}
}

func TestMarkConnectionResult_200MarksSuccess(t *testing.T) {
	c := provider.NewConnection("c1", "openai", "test", 0, "apikey")
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	s := fakeServerForStateMachine(t, c)
	s.markConnectionResult("c1", "gpt-5", 200, nil, nil)

	if got, want := c.State(), provider.StateIdle; got != want {
		t.Errorf("state = %v, want %v", got, want)
	}
}

// AC8b: a streaming request that receives 401 mid-stream produces the same
// state transition + cache invalidation as a non-streaming 401. The path
// that handles streaming errors funnels through markConnectionResult the
// same way — we verify both the synchronous-handler path (already tested
// above) AND a simulated "stream broke with 401" call site.
func TestMarkConnectionResult_StreamingAuthFailureMidStream(t *testing.T) {
	c := provider.NewConnection("c1", "anthropic", "test", 0, "subscription")
	c.SetCredentialForTest(&auth.Credential{Provider: "anthropic", AccessToken: "expiring-tok"})
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if !c.HasCredential() {
		t.Fatal("precondition: cred should be cached")
	}

	s := fakeServerForStateMachine(t, c)

	// Simulate: stream started successfully (200 OK), then closed with a
	// trailing 401 — the executor surfaces this as a markConnectionResult
	// call with statusCode=401 after the stream ends. The caller (stream
	// handler) reads the closing error body for any model-rejection signal.
	closingBody := []byte(`{"type":"error","error":{"type":"authentication_error","message":"token expired"}}`)
	s.markConnectionResult("c1", "claude-sonnet-4", 401, closingBody, nil)

	if got, want := c.State(), provider.StateAuthExpired; got != want {
		t.Errorf("state = %v, want %v after streaming 401", got, want)
	}
	if c.HasCredential() {
		t.Error("expected cred cache to be cleared after streaming 401")
	}
	// Non-model-rejection body: model is NOT in denylist. The user can
	// retry on the same model (against a different connection).
	if !c.CanServeModel("claude-sonnet-4") {
		t.Error("token-only auth error should not blacklist the model")
	}
}

// Stress: many concurrent 401s on the same connection should not race the
// invalidate or the state transitions.
func TestMarkConnectionResult_ConcurrentAuthFailures(t *testing.T) {
	c := provider.NewConnection("c1", "openai", "test", 0, "subscription")
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	s := fakeServerForStateMachine(t, c)

	// First 401 transitions Active → AuthExpired. Subsequent calls find the
	// connection already in AuthExpired and silently no-op (the transition
	// returns ErrTransitionRejected which markConnectionResult swallows).
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.markConnectionResult("c1", "gpt-5", 401, []byte("unauthorized"), nil)
		}()
	}
	wg.Wait()

	if got, want := c.State(), provider.StateAuthExpired; got != want {
		t.Errorf("final state = %v, want %v", got, want)
	}
}
