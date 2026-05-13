package catalog

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Models Discovery M2.8 — OpenRouter pricing oracle.
//
// The refresher downloads the public OpenRouter `/api/v1/models`
// catalog, converts per-token USD strings into per-1M-token USD
// floats, and persists with `source='openrouter'` to catalog_pricing
// (also creating catalog_models rows for any new qualified IDs so the
// FK is satisfied).
//
// Source precedence (ADR-2 §Conflict resolution): user > openrouter >
// discovery > seed. The user-pricing endpoint is the only way to
// overwrite an openrouter row; discovery and seed both lose.

// loadOpenRouterFixture reads the vendored fixture (M2.7a) from
// testdata/openrouter_response.json. Tests share one read per process
// — the fixture is 432 KB so re-reading per test is mild waste, but
// also harmless.
func loadOpenRouterFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "openrouter_response.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// newOpenRouterTestServer spins up an httptest.Server that serves the
// vendored fixture verbatim on every request. Capture path so the test
// can assert the refresher called the right endpoint.
func newOpenRouterTestServer(t *testing.T, body []byte) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/models") {
			t.Errorf("unexpected path %q; want suffix /api/v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	return srv.URL, srv.Close
}

// TestFetchAndPersist_ParsesOpenRouterShape — AC17/AC18 happy path.
// The fixture serves the live OpenRouter shape (365 models, all
// canonical fields). After FetchAndPersist:
//
//   - At least one catalog_pricing row exists with source='openrouter'.
//   - A pinned model (claude-sonnet-4.5) has its per-token prices
//     correctly converted to per-1M-token values matching what we
//     expect from the fixture's "prompt" / "completion" /
//     "input_cache_read" / "input_cache_write" strings.
//   - The Registry's in-memory cache for that pinned model was
//     invalidated (subsequent Pricing() returns the openrouter values).
func TestFetchAndPersist_ParsesOpenRouterShape(t *testing.T) {
	cs, _, ctx := freshStore(t)
	reg := NewRegistry(cs)

	body := loadOpenRouterFixture(t)
	url, closeSrv := newOpenRouterTestServer(t, body)
	defer closeSrv()

	r := &OpenRouterRefresher{
		Store:    cs,
		Registry: reg,
		URL:      url + "/api/v1/models",
		Client:   http.DefaultClient,
	}

	count, err := r.FetchAndPersist(ctx)
	if err != nil {
		t.Fatalf("FetchAndPersist: %v", err)
	}
	if count == 0 {
		t.Fatalf("Count = 0, want > 0 (fixture has 365 models)")
	}
	if count < 100 {
		t.Errorf("Count = %d, suspiciously low for the fixture (~365 models)", count)
	}

	// Pin claude-sonnet-4.5 — a stable Anthropic model present in the
	// fixture with predictable per-token prices that round-trip
	// cleanly into per-1M values:
	//   prompt 0.000003     → input  3.0
	//   completion 0.000015 → output 15.0
	//   input_cache_read 0.0000003     → cache_read  0.3
	//   input_cache_write 0.00000375   → cache_write 3.75
	pricing, err := cs.GetPricing(ctx, "openrouter", "anthropic/claude-sonnet-4.5")
	if err != nil {
		t.Fatalf("GetPricing claude-sonnet-4.5: %v", err)
	}
	if pricing == nil {
		t.Fatal("claude-sonnet-4.5 pricing row missing after FetchAndPersist")
	}
	if pricing.Source != SourceOpenRouter {
		t.Errorf("Source = %q, want %q", pricing.Source, SourceOpenRouter)
	}
	approxEq(t, "Input", pricing.Input, 3.0)
	approxEq(t, "Output", pricing.Output, 15.0)
	approxEq(t, "CacheRead", pricing.CacheRead, 0.3)
	approxEq(t, "CacheWrite", pricing.CacheWrite, 3.75)

	// Registry was invalidated — the cache now serves the openrouter
	// values, not whatever (zero) state it had before.
	regPricing := reg.Pricing("openrouter", "anthropic/claude-sonnet-4.5")
	if regPricing == nil {
		t.Fatal("Registry.Pricing returned nil after FetchAndPersist")
	}
	approxEq(t, "Registry Input", regPricing.Input, 3.0)
}

// approxEq asserts float equality within 1e-9 — covers any IEEE-754
// rounding artifact from the per-token-USD string → per-1M-USD float
// conversion without being so loose it admits a 1% drift.
func approxEq(t *testing.T, name string, got, want float64) {
	t.Helper()
	const eps = 1e-9
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > eps {
		t.Errorf("%s = %g, want %g (diff %g > %g)", name, got, want, diff, eps)
	}
}

