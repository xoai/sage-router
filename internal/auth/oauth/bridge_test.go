package oauth

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHandler records what the bridge passes to OnFlowComplete and
// returns a configurable (connID, redirectTo, err).
type fakeHandler struct {
	mu         sync.Mutex
	gotFlow    *Flow
	gotResp    *TokenResponse
	connID     string
	redirectTo string
	err        error
}

func (f *fakeHandler) OnFlowComplete(ctx context.Context, flow *Flow, resp *TokenResponse) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotFlow = flow
	f.gotResp = resp
	return f.connID, f.redirectTo, f.err
}

// withTokenServer spins up an httptest-like backend that pretends to be the
// provider's token endpoint, and points the test flow's Exchange at it.
// The bridge handler will call Exchange on the bridge's behalf.
func withTokenServer(t *testing.T, response string, status int) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, "http://" + ln.Addr().String()
}

func startBridge(t *testing.T, h BridgeHandler) *Bridge {
	t.Helper()
	b := NewBridgeForTest(h)
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = b.Stop(ctx)
	})
	return b
}

func TestBridge_StartAssignsEphemeralPorts(t *testing.T) {
	b := startBridge(t, &fakeHandler{})
	if b.Health("openai") != HealthAvailable {
		t.Errorf("openai health = %q, want available", b.Health("openai"))
	}
	if b.Health("anthropic") != HealthAvailable {
		t.Errorf("anthropic health = %q, want available", b.Health("anthropic"))
	}
	if !strings.HasPrefix(b.CallbackURL("openai"), "http://127.0.0.1:") {
		t.Errorf("CallbackURL malformed: %s", b.CallbackURL("openai"))
	}
	if !strings.HasSuffix(b.CallbackURL("openai"), "/auth/callback") {
		t.Errorf("openai CallbackURL must end in /auth/callback; got %s", b.CallbackURL("openai"))
	}
	if !strings.HasSuffix(b.CallbackURL("anthropic"), "/callback") {
		t.Errorf("anthropic CallbackURL must end in /callback; got %s", b.CallbackURL("anthropic"))
	}
}

func TestBridge_DegradedModeWhenPortBusy(t *testing.T) {
	// Hold 127.0.0.1:0 ports won't conflict — but we can simulate by
	// using a fixed port and binding twice. We'll grab a port via a
	// real listener and then create a Bridge with that exact port set
	// for openai.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	heldPort := ln.Addr().(*net.TCPAddr).Port

	b := NewBridgeForTest(&fakeHandler{})
	b.openaiPort = heldPort // collision target
	// anthropicPort stays at 0 (ephemeral) so the other half can bind.
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = b.Stop(ctx)
	})

	if got := b.Health("openai"); got != HealthPortBusy {
		t.Errorf("openai health = %q, want %q (port busy)", got, HealthPortBusy)
	}
	if got := b.Health("anthropic"); got != HealthAvailable {
		t.Errorf("anthropic health = %q, want available (separate port)", got)
	}
}

func TestBridge_TryRebind_FlipsToAvailableAfterRelease(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	heldPort := ln.Addr().(*net.TCPAddr).Port

	b := NewBridgeForTest(&fakeHandler{})
	b.openaiPort = heldPort
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = b.Stop(ctx)
	})

	if b.Health("openai") != HealthPortBusy {
		t.Fatal("precondition: openai should be port_busy")
	}

	// Release the port and retry the bind.
	_ = ln.Close()
	// Give the OS a moment to release.
	time.Sleep(50 * time.Millisecond)
	if got := b.TryRebind("openai"); got != HealthAvailable {
		t.Errorf("TryRebind = %q, want %q after port release", got, HealthAvailable)
	}
}

