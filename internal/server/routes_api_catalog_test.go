package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sage-router/internal/auth"
	"sage-router/internal/bypass"
	"sage-router/internal/catalog"
	"sage-router/internal/config"
	"sage-router/internal/provider"
	"sage-router/internal/ratelimit"
	"sage-router/internal/routing"
	"sage-router/internal/store"
	"sage-router/internal/translate"
	claudeTranslate "sage-router/internal/translate/claude"
	openaiTranslate "sage-router/internal/translate/openai"
	"sage-router/internal/usage"
)

// Models Discovery M2.10 — new `/api/catalog/*` HTTP endpoints (AC20).
//
// Five new JWT-protected handlers:
//   - GET    /api/catalog/models
//   - GET    /api/catalog/models/{provider}
//   - PUT    /api/catalog/pricing/{provider}/{model_id...}
//   - DELETE /api/catalog/pricing/{provider}/{model_id...}
//   - GET    /api/catalog/providers
//
// model_id is a catch-all path segment because OpenRouter qualifies
// IDs as `<vendor>/<model>` (e.g., `anthropic/claude-sonnet-4`).
// Go 1.22's http.ServeMux supports `{name...}` for greedy matching.
//
// User pricing overrides write source='user'; per ADR-2 §Conflict
// resolution + M2.9's precedence matrix, user wins over every other
// source. DELETE removes the row entirely — subsequent discovery /
// OpenRouter refresh re-populates from the upstream.

// newCatalogTestServer is a tighter variant of setupTestServer that
// includes the catalog wiring (Catalog Registry + CatalogStore) so
// the new endpoints have something to read/write.
func newCatalogTestServer(t *testing.T) (*Server, store.Store, catalog.Store) {
	t.Helper()

	db, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cs := catalog.NewSQLiteStore(db.DB())
	if err := catalog.SeedFromConstants(context.Background(), cs); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := catalog.SeedProviderMeta(context.Background(), cs); err != nil {
		t.Fatalf("seed provider meta: %v", err)
	}

	authMgr := auth.NewManager("hash", []byte("jwt"), []byte("hmac"))

	translateReg := translate.NewRegistry()
	translateReg.Register(openaiTranslate.New())
	translateReg.Register(claudeTranslate.New())

	providerReg := provider.NewRegistry()
	for id, p := range config.KnownProviders {
		providerReg.Register(id, provider.ProviderMeta{
			ID: p.ID, Name: p.Name, Format: p.Format, BaseURL: p.BaseURL, AuthTypes: p.AuthTypes,
		})
	}

	srv := New(Config{Host: "127.0.0.1", Port: 0}, Dependencies{
		Store:             db,
		TranslateRegistry: translateReg,
		ProviderSelector:  provider.NewSelector(),
		ProviderRegistry:  providerReg,
		UsageTracker:      usage.NewTracker(db),
		Auth:              authMgr,
		SmartRouter:       routing.NewSmartRouter(),
		ConversationStore: routing.NewConversationStore(),
		BypassFilter:      bypass.NewFilter(),
		RateLimiter:       ratelimit.New(),
		Catalog:           catalog.NewRegistry(cs),
		CatalogStore:      cs,
	})

	return srv, db, cs
}

// catalogTestSessionCookie returns a JWT cookie that the middleware
// accepts. Used by tests that go through the full handler chain.
// Uses a 1-hour expiry rather than 0 to avoid CI clock-drift flakes:
// `GenerateToken(0)` sets Exp = now.Unix(), which the validator
// rejects on any subsequent second. 1 hour gives the test process
// plenty of headroom regardless of runner speed.
func catalogTestSessionCookie(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	token, err := srv.deps.Auth.GenerateToken(time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	return &http.Cookie{Name: "sage-auth", Value: token, Path: "/"}
}

// TestGetCatalogModels_RequiresAuth — AC20. The new endpoint is
// JWT-protected via the same `protect()` wrapper used by every other
// dashboard API route. Without a valid session cookie the request is
// rejected at the middleware layer before any handler runs.
func TestGetCatalogModels_RequiresAuth(t *testing.T) {
	srv, _, _ := newCatalogTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/catalog/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestGetCatalogModels_ListsAllModels — JWT-authenticated GET returns
// the catalog Models with their source badges. Distinct from
// /api/models which filters to active connections only; this is the
// admin view, comprehensive.
func TestGetCatalogModels_ListsAllModels(t *testing.T) {
	srv, _, _ := newCatalogTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/catalog/models", nil)
	req.AddCookie(catalogTestSessionCookie(t, srv))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var models []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("expected at least one seeded model in response")
	}

	// Every row must carry the source badge so the dashboard's color-
	// coding logic has something to switch on. The seed populates
	// source='seed' for every row.
	for _, m := range models {
		src, _ := m["source"].(string)
		if src == "" {
			t.Errorf("model row missing source: %+v", m)
		}
		if m["provider"] == nil || m["model_id"] == nil {
			t.Errorf("model row missing identifying fields: %+v", m)
		}
	}
}

// TestGetCatalogModels_FilteredByProvider — `GET /api/catalog/models/{provider}`
// restricts the response to one provider's rows.
func TestGetCatalogModels_FilteredByProvider(t *testing.T) {
	srv, _, _ := newCatalogTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/catalog/models/anthropic", nil)
	req.AddCookie(catalogTestSessionCookie(t, srv))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var models []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("expected at least one anthropic model in response")
	}
	for _, m := range models {
		if m["provider"] != "anthropic" {
			t.Errorf("model row leaked from other provider: %+v", m)
		}
	}
}

