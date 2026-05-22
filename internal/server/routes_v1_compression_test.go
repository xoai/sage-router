package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"sage-router/internal/compress"
	"sage-router/internal/executor"
	"sage-router/pkg/canonical"
)

// M4 T8 — the compression pipeline wiring: the Compress stage is reached in
// executeRequest for a compression_enabled request and skipped otherwise, and
// reqCtx.tokensBefore carries the estimate out (AC8 wiring, AC9 carrier +
// negative case).
//
// Strict "Compress runs before InjectCacheHints" ordering is not unit-tested:
// the two stages mutate disjoint regions (tool-result content vs. system
// blocks — AC5), so order is non-observable and non-load-bearing. The Compress
// block is textually placed before the cache-hint block in executeRequest.
//
// The upstream body is JSON, so an ANSI ESC byte in the tool-result is
// serialized as the escape sequence  — the substring "u001b" is the
// marker the assertions look for (it never occurs in ordinary text).
func TestExecuteRequest_CompressionWiring(t *testing.T) {
	var capturedBody string
	exec := &mockExecutor{
		providerID: "openai",
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			capturedBody = string(req.Body)
			return &executor.Result{
				StatusCode: 200,
				Headers:    http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)),
				Latency: time.Millisecond,
			}, nil
		},
	}
	srv, db := setupTestServer(t, map[string]executor.Executor{
		"openai":  exec,
		"default": &sentinelExecutor{t: t, providerID: "default"},
	})
	// setupTestServer leaves Compressor nil; wire a real one for this test.
	cmp, err := compress.NewCompressor()
	if err != nil {
		t.Fatalf("NewCompressor: %v", err)
	}
	srv.deps.Compressor = cmp

	// A request whose tool-result message carries compressible noise — ANSI
	// escape sequences (real ESC bytes) and a long run of identical lines.
	// The Catalog is nil here, so the pre-flight gate takes the "unknown"
	// path and compression runs regardless of size.
	noisy := "\x1b[31mERROR\x1b[0m\n" + strings.Repeat("dup line\n", 30)
	reqObj := map[string]any{
		"model": "openai/gpt-4o",
		"messages": []map[string]any{
			{"role": "user", "content": "run it"},
			{"role": "tool", "tool_call_id": "t1", "content": noisy},
		},
	}
	bodyBytes, _ := json.Marshal(reqObj)

	// compression_enabled = true → the tool-result is compressed upstream
	// (the ANSI escapes are stripped) and reqCtx.tokensBefore is carried out.
	r1, body1 := buildExecuteReq(t, string(bodyBytes))
	rc1 := &requestContext{firstMsg: "run it", requestBody: body1, compressionEnabled: true}
	conn1 := pickExecuteConn(t, srv, db, "openai", "primary")
	res1, err := srv.executeRequest(r1.Context(), r1, body1, canonical.FormatOpenAI,
		"openai", "gpt-4o", false, conn1, nil, "req-comp-on", time.Now(), rc1)
	if err != nil {
		t.Fatalf("executeRequest (enabled): %v", err)
	}
	if res1 != nil && res1.Body != nil {
		res1.Body.Close()
	}
	if strings.Contains(capturedBody, "u001b") {
		t.Errorf("compression_enabled: ANSI escapes survived into the upstream body — Compress was not reached\nbody: %s", capturedBody)
	}
	if rc1.tokensBefore <= 0 {
		t.Errorf("compression_enabled: reqCtx.tokensBefore = %d, want > 0 (the AC9 carrier)", rc1.tokensBefore)
	}

	// compression_enabled = false → the tool-result reaches upstream
	// untouched (ANSI escapes intact) and tokensBefore stays 0 (AC9 negative).
	r2, body2 := buildExecuteReq(t, string(bodyBytes))
	rc2 := &requestContext{firstMsg: "run it", requestBody: body2, compressionEnabled: false}
	conn2 := pickExecuteConn(t, srv, db, "openai", "secondary")
	res2, err := srv.executeRequest(r2.Context(), r2, body2, canonical.FormatOpenAI,
		"openai", "gpt-4o", false, conn2, nil, "req-comp-off", time.Now(), rc2)
	if err != nil {
		t.Fatalf("executeRequest (disabled): %v", err)
	}
	if res2 != nil && res2.Body != nil {
		res2.Body.Close()
	}
	if !strings.Contains(capturedBody, "u001b") {
		t.Errorf("compression disabled: the tool-result should have reached upstream uncompressed (ANSI intact)\nbody: %s", capturedBody)
	}
	if rc2.tokensBefore != 0 {
		t.Errorf("compression disabled: reqCtx.tokensBefore = %d, want 0 (AC9 negative case)", rc2.tokensBefore)
	}
}