func TestBridge_RegisterAndConsume(t *testing.T) {
	b := startBridge(t, &fakeHandler{})

	f, _ := NewFlow("openai", "Work")
	b.Register(f)

	got, ok := b.Consume(f.State)
	if !ok {
		t.Fatal("Consume should return the registered flow")
	}
	if got.State != f.State {
		t.Errorf("State mismatch")
	}
	// Second consume returns false (LoadAndDelete).
	if _, ok := b.Consume(f.State); ok {
		t.Error("Consume should be single-shot")
	}
}

func TestBridge_ConsumeUnknownStateReturnsFalse(t *testing.T) {
	b := startBridge(t, &fakeHandler{})
	if _, ok := b.Consume("not-a-real-state"); ok {
		t.Error("unknown state should not Consume")
	}
}

func TestBridge_SweeperEvictsExpiredFlows(t *testing.T) {
	b := startBridge(t, &fakeHandler{})

	f, _ := NewFlow("openai", "Work")
	b.Register(f)
	// flowTTL is 200ms in test config; sleep past it + one sweep interval.
	time.Sleep(350 * time.Millisecond)
	if _, ok := b.Consume(f.State); ok {
		t.Error("expired flow should have been swept")
	}
}

func TestBridge_ConcurrentRegisterConsume_NoRaces(t *testing.T) {
	b := startBridge(t, &fakeHandler{})

	const N = 50
	var wg sync.WaitGroup
	flows := make([]*Flow, N)
	for i := 0; i < N; i++ {
		f, _ := NewFlow("openai", "Concurrent")
		flows[i] = f
		b.Register(f)
	}
	var consumed int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(state string) {
			defer wg.Done()
			if _, ok := b.Consume(state); ok {
				atomic.AddInt32(&consumed, 1)
			}
		}(flows[i].State)
	}
	wg.Wait()
	if int(consumed) != N {
		t.Errorf("consumed = %d, want %d", consumed, N)
	}
}

func TestBridge_CallbackHappyPath(t *testing.T) {
	// Stand up a token-endpoint mock and point the flow's Exchange at it.
	_, tokURL := withTokenServer(t, `{
		"access_token": "atk-fresh",
		"refresh_token": "rtk-fresh",
		"expires_in": 3600
	}`, 200)

	h := &fakeHandler{redirectTo: "https://dashboard.example.com/oauth-complete"}
	b := startBridge(t, h)

	f, err := NewFlow("openai", "Work")
	if err != nil {
		t.Fatal(err)
	}
	f.tokenURL = tokURL
	b.Register(f)

	// Drive the GET callback ourselves.
	callbackURL := b.CallbackURL("openai") + "?code=ok&state=" + f.State
	client := &http.Client{
		// Don't auto-follow — we want to see the 302 the bridge issues.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
	resp, err := client.Get(callbackURL)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://dashboard.example.com/oauth-complete" {
		t.Errorf("Location = %q, want dashboard redirect", got)
	}

	// Verify the handler saw the parsed token + flow.
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gotFlow == nil || h.gotResp == nil {
		t.Fatal("handler was not invoked")
	}
	if h.gotResp.AccessToken != "atk-fresh" {
		t.Errorf("handler saw AccessToken = %q, want atk-fresh", h.gotResp.AccessToken)
	}
}