// TestFetchAndPersist_PreservesUserOverrides — AC18. A pricing row
// with source='user' is the authoritative override; the openrouter
// refresher MUST NOT overwrite it. After FetchAndPersist, the user
// values remain — even if openrouter quotes different numbers.
func TestFetchAndPersist_PreservesUserOverrides(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Seed the model row + a user pricing override BEFORE the refresh.
	// Using gpt-4o-mini — present in the OpenRouter fixture.
	const userInput = 99.99
	const userOutput = 999.99
	if err := cs.UpsertModel(ctx, Model{
		Provider: "openrouter",
		ModelID:  "openai/gpt-4o-mini",
		Source:   SourceUser,
	}); err != nil {
		t.Fatalf("seed user model: %v", err)
	}
	if err := cs.UpsertPricing(ctx, "openrouter", "openai/gpt-4o-mini", Pricing{
		Input:  userInput,
		Output: userOutput,
		Source: SourceUser,
	}); err != nil {
		t.Fatalf("seed user pricing: %v", err)
	}

	body := loadOpenRouterFixture(t)
	url, closeSrv := newOpenRouterTestServer(t, body)
	defer closeSrv()

	r := &OpenRouterRefresher{
		Store:    cs,
		Registry: NewRegistry(cs),
		URL:      url + "/api/v1/models",
		Client:   http.DefaultClient,
	}
	if _, err := r.FetchAndPersist(ctx); err != nil {
		t.Fatalf("FetchAndPersist: %v", err)
	}

	pricing, err := cs.GetPricing(ctx, "openrouter", "openai/gpt-4o-mini")
	if err != nil {
		t.Fatalf("GetPricing: %v", err)
	}
	if pricing == nil {
		t.Fatal("user pricing row vanished after refresh")
	}
	if pricing.Source != SourceUser {
		t.Errorf("Source = %q after refresh, want %q (user-overwritten)", pricing.Source, SourceUser)
	}
	approxEq(t, "Input (user-preserved)", pricing.Input, userInput)
	approxEq(t, "Output (user-preserved)", pricing.Output, userOutput)
}

// TestFetchAndPersist_UpsertModelsBeforePricing — AC18 FK safety.
// catalog_pricing has a FK on catalog_models. The refresher MUST
// upsert model rows before pricing rows so the FK is always satisfied.
// Test: fresh DB, refresh, assert every catalog_pricing row has a
// matching catalog_models row.
func TestFetchAndPersist_UpsertModelsBeforePricing(t *testing.T) {
	cs, st, ctx := freshStore(t)

	body := loadOpenRouterFixture(t)
	url, closeSrv := newOpenRouterTestServer(t, body)
	defer closeSrv()

	r := &OpenRouterRefresher{
		Store:    cs,
		Registry: NewRegistry(cs),
		URL:      url + "/api/v1/models",
		Client:   http.DefaultClient,
	}
	if _, err := r.FetchAndPersist(ctx); err != nil {
		t.Fatalf("FetchAndPersist: %v", err)
	}

	// Count orphan pricing rows: pricing rows whose (provider, model_id)
	// has no matching model row. With FK ON DELETE CASCADE this SHOULD
	// be impossible at the SQLite layer; the test pins the contract for
	// the application layer (refresher writes models first).
	var orphans int
	err := st.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM catalog_pricing p
		WHERE NOT EXISTS (
			SELECT 1 FROM catalog_models m
			WHERE m.provider = p.provider AND m.model_id = p.model_id
		)
	`).Scan(&orphans)
	if err != nil {
		t.Fatalf("orphan query: %v", err)
	}
	if orphans != 0 {
		t.Errorf("orphan pricing rows: %d (FK contract violation; models must upsert before pricing)", orphans)
	}

	// And the positive: openrouter pricing rows exist at all.
	var openrouterPricing int
	err = st.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM catalog_pricing WHERE source = 'openrouter'
	`).Scan(&openrouterPricing)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	if openrouterPricing == 0 {
		t.Fatal("no openrouter pricing rows after refresh; expected > 0")
	}
}

