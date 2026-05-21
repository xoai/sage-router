package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"sage-router/internal/executor"
	"sage-router/pkg/canonical"
)

// M3 T3 — applyQuotaWindow: parse a response's rate-limit headers into the
// connection's best-effort QuotaWindow (AC4, AC10).

func setQuotaHeaders(kv map[string]string) http.Header {
	h := http.Header{}
	for k, v := range kv {
		h.Set(k, v)
	}
	return h
}

func TestApplyQuotaWindow_PopulatesFromProviderHeaders(t *testing.T) {
	cases := []struct {
		name          string
		provider      string
		headers       map[string]string
		wantKnown     bool
		wantRemaining int
		wantReset     bool // expect a non-zero ResetAt
	}{
		{
			name:     "openai remaining + reset",
			provider: "openai",
			headers: map[string]string{
				"x-ratelimit-remaining-requests": "42",
				"x-ratelimit-reset-requests":     "1m30s",
			},
			wantKnown: true, wantRemaining: 42, wantReset: true,
		},
		{
			name:     "anthropic remaining + reset",
			provider: "anthropic",
			headers: map[string]string{
				"anthropic-ratelimit-requests-remaining": "1000",
				"anthropic-ratelimit-requests-reset":     time.Now().Add(90 * time.Second).UTC().Format(time.RFC3339),
			},
			wantKnown: true, wantRemaining: 1000, wantReset: true,
		},
		{
			name:      "generic Retry-After only — reset known, remaining not reported",
			provider:  "gemini",
			headers:   map[string]string{"Retry-After": "60"},
			wantKnown: true, wantRemaining: -1, wantReset: true,
		},
		{
			name:      "no rate-limit headers — unknown window",
			provider:  "openai",
			headers:   map[string]string{"Content-Type": "application/json"},
			wantKnown: false, wantRemaining: -1, wantReset: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, db := setupTestServer(t, nil)
			connID := addConnection(t, srv, db, tc.provider, "primary", "apikey")

			srv.applyQuotaWindow(connID, tc.provider, setQuotaHeaders(tc.headers))

			qw := srv.deps.ProviderSelector.ConnectionByID(connID).QuotaWindow()
			if qw.Known != tc.wantKnown {
				t.Errorf("Known = %v, want %v", qw.Known, tc.wantKnown)
			}
			if qw.Remaining != tc.wantRemaining {
				t.Errorf("Remaining = %d, want %d", qw.Remaining, tc.wantRemaining)
			}
			if gotReset := !qw.ResetAt.IsZero(); gotReset != tc.wantReset {
				t.Errorf("ResetAt-set = %v, want %v (ResetAt=%v)", gotReset, tc.wantReset, qw.ResetAt)
			}
		})
	}
}

// AC10 — best-effort: applyQuotaWindow on an unknown connection ID is a no-op,
// never a panic.
func TestApplyQuotaWindow_UnregisteredConnIsNoOp(t *testing.T) {
	srv, _ := setupTestServer(t, nil)
	srv.applyQuotaWindow("does-not-exist", "openai",
		setQuotaHeaders(map[string]string{"x-ratelimit-remaining-requests": "5"}))
	// Reaching here without a panic is the assertion.
}

// quotaHeaderExecutor returns a canned Result whose Headers carry a rate-limit
// remaining count — drives the executeRequest wiring (AC4).
func quotaHeaderExecutor(providerID string, status int, remaining string) *mockExecutor {
	return &mockExecutor{
		providerID: providerID,
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			h := http.Header{"Content-Type": {"application/json"}}
			h.Set("x-ratelimit-remaining-requests", remaining)
			body := `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
			if status >= 400 {
				body = `{"error":{"message":"busy"}}`
			}
			return &executor.Result{
				StatusCode: status,
				Headers:    h,
				Body:       io.NopCloser(strings.NewReader(body)),
				Latency:    time.Millisecond,
			}, nil
		},
	}
}

// AC4 — applyQuotaWindow is wired into executeRequest's success return.
func TestExecuteRequest_PopulatesQuotaWindowOnSuccess(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  quotaHeaderExecutor("openai", 200, "99"),
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	conn := pickExecuteConn(t, srv, db, "openai", "primary")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	result, err := srv.executeRequest(r.Context(), r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-qw-ok", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body})
	if err != nil {
		t.Fatalf("executeRequest: %v", err)
	}
	if result != nil && result.Body != nil {
		result.Body.Close()
	}
	qw := srv.deps.ProviderSelector.ConnectionByID(conn.ID).QuotaWindow()
	if !qw.Known || qw.Remaining != 99 {
		t.Errorf("QuotaWindow after a 200 = %+v, want Known=true Remaining=99", qw)
	}
}

// AC4 — and into the 4xx/5xx branch (an intermediate response the success path
// never sees).
func TestExecuteRequest_PopulatesQuotaWindowOn5xx(t *testing.T) {
	executors := map[string]executor.Executor{
		"openai":  quotaHeaderExecutor("openai", 503, "3"),
		"default": &sentinelExecutor{t: t, providerID: "default"},
	}
	srv, db := setupTestServer(t, executors)
	conn := pickExecuteConn(t, srv, db, "openai", "primary")

	r, body := buildExecuteReq(t, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	result, _ := srv.executeRequest(r.Context(), r, body, canonical.FormatOpenAI, "openai", "gpt-4o", false, conn, nil, "req-qw-5xx", time.Now(), &requestContext{firstMsg: extractFirstUserMsg(body), requestBody: body})
	if result != nil && result.Body != nil {
		result.Body.Close()
	}
	qw := srv.deps.ProviderSelector.ConnectionByID(conn.ID).QuotaWindow()
	if !qw.Known || qw.Remaining != 3 {
		t.Errorf("QuotaWindow after a 503 = %+v, want Known=true Remaining=3", qw)
	}
}