func TestBridge_CallbackTerminalPageWhenHandlerReturnsEmptyRedirect(t *testing.T) {
	_, tokURL := withTokenServer(t, `{"access_token":"a","expires_in":1}`, 200)
	h := &fakeHandler{} // redirectTo zero-value → terminal page
	b := startBridge(t, h)

	f, _ := NewFlow("openai", "Work")
	f.tokenURL = tokURL
	b.Register(f)

	resp, err := http.Get(b.CallbackURL("openai") + "?code=ok&state=" + f.State)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestBridge_CallbackInvalidStateReturns400(t *testing.T) {
	b := startBridge(t, &fakeHandler{})
	resp, err := http.Get(b.CallbackURL("openai") + "?code=x&state=does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestBridge_CallbackMissingParamsReturns400(t *testing.T) {
	b := startBridge(t, &fakeHandler{})
	// No code, no state.
	resp, err := http.Get(b.CallbackURL("openai"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestBridge_CallbackCrossPortConfusion(t *testing.T) {
	// Register an openai flow but submit its state to the anthropic
	// callback port — should 400 (defense against confused-deputy attacks).
	b := startBridge(t, &fakeHandler{})

	f, _ := NewFlow("openai", "Work")
	b.Register(f)

	resp, err := http.Get(b.CallbackURL("anthropic") + "?code=x&state=" + f.State)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for cross-port confusion", resp.StatusCode)
	}
}

func TestBridge_CallbackTokenExchangeFailureReturns400(t *testing.T) {
	_, tokURL := withTokenServer(t, `{"error":"invalid_grant"}`, 400)
	b := startBridge(t, &fakeHandler{})

	f, _ := NewFlow("openai", "Work")
	f.tokenURL = tokURL
	b.Register(f)

	resp, err := http.Get(b.CallbackURL("openai") + "?code=bad&state=" + f.State)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestBridge_StopIsIdempotent(t *testing.T) {
	b := startBridge(t, &fakeHandler{})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := b.Stop(ctx); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	if err := b.Stop(ctx); err != nil {
		t.Errorf("second Stop should be no-op: %v", err)
	}
	if got := b.Health("openai"); got != HealthStopped {
		t.Errorf("health after Stop = %q, want %q", got, HealthStopped)
	}
}

func TestBridge_Status_LifecyclePendingThroughComplete(t *testing.T) {
	_, tokURL := withTokenServer(t, `{"access_token":"a","expires_in":3600}`, 200)
	h := &fakeHandler{connID: "conn-abc"}
	b := startBridge(t, h)

	f, _ := NewFlow("openai", "Work")
	f.tokenURL = tokURL

	// 1. Unknown state before Register.
	if got := b.Status(f.State); got.Status != StatusUnknown {
		t.Errorf("unknown state should report StatusUnknown; got %+v", got)
	}

	// 2. Pending after Register.
	b.Register(f)
	if got := b.Status(f.State); got.Status != StatusPending {
		t.Errorf("after Register: status = %v, want pending", got.Status)
	}

	// 3. Complete after callback runs.
	resp, err := http.Get(b.CallbackURL("openai") + "?code=ok&state=" + f.State)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := b.Status(f.State)
	if got.Status != StatusComplete {
		t.Errorf("after callback: status = %v, want complete", got.Status)
	}
	if got.ConnectionID != "conn-abc" {
		t.Errorf("ConnectionID = %q, want conn-abc", got.ConnectionID)
	}
}

func TestBridge_Status_ErrorRecordedOnTokenExchangeFailure(t *testing.T) {
	_, tokURL := withTokenServer(t, `{"error":"invalid_grant"}`, 400)
	b := startBridge(t, &fakeHandler{})

	f, _ := NewFlow("openai", "Work")
	f.tokenURL = tokURL
	b.Register(f)

	resp, _ := http.Get(b.CallbackURL("openai") + "?code=bad&state=" + f.State)
	resp.Body.Close()

	got := b.Status(f.State)
	if got.Status != StatusError {
		t.Errorf("status after exchange failure = %v, want error", got.Status)
	}
	if got.Error == "" {
		t.Error("StatusError should include a non-empty Error message")
	}
}

// regression: confirm that the CallbackURL we compute resolves correctly
// against url.Parse — used as the redirect_uri sent to the provider.
func TestBridge_CallbackURLParseable(t *testing.T) {
	b := startBridge(t, &fakeHandler{})
	for _, p := range []string{"openai", "anthropic"} {
		u, err := url.Parse(b.CallbackURL(p))
		if err != nil {
			t.Errorf("%s CallbackURL unparseable: %v", p, err)
		}
		if u.Scheme != "http" {
			t.Errorf("%s scheme = %q, want http", p, u.Scheme)
		}
	}
}
