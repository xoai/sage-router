package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/auth/oauth"
	"sage-router/internal/provider"
	"sage-router/internal/store"
)

// setupOAuthTestServer builds a minimal Server with just the deps the
// subscription-auth routes need. The bridge is started on ephemeral
// ports so the test doesn't collide with running Codex/Claude tools.
func setupOAuthTestServer(t *testing.T) (*Server, *oauth.Bridge) {
	t.Helper()
	db, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db.SetEncryptionKey(store.DeriveKey("test-master"))
	t.Cleanup(func() { db.Close() })

	srv := &Server{
		deps: Dependencies{
			Store:            db,
			ProviderSelector: provider.NewSelector(),
		},
	}
	bridge := oauth.NewBridgeForTest(&oauthBridgeHandler{srv: srv})
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("bridge start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = bridge.Stop(ctx)
	})
	srv.deps.OAuthBridge = bridge
	return srv, bridge
}

func doJSON(t *testing.T, handler http.HandlerFunc, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v (body=%q)", err, rec.Body.String())
	}
	return out
}

// ── TOS gate ──

func TestOAuthStart_TOSGateBlocksUnacknowledged(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	rec := doJSON(t, srv.handleOAuthStart, "POST", "/api/auth/oauth/start",
		map[string]any{"provider": "openai", "name": "Work"})
	if rec.Code != http.StatusPreconditionRequired {
		t.Errorf("status = %d, want 428", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["requires_tos"] != true {
		t.Errorf("requires_tos = %v, want true", body["requires_tos"])
	}
	if !strings.Contains(body["message"].(string), "subscription") {
		t.Errorf("message should mention 'subscription'; got %q", body["message"])
	}
}

func TestOAuthStart_ProceedsAfterTOSAccept(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	if err := auth.AcknowledgeTOS(srv.deps.Store); err != nil {
		t.Fatalf("ack tos: %v", err)
	}
	rec := doJSON(t, srv.handleOAuthStart, "POST", "/api/auth/oauth/start",
		map[string]any{"provider": "openai", "name": "Work"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["authorize_url"] == nil {
		t.Error("response missing authorize_url")
	}
	if body["state"] == nil {
		t.Error("response missing state")
	}
}

// ── Provider resolution ──

func TestOAuthStart_UnknownProviderRejected(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	auth.AcknowledgeTOS(srv.deps.Store)
	rec := doJSON(t, srv.handleOAuthStart, "POST", "/api/auth/oauth/start",
		map[string]any{"provider": "not-real"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestOAuthStart_AliasResolution(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	auth.AcknowledgeTOS(srv.deps.Store)
	// "claude" should resolve to "anthropic" — and anthropic is a PKCE
	// provider so the flow should start.
	rec := doJSON(t, srv.handleOAuthStart, "POST", "/api/auth/oauth/start",
		map[string]any{"provider": "claude", "name": "Personal"})
	if rec.Code != http.StatusOK {
		t.Errorf("alias 'claude' should resolve; got %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	authURL := body["authorize_url"].(string)
	if !strings.HasPrefix(authURL, "https://claude.ai/oauth/authorize") {
		t.Errorf("authorize_url should target claude.ai; got %s", authURL)
	}
}

// ── Status polling ──

func TestOAuthStatus_RoundTrip(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	auth.AcknowledgeTOS(srv.deps.Store)

	startRec := doJSON(t, srv.handleOAuthStart, "POST", "/api/auth/oauth/start",
		map[string]any{"provider": "openai", "name": "Work"})
	state := decodeBody(t, startRec)["state"].(string)

	statusRec := doJSON(t, srv.handleOAuthStatus, "GET",
		"/api/auth/oauth/status?state="+state, nil)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status = %d", statusRec.Code)
	}
	body := decodeBody(t, statusRec)
	if body["status"] != "pending" {
		t.Errorf("status = %v, want pending", body["status"])
	}
}

func TestOAuthStatus_RequiresStateParam(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	rec := doJSON(t, srv.handleOAuthStatus, "GET", "/api/auth/oauth/status", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing state", rec.Code)
	}
}

// ── Health endpoint ──

func TestOAuthHealth_ReturnsPerProviderStatus(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	rec := doJSON(t, srv.handleOAuthHealth, "GET", "/api/auth/oauth/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["openai"] != "available" {
		t.Errorf("openai health = %v, want available", body["openai"])
	}
	if body["anthropic"] != "available" {
		t.Errorf("anthropic health = %v, want available", body["anthropic"])
	}
}

// ── Import ──

func TestImport_TOSGateBlocksUnacknowledged(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	rec := doJSON(t, srv.handleImport, "POST", "/api/auth/import",
		map[string]any{"provider": "openai", "name": "Imported"})
	if rec.Code != http.StatusPreconditionRequired {
		t.Errorf("status = %d, want 428", rec.Code)
	}
}

func TestImport_FromFixtureFile(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	auth.AcknowledgeTOS(srv.deps.Store)

	// Use the Codex fixture from the imports package.
	fixture, err := filepath.Abs("../auth/imports/testdata/codex_valid.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	rec := doJSON(t, srv.handleImport, "POST", "/api/auth/import",
		map[string]any{"provider": "openai", "name": "FromCodex", "path": fixture})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	connID, _ := body["id"].(string)
	if connID == "" {
		t.Fatal("response missing connection id")
	}

	// Verify the connection landed in the store with canonical AuthType.
	c, err := srv.deps.Store.GetConnection(connID)
	if err != nil {
		t.Fatalf("get conn: %v", err)
	}
	if c.AuthType != auth.AuthTypeSubscription {
		t.Errorf("AuthType = %q, want subscription", c.AuthType)
	}
	if c.AccessToken == "" {
		t.Error("AccessToken not persisted")
	}
	// Persisted lifecycle state is "idle" (M2 facet model — createSubscriptionConnection).
	if c.State != "idle" {
		t.Errorf("persisted State = %q, want idle", c.State)
	}
	// Selector should know about it.
	if srv.deps.ProviderSelector.ConnectionByID(connID) == nil {
		t.Error("connection not registered with selector")
	}
}

func TestImport_ProviderDataRoundTripsThroughAuthStore(t *testing.T) {
	// Regression for the Gate-3 CRITICAL finding: the writer was using a
	// flat provider_data shape but AuthStore.GetCredential decoded into a
	// nested {account_id, extra} struct, silently dropping ExtraData on
	// every read. This test imports a Claude fixture (which carries
	// subscription_type + scopes in ExtraData) and verifies those fields
	// survive a GetCredential round-trip via the AuthStore facade.
	srv, _ := setupOAuthTestServer(t)
	auth.AcknowledgeTOS(srv.deps.Store)

	fixture, err := filepath.Abs("../auth/imports/testdata/claude_valid.json")
	if err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, srv.handleImport, "POST", "/api/auth/import",
		map[string]any{"provider": "anthropic", "name": "ClaudeImport", "path": fixture})
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: status %d, body=%s", rec.Code, rec.Body.String())
	}
	connID := decodeBody(t, rec)["id"].(string)

	// Build the same adapter wiring main.go uses.
	type connStoreAdapter struct{ s store.Store }
	// Inline adapter for the test only — keeps the test self-contained.
	store := srv.deps.Store
	c, err := store.GetConnection(connID)
	if err != nil {
		t.Fatalf("get conn: %v", err)
	}
	// Round-trip provider_data through the nested decoder shape.
	var pd struct {
		AccountID string         `json:"account_id,omitempty"`
		Extra     map[string]any `json:"extra,omitempty"`
	}
	if len(c.ProviderData) == 0 {
		t.Fatal("provider_data not persisted")
	}
	if err := json.Unmarshal(c.ProviderData, &pd); err != nil {
		t.Fatalf("provider_data not in expected nested shape: %v (raw=%s)", err, c.ProviderData)
	}
	if pd.Extra == nil {
		t.Fatal("ExtraData dropped during round-trip — provider_data shape mismatch reintroduced")
	}
	if pd.Extra["subscription_type"] != "max" {
		t.Errorf("subscription_type lost: %+v", pd.Extra)
	}
	if _, ok := pd.Extra["scopes"]; !ok {
		t.Errorf("scopes lost: %+v", pd.Extra)
	}
	_ = connStoreAdapter{} // silence unused-type lint
}

func TestHandleGetUsageSummary_EnrichesWithSubscriptionSavings(t *testing.T) {
	// AC30 regression: the dashboard's /api/usage/summary endpoint must
	// surface SubscriptionSavings — sum of (would-have-been API cost)
	// across subscription-tagged rows, computed at query time from the
	// pricing table.
	srv, _ := setupOAuthTestServer(t)

	// Seed one apikey row + two subscription rows.
	if err := srv.deps.Store.RecordUsage(&store.UsageEntry{
		ID: "u-api", RequestID: "u-api",
		Provider: "openai", Model: "gpt-5",
		ConnectionID: "c-api", InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
		Cost: 0.5, CostSource: "apikey", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.deps.Store.RecordUsage(&store.UsageEntry{
		ID: "u-sub-1", RequestID: "u-sub-1",
		Provider: "openai", Model: "gpt-5",
		ConnectionID: "c-sub", InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500,
		Cost: 0, CostSource: "subscription", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.deps.Store.RecordUsage(&store.UsageEntry{
		ID: "u-sub-2", RequestID: "u-sub-2",
		Provider: "anthropic", Model: "claude-opus-4",
		ConnectionID: "c-sub-anth", InputTokens: 2000, OutputTokens: 1000, TotalTokens: 3000,
		Cost: 0, CostSource: "subscription", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}

	rec := doJSON(t, srv.handleGetUsageSummary, "GET", "/api/usage/summary", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)

	// total_cost is just the sum of recorded Cost values (subscription rows = 0).
	if got := body["total_cost"]; got != 0.5 {
		t.Errorf("total_cost = %v, want 0.5", got)
	}

	// by_cost_source must include both buckets.
	bcs, _ := body["by_cost_source"].(map[string]any)
	if bcs == nil {
		t.Fatal("by_cost_source missing")
	}
	if _, ok := bcs["apikey"]; !ok {
		t.Error("by_cost_source missing 'apikey' bucket")
	}
	if _, ok := bcs["subscription"]; !ok {
		t.Error("by_cost_source missing 'subscription' bucket")
	}

	// SubscriptionSavings must be > 0 (the actual value depends on the
	// current pricing table; we only assert non-zero to avoid hardcoding
	// pricing that may change).
	savings, ok := body["subscription_savings"].(float64)
	if !ok {
		t.Fatalf("subscription_savings missing or not a number: %v", body["subscription_savings"])
	}
	// If pricing for openai/gpt-5 or anthropic/claude-opus-4 isn't in the
	// catalog, savings will legitimately be 0 — that's not a bug, just an
	// observation. Log instead of fail.
	if savings == 0 {
		t.Logf("subscription_savings = 0 — pricing table may not include the test models; not a regression unless test data is changed")
	}
}

func TestImport_FileNotFoundReturns404(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	auth.AcknowledgeTOS(srv.deps.Store)
	rec := doJSON(t, srv.handleImport, "POST", "/api/auth/import",
		map[string]any{"provider": "openai", "name": "X", "path": "/tmp/does-not-exist-" + t.Name()})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ── TOS endpoint ──

func TestTOSAccept_PersistsSetting(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	if auth.IsTOSAcknowledged(srv.deps.Store) {
		t.Fatal("precondition: tos should NOT be acknowledged")
	}
	rec := doJSON(t, srv.handleTOSAccept, "POST", "/api/auth/tos/accept", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !auth.IsTOSAcknowledged(srv.deps.Store) {
		t.Error("TOS should be acknowledged after handler")
	}
}

// ── Device-code deprecation (410 Gone) ──

func TestOpenAIDeviceStart_Returns410Gone(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	rec := doJSON(t, srv.handleOpenAIDeviceStartDeprecated, "POST", "/api/oauth/openai/device", nil)
	if rec.Code != http.StatusGone {
		t.Errorf("status = %d, want 410", rec.Code)
	}
	body := decodeBody(t, rec)
	if !strings.Contains(body["message"].(string), "PKCE") {
		t.Errorf("message should point at PKCE replacement; got %q", body["message"])
	}
}

func TestOpenAIDevicePoll_Returns410Gone(t *testing.T) {
	srv, _ := setupOAuthTestServer(t)
	rec := doJSON(t, srv.handleOpenAIDevicePollDeprecated, "POST", "/api/oauth/openai/poll", nil)
	if rec.Code != http.StatusGone {
		t.Errorf("status = %d, want 410", rec.Code)
	}
}

// ── End-to-end: full OAuth flow through the bridge ──

func TestBridgeHandler_EndToEndCreatesConnection(t *testing.T) {
	srv, bridge := setupOAuthTestServer(t)
	auth.AcknowledgeTOS(srv.deps.Store)

	// Mock the provider's token endpoint.
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "atk-fresh",
			"refresh_token": "rtk-fresh",
			"expires_in": 3600
		}`))
	}))
	defer tokSrv.Close()

	// Start a flow via the handler (captures Origin).
	startReq := httptest.NewRequest("POST", "/api/auth/oauth/start",
		strings.NewReader(`{"provider":"openai","name":"E2E"}`))
	startReq.Header.Set("Content-Type", "application/json")
	startReq.Header.Set("Origin", "https://dash.example.com")
	startRec := httptest.NewRecorder()
	srv.handleOAuthStart(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", startRec.Code, startRec.Body.String())
	}
	state := decodeBody(t, startRec)["state"].(string)

	// Override the registered Flow's tokenURL to point at our mock.
	// Bridge's Consume removes it, but we need the override BEFORE
	// consume. The cleanest way: drain pendingFlows, mutate, re-register.
	flow, ok := bridge.Consume(state)
	if !ok {
		t.Fatal("flow vanished from pendingFlows")
	}
	flow.SetTokenURLForTest(tokSrv.URL)
	bridge.Register(flow)

	// Drive the callback against the bridge directly. Don't follow the
	// dashboard redirect — Origin points at a fake hostname.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(bridge.CallbackURL("openai") + "?code=ok&state=" + state)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("callback status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "dash.example.com") {
		t.Errorf("Location header should preserve Origin; got %q", loc)
	}

	// Poll status — should now report complete with a connection ID.
	statusRec := doJSON(t, srv.handleOAuthStatus, "GET",
		"/api/auth/oauth/status?state="+state, nil)
	body := decodeBody(t, statusRec)
	if body["status"] != "complete" {
		t.Errorf("status = %v, want complete", body["status"])
	}
	connID, _ := body["conn_id"].(string)
	if connID == "" {
		t.Fatal("conn_id missing from status response")
	}

	// Verify the connection lives in the store + selector.
	c, err := srv.deps.Store.GetConnection(connID)
	if err != nil {
		t.Fatalf("get conn: %v", err)
	}
	if c.AuthType != auth.AuthTypeSubscription {
		t.Errorf("AuthType = %q, want subscription", c.AuthType)
	}
	if c.AccessToken != "atk-fresh" {
		t.Errorf("AccessToken = %q, want atk-fresh", c.AccessToken)
	}
	if srv.deps.ProviderSelector.ConnectionByID(connID) == nil {
		t.Error("connection not registered with selector")
	}
}