// TestFetchAndPersist_FailureIsNonFatal — AC18 failure contract.
// HTTP 500 from the upstream must return an error without panicking,
// without partially-applying writes, and without corrupting existing
// catalog state. Used by the background refresher to log+continue
// rather than crash the process.
func TestFetchAndPersist_FailureIsNonFatal(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Seed a baseline pricing row that must survive a failed refresh.
	if err := cs.UpsertModel(ctx, Model{
		Provider: "openrouter",
		ModelID:  "anthropic/claude-sonnet-4.5",
		Source:   SourceSeed,
	}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if err := cs.UpsertPricing(ctx, "openrouter", "anthropic/claude-sonnet-4.5", Pricing{
		Input:  1.0,
		Output: 5.0,
		Source: SourceSeed,
	}); err != nil {
		t.Fatalf("seed pricing: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream down"}`))
	}))
	defer srv.Close()

	r := &OpenRouterRefresher{
		Store:    cs,
		Registry: NewRegistry(cs),
		URL:      srv.URL + "/api/v1/models",
		Client:   http.DefaultClient,
	}
	count, err := r.FetchAndPersist(ctx)
	if err == nil {
		t.Fatalf("expected error on 500; got count=%d err=nil", count)
	}
	if count != 0 {
		t.Errorf("Count = %d on failure path; want 0 (no writes)", count)
	}

	// Seed row untouched — failure path must not have advanced the
	// pricing source or values.
	pricing, err := cs.GetPricing(ctx, "openrouter", "anthropic/claude-sonnet-4.5")
	if err != nil {
		t.Fatalf("GetPricing: %v", err)
	}
	if pricing == nil {
		t.Fatal("seed pricing vanished after failed refresh")
	}
	if pricing.Source != SourceSeed {
		t.Errorf("Source = %q post-failure, want %q (seed preserved)", pricing.Source, SourceSeed)
	}
	approxEq(t, "Input (seed-preserved)", pricing.Input, 1.0)
}

// TestFetchAndPersist_MalformedJSONReturnsError — adjacent contract:
// the upstream returned 200 but the body isn't valid JSON. Same
// failure semantics as the 500 case: error returned, no writes.
func TestFetchAndPersist_MalformedJSONReturnsError(t *testing.T) {
	cs, _, ctx := freshStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not even close to json`))
	}))
	defer srv.Close()

	r := &OpenRouterRefresher{
		Store:    cs,
		Registry: NewRegistry(cs),
		URL:      srv.URL + "/api/v1/models",
		Client:   http.DefaultClient,
	}
	if _, err := r.FetchAndPersist(ctx); err == nil {
		t.Fatal("expected error on malformed body")
	}
}

