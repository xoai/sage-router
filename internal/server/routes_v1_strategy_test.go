package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"sage-router/internal/executor"
	"sage-router/internal/provider"
	"sage-router/internal/routing"
	"sage-router/internal/store"
)

// TestConnStrategyFor (M3 T7 — AC9 plumbing half) pins the routing.Strategy →
// provider.SelectStrategy mapping for all seven routing strategies plus an
// unrecognized value: only the two connection-selection strategies carry a
// non-default mapping.
func TestConnStrategyFor(t *testing.T) {
	tests := []struct {
		strategy routing.Strategy
		want     provider.SelectStrategy
	}{
		{routing.StrategyBalanced, provider.SelectDefault},
		{routing.StrategyFast, provider.SelectDefault},
		{routing.StrategyCheap, provider.SelectDefault},
		{routing.StrategyBest, provider.SelectDefault},
		{routing.StrategyUserOrder, provider.SelectDefault},
		{routing.StrategyP2C, provider.SelectP2C},
		{routing.StrategyResetAware, provider.SelectResetAware},
		{routing.Strategy("__unrecognized__"), provider.SelectDefault},
	}
	for _, tt := range tests {
		if got := connStrategyFor(tt.strategy); got != tt.want {
			t.Errorf("connStrategyFor(%q) = %d, want %d", tt.strategy, got, tt.want)
		}
	}
}

// addStrategyConn registers an apikey connection with a caller-chosen priority
// and API key so a test can distinguish which connection actually served a
// request (the mock executor reads req.Credentials.APIKey). Returns the
// in-memory provider.Connection so the test can drive its usage counters.
func addStrategyConn(t *testing.T, srv *Server, db store.Store, providerID, id, apiKey string, priority int) *provider.Connection {
	t.Helper()
	conn := &store.Connection{
		ID:       id,
		Provider: providerID,
		Name:     id,
		AuthType: "apikey",
		APIKey:   apiKey,
		Priority: priority,
		State:    "idle",
	}
	if err := db.CreateConnection(conn); err != nil {
		t.Fatalf("CreateConnection %s: %v", id, err)
	}
	pc := provider.NewConnection(conn.ID, conn.Provider, conn.Name, conn.Priority, conn.AuthType)
	srv.deps.ProviderSelector.Register(pc)
	return pc
}

// recordingClaudeExecutor returns a mock anthropic executor that records the
// API key of the connection it served, then returns a canned 200.
func recordingClaudeExecutor(servedKey *string) *mockExecutor {
	return &mockExecutor{
		providerID: "anthropic",
		handler: func(req *executor.ExecuteRequest) (*executor.Result, error) {
			if req.Credentials != nil {
				*servedKey = req.Credentials.APIKey
			}
			resp := `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
			return &executor.Result{
				StatusCode: 200,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(resp)),
				Latency:    time.Millisecond,
			}, nil
		},
	}
}

// TestE2E_AutoP2C_RoutesViaP2C (M3 T8 — AC9 e2e half) is the proof the
// connection-selection strategy threads end-to-end: an auto:p2c HTTP request
// flows resolveModel → handleComboRequest → selectConnection → Select with
// SelectP2C. Two anthropic connections are arranged so p2c's pick and
// SelectDefault's pick diverge — observing the p2c pick proves every plumbing
// touch point is wired; a missed one would leave SelectDefault in effect.
func TestE2E_AutoP2C_RoutesViaP2C(t *testing.T) {
	var servedKey string
	srv, db := setupTestServer(t, map[string]executor.Executor{
		"anthropic": recordingClaudeExecutor(&servedKey),
		"default":   &sentinelExecutor{t: t, providerID: "default"},
	})
	// auto:* smart routing needs a populated catalog; setupTestServer wires none.
	srv.deps.Catalog = buildSmartCandidatesCatalog(t, db)

	// connA: best (lowest) priority — SelectDefault's pick — and recently used.
	// connB: worse priority but never used. p2c orders the two sampled
	// candidates least-recently-used first, so it picks connB. With exactly two
	// candidates p2c always samples both, so the outcome is deterministic and
	// does not depend on the RNG seed.
	connA := addStrategyConn(t, srv, db, "anthropic", "connA", "cred-A", 0)
	addStrategyConn(t, srv, db, "anthropic", "connB", "cred-B", 5)
	if err := connA.MarkUsed(); err != nil {
		t.Fatalf("connA MarkUsed: %v", err)
	}
	if err := connA.MarkSuccess(); err != nil { // back to Idle, LastUsedAt now recent
		t.Fatalf("connA MarkSuccess: %v", err)
	}

	w := doRequest(t, srv, "POST", "/v1/chat/completions", map[string]any{
		"model":    "auto:p2c",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("auto:p2c request: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if servedKey != "cred-B" {
		t.Errorf("auto:p2c served the connection with key %q, want cred-B — p2c "+
			"picks the least-recently-used connection; cred-A means the strategy "+
			"never reached Select and a plumbing touch point is unwired", servedKey)
	}
}
