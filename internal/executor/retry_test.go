package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ── Mock executor ──

type mockResponse struct {
	result *Result
	err    error
}

type mockExecutor struct {
	calls     int
	responses []mockResponse
}

func (m *mockExecutor) Provider() string { return "mock" }

func (m *mockExecutor) Execute(ctx context.Context, req *ExecuteRequest) (*Result, error) {
	i := m.calls
	m.calls++
	if i >= len(m.responses) {
		return nil, fmt.Errorf("no more mock responses")
	}
	return m.responses[i].result, m.responses[i].err
}

// ── Helpers ──

func emptyBody() io.ReadCloser {
	return io.NopCloser(strings.NewReader(""))
}

func okResult() *Result {
	return &Result{
		StatusCode: 200,
		Headers:    http.Header{},
		Body:       emptyBody(),
		Latency:    time.Millisecond,
	}
}

func statusResult(code int) *Result {
	return &Result{
		StatusCode: code,
		Headers:    http.Header{},
		Body:       emptyBody(),
		Latency:    time.Millisecond,
	}
}

func fastRetryConfig(maxRetries int) RetryConfig {
	return RetryConfig{
		MaxRetries: maxRetries,
		BaseDelay:  time.Millisecond,
		MaxDelay:   10 * time.Millisecond,
	}
}

func baseReq() *ExecuteRequest {
	return &ExecuteRequest{
		Model:  "test-model",
		Body:   []byte(`{}`),
		Stream: false,
	}
}

// ── Tests ──

func TestRetryExecutorSuccessOnFirst(t *testing.T) {
	mock := &mockExecutor{
		responses: []mockResponse{
			{result: okResult()},
		},
	}
	re := NewRetryExecutor(mock, fastRetryConfig(3))

	result, err := re.Execute(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", result.StatusCode)
	}
	if mock.calls != 1 {
		t.Errorf("expected 1 call, got %d", mock.calls)
	}
}

func TestRetryExecutorSuccessAfterRetries(t *testing.T) {
	mock := &mockExecutor{
		responses: []mockResponse{
			{result: statusResult(503)},
			{result: statusResult(503)},
			{result: okResult()},
		},
	}
	re := NewRetryExecutor(mock, fastRetryConfig(3))

	result, err := re.Execute(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", result.StatusCode)
	}
	if mock.calls != 3 {
		t.Errorf("expected 3 calls, got %d", mock.calls)
	}
}

func TestRetryExecutorExhausted(t *testing.T) {
	mock := &mockExecutor{
		responses: []mockResponse{
			{result: statusResult(429)},
			{result: statusResult(429)},
			{result: statusResult(429)},
		},
	}
	re := NewRetryExecutor(mock, fastRetryConfig(2))

	result, err := re.Execute(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("expected no error (last result returned), got %v", err)
	}
	if result.StatusCode != 429 {
		t.Errorf("expected status 429, got %d", result.StatusCode)
	}
	if mock.calls != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 retries), got %d", mock.calls)
	}
}

func TestRetryExecutorNetworkError(t *testing.T) {
	mock := &mockExecutor{
		responses: []mockResponse{
			{err: fmt.Errorf("connection refused")},
			{err: fmt.Errorf("connection reset")},
			{result: okResult()},
		},
	}
	re := NewRetryExecutor(mock, fastRetryConfig(3))

	result, err := re.Execute(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", result.StatusCode)
	}
	if mock.calls != 3 {
		t.Errorf("expected 3 calls, got %d", mock.calls)
	}
}

func TestRetryExecutorNonRetryable(t *testing.T) {
	mock := &mockExecutor{
		responses: []mockResponse{
			{result: statusResult(400)},
		},
	}
	re := NewRetryExecutor(mock, fastRetryConfig(3))

	result, err := re.Execute(context.Background(), baseReq())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.StatusCode != 400 {
		t.Errorf("expected status 400, got %d", result.StatusCode)
	}
	if mock.calls != 1 {
		t.Errorf("expected 1 call (no retry for 400), got %d", mock.calls)
	}
}

func TestRetryExecutorStreamingNoRetry(t *testing.T) {
	mock := &mockExecutor{
		responses: []mockResponse{
			{result: okResult()},
		},
	}
	re := NewRetryExecutor(mock, fastRetryConfig(3))

	req := baseReq()
	req.Stream = true

	result, err := re.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", result.StatusCode)
	}
	if mock.calls != 1 {
		t.Errorf("expected 1 call, got %d", mock.calls)
	}
}

func TestRetryExecutorContextCanceled(t *testing.T) {
	// First call returns a retryable status so the executor enters the
	// backoff wait. We cancel the context before the delay elapses.
	mock := &mockExecutor{
		responses: []mockResponse{
			{result: statusResult(503)},
			{result: okResult()},
		},
	}
	re := NewRetryExecutor(mock, RetryConfig{
		MaxRetries: 3,
		BaseDelay:  time.Second, // long enough that context check fires first
		MaxDelay:   time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the backoff select picks up ctx.Done()

	_, err := re.Execute(ctx, baseReq())
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// ── OverrideCapabilities delegation ──
//
// Models Discovery M3.4b post-review fix. Production wiring at
// cmd/sage-router/main.go wraps every Executor in NewRetryExecutor
// before storing in `deps.Executors`. Without explicit delegation, the
// type assertion `exec.(CapabilityOverrider)` in
// server.buildSmartCandidates would fail in production even after
// M3.5 implements OverrideCapabilities on the inner executors.

type capOverridingMock struct {
	mockExecutor
	overrideFor string
}

func (c *capOverridingMock) OverrideCapabilities(model string, base Capabilities) Capabilities {
	if model == c.overrideFor {
		base.SupportsThinking = true
	}
	return base
}

func TestRetryExecutor_OverrideCapabilities_DelegatesToInner(t *testing.T) {
	inner := &capOverridingMock{overrideFor: "model-x"}
	re := NewRetryExecutor(inner, RetryConfig{})

	// re must be observable as CapabilityOverrider for the
	// server.buildSmartCandidates type-assertion to succeed.
	overrider, ok := any(re).(CapabilityOverrider)
	if !ok {
		t.Fatal("*RetryExecutor does not satisfy CapabilityOverrider — production override path is dead")
	}

	out := overrider.OverrideCapabilities("model-x", Capabilities{})
	if !out.SupportsThinking {
		t.Errorf("override for model-x: SupportsThinking=false, want true (inner flipped it)")
	}
	out = overrider.OverrideCapabilities("model-other", Capabilities{})
	if out.SupportsThinking {
		t.Errorf("override for model-other: SupportsThinking=true, want false (inner left it alone)")
	}
}

func TestRetryExecutor_OverrideCapabilities_InnerLacksInterface(t *testing.T) {
	// mockExecutor (base) doesn't implement CapabilityOverrider — the
	// RetryExecutor wrap must still return the interface (so the
	// production type-assertion succeeds) but the body becomes identity.
	inner := &mockExecutor{}
	re := NewRetryExecutor(inner, RetryConfig{})

	overrider, ok := any(re).(CapabilityOverrider)
	if !ok {
		t.Fatal("*RetryExecutor missing CapabilityOverrider")
	}
	in := Capabilities{SupportsImages: true, SupportsTools: true}
	out := overrider.OverrideCapabilities("any-model", in)
	if out != in {
		t.Errorf("identity broken: got %+v, want %+v (inner has no override)", out, in)
	}
}