// TestParseOpenRouterPricing_ConversionMath — unit test for the
// per-token → per-1M-token math. Inline fixture with one model
// pinned at canonical claude-sonnet-4.5 values to detect drift in
// either the JSON shape parsing or the multiplication factor.
func TestParseOpenRouterPricing_ConversionMath(t *testing.T) {
	body := []byte(`{
		"data": [
			{
				"id": "anthropic/claude-sonnet-4.5",
				"name": "Anthropic: Claude Sonnet 4.5",
				"context_length": 1000000,
				"pricing": {
					"prompt": "0.000003",
					"completion": "0.000015",
					"input_cache_read": "0.0000003",
					"input_cache_write": "0.00000375"
				},
				"top_provider": {"context_length": 1000000, "max_completion_tokens": 64000}
			}
		]
	}`)
	updates, err := parseOpenRouterPricing(body)
	if err != nil {
		t.Fatalf("parseOpenRouterPricing: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("len(updates) = %d, want 1", len(updates))
	}
	u := updates[0]
	if u.Provider != "openrouter" {
		t.Errorf("Provider = %q, want openrouter", u.Provider)
	}
	if u.ModelID != "anthropic/claude-sonnet-4.5" {
		t.Errorf("ModelID = %q, want anthropic/claude-sonnet-4.5", u.ModelID)
	}
	if u.Pricing.Source != SourceOpenRouter {
		t.Errorf("Source = %q, want %q", u.Pricing.Source, SourceOpenRouter)
	}
	approxEq(t, "Input", u.Pricing.Input, 3.0)
	approxEq(t, "Output", u.Pricing.Output, 15.0)
	approxEq(t, "CacheRead", u.Pricing.CacheRead, 0.3)
	approxEq(t, "CacheWrite", u.Pricing.CacheWrite, 3.75)
}

// TestParseOpenRouterPricing_FreeModelHasZeroPrices — `:free` models
// in the fixture serve at $0 across all dimensions. Parser must NOT
// skip them; persisting a $0 row is the right answer when the user
// asks "what does this model cost?"
func TestParseOpenRouterPricing_FreeModelHasZeroPrices(t *testing.T) {
	body := []byte(`{
		"data": [
			{
				"id": "vendor/model-name:free",
				"pricing": {
					"prompt": "0",
					"completion": "0",
					"input_cache_read": "0",
					"input_cache_write": "0"
				}
			}
		]
	}`)
	updates, err := parseOpenRouterPricing(body)
	if err != nil {
		t.Fatalf("parseOpenRouterPricing: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("len(updates) = %d, want 1 (free models still persist)", len(updates))
	}
	u := updates[0]
	approxEq(t, "Input (free)", u.Pricing.Input, 0)
	approxEq(t, "Output (free)", u.Pricing.Output, 0)
}

// TestParseOpenRouterPricing_MalformedPriceStringYieldsZeroAndPreservesRow —
// the contract for unparseable price strings (review finding MAJOR-2).
// A garbage value like `"not-a-number"` returns 0 for that field and
// logs a Warn (visible to operators), but does NOT drop the rest of
// the entry. Other valid prices on the same model still persist.
//
// Distinct from `_FreeModelHasZeroPrices` (string "0" is valid) and
// `_NoPricingObjectIsSkipped` (the `pricing` object itself is missing).
// This case covers data corruption — the upstream returned a malformed
// field — and ensures the row survives so operators can catch the
// issue from the Warn log without losing pricing data for the rest
// of the catalog.
func TestParseOpenRouterPricing_MalformedPriceStringYieldsZeroAndPreservesRow(t *testing.T) {
	// Capture slog output so the test can assert the Warn line fires on
	// every malformed field. Without this, a regression to silent-zero
	// would leave the value assertions intact but eliminate the operator-
	// visible diagnostic. Same bytes.Buffer + slog.NewTextHandler pattern
	// used by TestSubscriptionAuth_TokenNeverAppearsInLogs.
	var logbuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	body := []byte(`{
		"data": [
			{
				"id": "vendor/garbage-price-model",
				"pricing": {
					"prompt": "not-a-number",
					"completion": "0.000005",
					"input_cache_read": "$0.0001",
					"input_cache_write": "0.00000375"
				}
			}
		]
	}`)
	updates, err := parseOpenRouterPricing(body)
	if err != nil {
		t.Fatalf("parseOpenRouterPricing: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("len(updates) = %d, want 1 (row preserved on partial malformation)", len(updates))
	}
	u := updates[0]
	if u.ModelID != "vendor/garbage-price-model" {
		t.Errorf("ModelID = %q, want vendor/garbage-price-model", u.ModelID)
	}
	// Malformed fields → 0.
	approxEq(t, "Input (malformed)", u.Pricing.Input, 0)
	approxEq(t, "CacheRead (malformed $-prefix)", u.Pricing.CacheRead, 0)
	// Valid fields on the same row → preserved.
	approxEq(t, "Output (valid)", u.Pricing.Output, 5.0)
	approxEq(t, "CacheWrite (valid)", u.Pricing.CacheWrite, 3.75)

	// Operator-visible diagnostic: both malformed fields produced a Warn.
	// A regression to silent-zero would leave the value assertions intact
	// but lose the only signal an operator has that the upstream sent
	// garbage. The model_id appears in the log so multi-row corruption
	// can be attributed at log-grep time.
	logs := logbuf.String()
	if !strings.Contains(logs, "vendor/garbage-price-model") {
		t.Errorf("expected slog.Warn to reference model_id; got: %s", logs)
	}
	if !strings.Contains(logs, "not-a-number") {
		t.Errorf("expected slog.Warn to reference the bad input value 'not-a-number'; got: %s", logs)
	}
}

// TestParseOpenRouterPricing_NoPricingObjectIsSkipped — a model entry
// with `pricing: null` (or missing) must be skipped, not persisted
// at $0. Distinct from the free-model case: there we KNOW it's $0;
// here we know nothing.
func TestParseOpenRouterPricing_NoPricingObjectIsSkipped(t *testing.T) {
	body := []byte(`{
		"data": [
			{"id": "vendor/known", "pricing": {"prompt": "0.000001", "completion": "0.000005"}},
			{"id": "vendor/no-pricing"}
		]
	}`)
	updates, err := parseOpenRouterPricing(body)
	if err != nil {
		t.Fatalf("parseOpenRouterPricing: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("len(updates) = %d, want 1 (no-pricing entry skipped)", len(updates))
	}
	if updates[0].ModelID != "vendor/known" {
		t.Errorf("ModelID = %q, want vendor/known", updates[0].ModelID)
	}
}
