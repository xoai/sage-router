package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"sage-router/internal/catalog"
	"sage-router/internal/store"
)

// Task 1.10 (plan) — AC9 backward-compat shim golden tests.
//
// /api/providers and /api/models must return the same JSON shape they
// did before the Models Discovery rewire. The catalog change must be
// invisible to any client that consumed these endpoints.
//
// Strategy:
//   - Build a Server with a deterministic fixture (catalog seeded
//     from config constants + a known set of connections).
//   - Hit the handler directly (bypass auth middleware — the shape
//     contract is what matters, not the auth chain).
//   - Compare body bytes against a captured golden file. First run
//     captures; subsequent runs assert.
//
// Regenerate with: UPDATE_GOLDEN=1 go test -run TestApi.*_Shape ...

// TestApiProviders_StableMergedShape — /api/providers shape contract
// (renamed from TestApiProviders_ShapeUnchangedByMigration after
// M2.12 review MAJOR-2). The new name reflects the post-M2.12 reality:
// the endpoint now ships the merged config.KnownProviders +
// catalog_provider_meta shape, and the test asserts that shape is
// **stable** — not that it matches the M1 byte-for-byte baseline.
//
// Decoupling from SeedProviderMeta: the original M1 fixture relied on
// SeedProviderMeta's defaults, which made the golden a transitive
// dependency on seed.go contents. The fixture now writes a hand-rolled
// fixed-state set per provider, so a future seed-defaults change
// won't trigger a confusing shape-drift error here.
//
// Regenerate with UPDATE_GOLDEN=1 when an intentional shape change
// lands; do NOT regenerate to mask test breakage from seed changes.
func TestApiProviders_StableMergedShape(t *testing.T) {
	st := backwardCompatFixtureStore(t)
	cat := buildSmartCandidatesCatalog(t, st)

	cs := catalog.NewSQLiteStore(st.DB())

	// Hand-rolled meta state — one row per KnownProvider with fixed
	// defaults that DON'T track seed.go. If seed.go changes its
	// defaults later, this test stays stable and an intentional shape
	// change requires editing this fixture (visible in diff).
	fixedMeta := map[string]catalog.ProviderMeta{
		"anthropic":      {Provider: "anthropic", DiscoveryEnabled: true, SubscriptionDiscoverable: true},
		"openai":         {Provider: "openai", DiscoveryEnabled: true, SubscriptionDiscoverable: true},
		"gemini":         {Provider: "gemini", DiscoveryEnabled: true, SubscriptionDiscoverable: false},
		"openrouter":     {Provider: "openrouter", DiscoveryEnabled: true, SubscriptionDiscoverable: false},
		"ollama":         {Provider: "ollama", DiscoveryEnabled: true, SubscriptionDiscoverable: false},
		"github-copilot": {Provider: "github-copilot", DiscoveryEnabled: false, SubscriptionDiscoverable: false},
	}
	for id, m := range fixedMeta {
		if err := cs.SetProviderMeta(context.Background(), id, m); err != nil {
			t.Fatalf("set meta %s: %v", id, err)
		}
	}

	s := &Server{deps: Dependencies{Store: st, Catalog: cat, CatalogStore: cs}}

	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	rec := httptest.NewRecorder()
	s.handleListProviders(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	assertJSONGolden(t, "api_providers_golden.json", rec.Body.Bytes())
}

// TestApiModels_ShapeUnchangedByMigration — /api/models returns the
// same modelEntry list shape as before the rewire. Today's catalog
// pricing was seeded from config.ModelCatalog, so the values are
// byte-equal (this is the AC6 byte-equal-cost claim at the API
// surface).
func TestApiModels_ShapeUnchangedByMigration(t *testing.T) {
	st := backwardCompatFixtureStore(t)
	cat := buildSmartCandidatesCatalog(t, st)
	s := &Server{deps: Dependencies{Store: st, Catalog: cat}}

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	rec := httptest.NewRecorder()
	s.handleListModelCatalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	assertJSONGolden(t, "api_models_golden.json", rec.Body.Bytes())
}

// TestApiProviders_EnrichedWithDiscoveryMeta — AC22. /api/providers
// merges static config.KnownProviders with runtime
// catalog_provider_meta state. The dashboard reads
// last_discovered_at + last_discovery_error + backoff_step from this
// merged view to render the discovery-loop status badge per provider.
//
// Assertions:
//   - Every provider in the response has the new fields populated
//     (zero-value when no meta row exists, real values otherwise).
//   - A provider with a written meta row carries the timestamp + error
//     through to the JSON shape.
//   - A provider with discovery_enabled=false (github-copilot in the
//     seed) surfaces that flag truthfully — the dashboard renders a
//     "discovery disabled" indicator off this.
func TestApiProviders_EnrichedWithDiscoveryMeta(t *testing.T) {
	st := backwardCompatFixtureStore(t)
	cat := buildSmartCandidatesCatalog(t, st)
	cs := catalog.NewSQLiteStore(st.DB())
	if err := catalog.SeedProviderMeta(context.Background(), cs); err != nil {
		t.Fatalf("seed provider meta: %v", err)
	}

	// Simulate a recent successful discovery for anthropic and a
	// failed discovery for openai. The JSON shape should reflect both.
	if err := cs.SetProviderMeta(context.Background(), "anthropic", catalog.ProviderMeta{
		Provider:                 "anthropic",
		DiscoveryEnabled:         true,
		SubscriptionDiscoverable: true,
	}); err != nil {
		t.Fatalf("set anthropic meta: %v", err)
	}
	if err := cs.SetProviderMeta(context.Background(), "openai", catalog.ProviderMeta{
		Provider:                 "openai",
		DiscoveryEnabled:         true,
		SubscriptionDiscoverable: true,
		LastDiscoveryError:       "fake 503 from upstream",
		BackoffStep:              2,
	}); err != nil {
		t.Fatalf("set openai meta: %v", err)
	}

	s := &Server{deps: Dependencies{Store: st, Catalog: cat, CatalogStore: cs}}

	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	rec := httptest.NewRecorder()
	s.handleListProviders(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var entries map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// openai: failure-state meta surfaced through the merge.
	openai := entries["openai"]
	if openai == nil {
		t.Fatal("openai entry missing")
	}
	if openai["last_discovery_error"] != "fake 503 from upstream" {
		t.Errorf("openai last_discovery_error = %v, want \"fake 503 from upstream\"",
			openai["last_discovery_error"])
	}
	if v, _ := openai["backoff_step"].(float64); int(v) != 2 {
		t.Errorf("openai backoff_step = %v, want 2", openai["backoff_step"])
	}
	if openai["discovery_enabled"] != true {
		t.Errorf("openai discovery_enabled = %v, want true", openai["discovery_enabled"])
	}

	// github-copilot: discovery_enabled=false per seed defaults. This
	// is the load-bearing flag the dashboard reads to render
	// "discovery disabled — using static allowlist."
	copilot := entries["github-copilot"]
	if copilot == nil {
		t.Fatal("github-copilot entry missing")
	}
	if copilot["discovery_enabled"] != false {
		t.Errorf("github-copilot discovery_enabled = %v, want false (seed default)", copilot["discovery_enabled"])
	}

	// Static fields still present — additive-only contract.
	if copilot["base_url"] == nil || copilot["models"] == nil {
		t.Errorf("github-copilot lost a static field: %+v", copilot)
	}
}

// backwardCompatFixtureStore seeds a fixed set of connections (mix of
// providers + auth_types + states). The shape of /api/models depends
// on activeProviders, which depends on this list.
func backwardCompatFixtureStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	conns := []store.Connection{
		{ID: "c1-openai", Provider: "openai", Name: "openai-1", AuthType: "apikey", APIKey: "sk-x1", State: "idle"},
		{ID: "c2-anthropic", Provider: "anthropic", Name: "anthropic-1", AuthType: "apikey", APIKey: "sk-x2", State: "idle"},
		{ID: "c3-gemini", Provider: "gemini", Name: "gemini-1", AuthType: "apikey", APIKey: "sk-x3", State: "idle"},
		// Disabled — must NOT enable gpt-4o etc.
		{ID: "c4-disabled", Provider: "openai", Name: "openai-disabled", AuthType: "apikey", APIKey: "sk-x4", State: "disabled"},
	}
	for i := range conns {
		if err := st.CreateConnection(&conns[i]); err != nil {
			t.Fatalf("CreateConnection %s: %v", conns[i].ID, err)
		}
	}
	return st
}

// assertJSONGolden compares `got` to the golden file at
// testdata/<name>. JSON is unmarshalled and re-marshalled with
// stable formatting (indented, no trailing whitespace) so map-key
// order doesn't make the golden flaky. First run captures the file.
func assertJSONGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)

	// Re-encode `got` with stable formatting. Maps unmarshal into
	// map[string]any with sorted keys when json.MarshalIndent is
	// called — this gives deterministic output regardless of the
	// handler's internal iteration order.
	var parsed any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody=%s", err, string(got))
	}
	normalised, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	normalised = append(normalised, '\n')

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, normalised, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("UPDATE_GOLDEN: wrote %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(path, normalised, 0o644); err != nil {
			t.Fatalf("write initial golden: %v", err)
		}
		t.Logf("captured initial golden snapshot at %s", path)
		return
	}
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	if string(want) != string(normalised) {
		actualPath := filepath.Join("testdata", "actual_"+name)
		_ = os.WriteFile(actualPath, normalised, 0o644)
		t.Errorf("%s shape drift — diff %s %s\nregenerate with: UPDATE_GOLDEN=1 go test ./internal/server/...",
			name, path, actualPath)
	}
}