// TestPutCatalogPricing_PersistsOverride — AC20 PUT path. Request
// body has new prices; row is upserted with source='user'. After PUT:
//   - GetPricing returns the new values
//   - Source is exactly 'user' (the user-wins precedence rule applies)
//   - Registry cache is invalidated — subsequent Registry.Pricing reads
//     return the new values (not the cached pre-PUT state)
func TestPutCatalogPricing_PersistsOverride(t *testing.T) {
	srv, _, cs := newCatalogTestServer(t)
	reg := srv.deps.Catalog

	// Warm the Registry cache with the seed values so the post-PUT
	// invalidation contract is observable. Sanity-check pre != target
	// so the test isn't trivially passing (M2.10-review MAJOR-3).
	pre := reg.Pricing("anthropic", "claude-sonnet-4-6")
	if pre == nil {
		t.Fatal("seed pricing for claude-sonnet-4-6 missing — fixture problem")
	}
	if pre.Input == 0.5 {
		t.Fatal("seed Input already 0.5 — test cannot distinguish pre/post-PUT state")
	}

	body := []byte(`{"input_price": 0.5, "output_price": 1.5, "cache_read_price": 0.05, "cache_write_price": 0.625, "thinking_price": 0}`)
	req := httptest.NewRequest(http.MethodPut,
		"/api/catalog/pricing/anthropic/claude-sonnet-4-6", bytes.NewReader(body))
	req.AddCookie(catalogTestSessionCookie(t, srv))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Persisted at the store layer with source='user'.
	got, err := cs.GetPricing(context.Background(), "anthropic", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("GetPricing: %v", err)
	}
	if got == nil {
		t.Fatal("pricing row vanished after PUT")
	}
	if got.Source != catalog.SourceUser {
		t.Errorf("Source = %q, want %q", got.Source, catalog.SourceUser)
	}
	if got.Input != 0.5 || got.Output != 1.5 || got.CacheRead != 0.05 || got.CacheWrite != 0.625 {
		t.Errorf("PUT didn't persist values: got %+v", got)
	}

	// Registry cache invalidated — next Pricing read MUST return the
	// new values, not the cached pre-PUT state.
	post := reg.Pricing("anthropic", "claude-sonnet-4-6")
	if post == nil {
		t.Fatal("Registry.Pricing returned nil after PUT")
	}
	if post.Input != 0.5 {
		t.Errorf("Registry cache NOT invalidated: Input = %g, want 0.5 (saw %g pre-PUT)",
			post.Input, pre.Input)
	}
}

// TestPutCatalogPricing_QualifiedOpenRouterModelID — OpenRouter model
// IDs contain a slash (`vendor/model`). The path `{model_id...}` is a
// catch-all segment per Go 1.22+ http.ServeMux semantics. Verify a
// PUT to `/api/catalog/pricing/openrouter/anthropic/claude-sonnet-4`
// resolves model_id="anthropic/claude-sonnet-4" (not "anthropic").
func TestPutCatalogPricing_QualifiedOpenRouterModelID(t *testing.T) {
	srv, _, cs := newCatalogTestServer(t)

	// Seed the openrouter model row so the FK is satisfied.
	if err := cs.UpsertModel(context.Background(), catalog.Model{
		Provider: "openrouter", ModelID: "anthropic/claude-sonnet-4",
		Source: catalog.SourceOpenRouter,
	}); err != nil {
		t.Fatalf("seed openrouter model: %v", err)
	}

	body := []byte(`{"input_price": 99.0, "output_price": 999.0, "cache_read_price": 0, "cache_write_price": 0, "thinking_price": 0}`)
	req := httptest.NewRequest(http.MethodPut,
		"/api/catalog/pricing/openrouter/anthropic/claude-sonnet-4", bytes.NewReader(body))
	req.AddCookie(catalogTestSessionCookie(t, srv))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	got, err := cs.GetPricing(context.Background(), "openrouter", "anthropic/claude-sonnet-4")
	if err != nil {
		t.Fatalf("GetPricing: %v", err)
	}
	if got == nil {
		t.Fatal("qualified-id pricing row missing — path-segment parser dropped the slash")
	}
	if got.Input != 99.0 {
		t.Errorf("Input = %g, want 99.0 (qualified ID lost?)", got.Input)
	}
}

// TestDeleteCatalogPricing_RemovesOverride — AC20 DELETE path. After
// a user PUT, DELETE removes the override row entirely. Subsequent
// GetPricing returns nil; the next refresh cycle (OpenRouter or
// discovery) re-populates. Registry cache is invalidated.
func TestDeleteCatalogPricing_RemovesOverride(t *testing.T) {
	srv, _, cs := newCatalogTestServer(t)
	reg := srv.deps.Catalog

	// Establish a user override first.
	if err := cs.UpsertPricing(context.Background(), "anthropic", "claude-sonnet-4-6", catalog.Pricing{
		Input: 0.01, Output: 0.05, Source: catalog.SourceUser,
	}); err != nil {
		t.Fatalf("seed user pricing: %v", err)
	}
	// Warm the Registry cache.
	if pre := reg.Pricing("anthropic", "claude-sonnet-4-6"); pre == nil || pre.Input != 0.01 {
		t.Fatalf("Registry cache pre-warm failed: %+v", pre)
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/api/catalog/pricing/anthropic/claude-sonnet-4-6", nil)
	req.AddCookie(catalogTestSessionCookie(t, srv))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 200 or 204; body: %s", rec.Code, rec.Body.String())
	}

	got, err := cs.GetPricing(context.Background(), "anthropic", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("GetPricing: %v", err)
	}
	if got != nil {
		t.Errorf("pricing row still present after DELETE: %+v", got)
	}

	// Registry cache invalidated; next Pricing read goes to the store
	// and observes the deletion. Registry.Pricing returns nil for a
	// missing model, or a zero-value Pricing (Source=="") when the
	// model row exists but its pricing row doesn't — both shapes are
	// post-DELETE truth. The Source=='user' value MUST NOT survive.
	post := reg.Pricing("anthropic", "claude-sonnet-4-6")
	if post != nil && post.Source == catalog.SourceUser {
		t.Errorf("Registry cache NOT invalidated after DELETE: still seeing user pricing %+v", post)
	}
}

// TestDeleteCatalogPricing_IdempotentOnMissing — DELETE for a row
// that doesn't exist returns 200/204 without error. Per ADR contract,
// DELETE is idempotent — the operator doesn't need to know the row
// already vanished from a prior refresh.
func TestDeleteCatalogPricing_IdempotentOnMissing(t *testing.T) {
	srv, _, _ := newCatalogTestServer(t)

	req := httptest.NewRequest(http.MethodDelete,
		"/api/catalog/pricing/anthropic/nonexistent-model", nil)
	req.AddCookie(catalogTestSessionCookie(t, srv))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Errorf("idempotent DELETE on missing row: status = %d, want 200 or 204; body: %s",
			rec.Code, rec.Body.String())
	}
}

// TestPutCatalogPricing_RejectsPartialBody — PUT is strictly
// replace-not-patch. A body missing any of the five price fields is
// rejected with 400. Prevents the silent-zeroing footgun where a
// caller sending `{"input_price": 5}` expecting "update only input_price"
// would zero output_price, cache_read_price, etc. under
// unconditional-replace semantics. (M2.10-review MAJOR-2.)
func TestPutCatalogPricing_RejectsPartialBody(t *testing.T) {
	srv, _, cs := newCatalogTestServer(t)

	// Seed the row first with realistic values so we can verify
	// nothing gets overwritten on the rejected PUT.
	if err := cs.UpsertPricing(context.Background(), "anthropic", "claude-sonnet-4-6", catalog.Pricing{
		Input: 3.0, Output: 15.0, CacheRead: 0.3, CacheWrite: 3.75, Thinking: 15.0,
		Source: catalog.SourceSeed,
	}); err != nil {
		t.Fatalf("seed pricing: %v", err)
	}

	// Partial body — only input_price set. All other fields nil.
	body := []byte(`{"input_price": 5.0}`)
	req := httptest.NewRequest(http.MethodPut,
		"/api/catalog/pricing/anthropic/claude-sonnet-4-6", bytes.NewReader(body))
	req.AddCookie(catalogTestSessionCookie(t, srv))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("partial PUT: status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	// Error message names the missing fields so the caller can fix
	// their payload.
	if !strings.Contains(rec.Body.String(), "output_price") {
		t.Errorf("400 body should name missing field 'output_price'; got: %s", rec.Body.String())
	}

	// Critically: no writes happened. The seed row's Output is
	// preserved (not zeroed by the rejected PUT).
	got, err := cs.GetPricing(context.Background(), "anthropic", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("GetPricing: %v", err)
	}
	if got == nil || got.Output != 15.0 {
		t.Errorf("rejected PUT must not corrupt existing row: got %+v, want Output=15.0", got)
	}
}

// TestPutCatalogPricing_AutoCreatedModelRowUsesSeedSource — operator
// PUTs pricing for a model the catalog has never seen. The handler
// must create the catalog_models row at `source='seed'`, NOT
// `source='user'`. If it were `source='user'`, the model precedence
// matrix (M2.9) would lock the row against subsequent discovery
// writes — the model would be stuck with zero capabilities forever.
// `source='seed'` loses to discovery, openrouter, and user, so a
// future discovery refresh can populate display_name, tier, etc.
// (M2.10-review MAJOR-1.)
func TestPutCatalogPricing_AutoCreatedModelRowUsesSeedSource(t *testing.T) {
	srv, _, cs := newCatalogTestServer(t)

	// Use a deliberately-unknown model ID so we exercise the auto-
	// create branch in the handler.
	body := []byte(`{"input_price": 1, "output_price": 5, "cache_read_price": 0.1, "cache_write_price": 1.25, "thinking_price": 0}`)
	req := httptest.NewRequest(http.MethodPut,
		"/api/catalog/pricing/anthropic/claude-unicorn-5-future", bytes.NewReader(body))
	req.AddCookie(catalogTestSessionCookie(t, srv))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	m, err := cs.GetModel(context.Background(), "anthropic", "claude-unicorn-5-future")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if m == nil {
		t.Fatal("auto-created model row missing after PUT")
	}
	if m.Source != catalog.SourceSeed {
		t.Errorf("auto-created model Source = %q, want %q (must be overwritable by discovery)",
			m.Source, catalog.SourceSeed)
	}
	// Pricing IS at SourceUser — only the model row uses seed.
	if m.Pricing.Source != catalog.SourceUser {
		t.Errorf("pricing Source = %q, want %q (user override should persist)",
			m.Pricing.Source, catalog.SourceUser)
	}

	// Discovery can still overwrite the model row (the load-bearing
	// contract — without this fix, an operator who pre-prices a new
	// model would permanently lock out discovery for it).
	if err := cs.UpsertModel(context.Background(), catalog.Model{
		Provider:    "anthropic",
		ModelID:     "claude-unicorn-5-future",
		DisplayName: "Claude Unicorn 5 (discovered)",
		Tier:        catalog.TierFrontier,
		Source:      catalog.SourceDiscovery,
	}); err != nil {
		t.Fatalf("post-PUT discovery upsert: %v", err)
	}
	m2, _ := cs.GetModel(context.Background(), "anthropic", "claude-unicorn-5-future")
	if m2.DisplayName != "Claude Unicorn 5 (discovered)" {
		t.Errorf("discovery couldn't overwrite seed-sourced model row: DisplayName = %q",
			m2.DisplayName)
	}
	// AND user pricing survives the discovery overwrite (pricing
	// precedence: user > discovery).
	if m2.Pricing.Source != catalog.SourceUser {
		t.Errorf("user pricing didn't survive discovery overwrite: Source = %q", m2.Pricing.Source)
	}
}

// TestPutCatalogPricing_RejectsInvalidValues — spec §6 / decision-log
// RM5 contract (M2.13 Gate 3 review MAJOR-1): negative prices and
// absurdly-high prices are rejected with 400 before reaching the
// catalog. Each rejected PUT also leaves any pre-existing pricing
// row intact.
//
// NaN and Inf are guarded in the handler too, but Go's stdlib JSON
// decoder rejects those literals at parse time (the JSON spec
// disallows them), so the IsNaN/IsInf branches are defense-in-depth
// for non-stdlib decoder paths. The wire-level tests below cover the
// branches the JSON decoder actually delivers.
func TestPutCatalogPricing_RejectsInvalidValues(t *testing.T) {
	srv, _, cs := newCatalogTestServer(t)

	// Seed a baseline so we can verify the rejected PUT didn't write.
	if err := cs.UpsertPricing(context.Background(), "anthropic", "claude-sonnet-4-6", catalog.Pricing{
		Input: 3.0, Output: 15.0, Source: catalog.SourceSeed,
	}); err != nil {
		t.Fatalf("seed pricing: %v", err)
	}

	cases := []struct {
		name   string
		body   string
		expect string // substring of the 400 body
	}{
		{
			name:   "Negative",
			body:   `{"input_price": -1.0, "output_price": 5, "cache_read_price": 0, "cache_write_price": 0, "thinking_price": 0}`,
			expect: "non-negative",
		},
		{
			name:   "AboveSanityCap",
			body:   `{"input_price": 999999, "output_price": 5, "cache_read_price": 0, "cache_write_price": 0, "thinking_price": 0}`,
			expect: "sanity cap",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut,
				"/api/catalog/pricing/anthropic/claude-sonnet-4-6",
				bytes.NewReader([]byte(c.body)))
			req.AddCookie(catalogTestSessionCookie(t, srv))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %s; body: %s",
					rec.Code, c.name, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.expect) {
				t.Errorf("400 body should mention %q; got: %s", c.expect, rec.Body.String())
			}

			// Seed row untouched — rejected PUT must not corrupt.
			got, err := cs.GetPricing(context.Background(), "anthropic", "claude-sonnet-4-6")
			if err != nil {
				t.Fatalf("GetPricing: %v", err)
			}
			if got == nil || got.Input != 3.0 || got.Source != catalog.SourceSeed {
				t.Errorf("rejected %s PUT corrupted seed row: %+v", c.name, got)
			}
		})
	}
}

// TestCatalogEndpoints_RequireAuth — table-driven coverage that EVERY
// new /api/catalog/* route is wrapped in the auth middleware. Catches
// the copy-paste-forgot-protect regression class. (M2.10-review
// MINOR-1.)
func TestCatalogEndpoints_RequireAuth(t *testing.T) {
	srv, _, _ := newCatalogTestServer(t)

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/catalog/models"},
		{http.MethodGet, "/api/catalog/models/anthropic"},
		{http.MethodPut, "/api/catalog/pricing/anthropic/some-model"},
		{http.MethodDelete, "/api/catalog/pricing/anthropic/some-model"},
		{http.MethodGet, "/api/catalog/providers"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			var body []byte
			if c.method == http.MethodPut {
				body = []byte(`{"input_price":0,"output_price":0,"cache_read_price":0,"cache_write_price":0,"thinking_price":0}`)
			}
			req := httptest.NewRequest(c.method, c.path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("unauthenticated %s %s: status = %d, want 401", c.method, c.path, rec.Code)
			}
		})
	}
}

// TestGetCatalogProviders_ListsProviderMetas — `/api/catalog/providers`
// returns the catalog_provider_meta rows so the dashboard can show
// discovery-loop state (last_discovered_at, last_discovery_error,
// backoff_step) per provider. Distinct from `/api/providers` which
// dumps the static `KnownProviders` map.
func TestGetCatalogProviders_ListsProviderMetas(t *testing.T) {
	srv, _, _ := newCatalogTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/catalog/providers", nil)
	req.AddCookie(catalogTestSessionCookie(t, srv))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var metas []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &metas); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Subset-presence assertion (M2.10-review MINOR-7): rather than
	// `len(metas) ≥ 6` which breaks if the seed shrinks for an
	// unrelated reason, assert that two well-known providers are
	// present. Catches the "providers endpoint serves empty list" bug
	// without coupling to seed cardinality.
	seenByProvider := map[string]map[string]any{}
	for _, m := range metas {
		p, _ := m["provider"].(string)
		if p == "" {
			t.Errorf("meta row missing provider: %+v", m)
			continue
		}
		seenByProvider[p] = m
	}
	for _, want := range []string{"openai", "anthropic"} {
		row, ok := seenByProvider[want]
		if !ok {
			t.Errorf("expected provider %q in response; got %v", want, seenByProvider)
			continue
		}
		// discovery_enabled is the load-bearing flag the dashboard
		// needs; the seed values are tested in M1's seed_test.go.
		if _, ok := row["discovery_enabled"]; !ok {
			t.Errorf("meta row for %q missing discovery_enabled: %+v", want, row)
		}
	}
}
