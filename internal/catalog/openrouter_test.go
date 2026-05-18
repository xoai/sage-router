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
	// Post fix 20260514-pricing-mirror: anthropic/* entries also emit
	// a mirror candidate, so this fixture produces 2 updates (1
	// openrouter + 1 anthropic mirror with same pricing).
	if len(updates) != 2 {
		t.Fatalf("len(updates) = %d, want 2 (1 openrouter + 1 anthropic mirror)", len(updates))
	}

	// Find the openrouter-namespace entry (subject of the original conversion-math contract).
	var orEntry *PricingUpdate
	for i := range updates {
		if updates[i].Provider == "openrouter" {
			orEntry = &updates[i]
			break
		}
	}
	if orEntry == nil {
		t.Fatalf("no openrouter entry in updates: %v", updates)
	}
	if orEntry.ModelID != "anthropic/claude-sonnet-4.5" {
		t.Errorf("ModelID = %q, want anthropic/claude-sonnet-4.5", orEntry.ModelID)
	}
	if orEntry.Pricing.Source != SourceOpenRouter {
		t.Errorf("Source = %q, want %q", orEntry.Pricing.Source, SourceOpenRouter)
	}
	approxEq(t, "Input", orEntry.Pricing.Input, 3.0)
	approxEq(t, "Output", orEntry.Pricing.Output, 15.0)
	approxEq(t, "CacheRead", orEntry.Pricing.CacheRead, 0.3)
	approxEq(t, "CacheWrite", orEntry.Pricing.CacheWrite, 3.75)
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

// ----- Fix 20260514-pricing-mirror tests -----

// TestParseOpenRouterPricing_EmitsMirrorForKnownPrefixes — fix
// 20260514-pricing-mirror. The parser emits TWO PricingUpdate entries
// per OpenRouter entry whose prefix is in openrouterPrefixToProvider:
// one for the openrouter namespace + one mirror candidate for the
// direct-provider namespace. Unknown prefixes (meta-llama, mistralai,
// etc.) get only the openrouter entry.
func TestParseOpenRouterPricing_EmitsMirrorForKnownPrefixes(t *testing.T) {
	body := `{"data":[
		{"id":"openai/gpt-5","pricing":{"prompt":"0.0000125","completion":"0.0001"}},
		{"id":"anthropic/claude-opus-4.7","pricing":{"prompt":"0.0000050","completion":"0.0000250"}},
		{"id":"google/gemini-2.5-pro","pricing":{"prompt":"0.00000125","completion":"0.00001"}},
		{"id":"meta-llama/llama-3.3","pricing":{"prompt":"0.0000001","completion":"0.0000005"}}
	]}`
	updates, err := parseOpenRouterPricing([]byte(body))
	if err != nil {
		t.Fatalf("parseOpenRouterPricing: %v", err)
	}
	// 4 entries × (1 openrouter + 0 or 1 mirror)
	// = 4 openrouter + 3 mirrors (meta-llama excluded) = 7 total
	if len(updates) != 7 {
		t.Fatalf("len(updates) = %d, want 7 (4 openrouter + 3 mirrors)", len(updates))
	}

	// Count by provider.
	counts := map[string]int{}
	for _, u := range updates {
		counts[u.Provider]++
	}
	if counts["openrouter"] != 4 {
		t.Errorf("openrouter count = %d, want 4", counts["openrouter"])
	}
	if counts["openai"] != 1 || counts["anthropic"] != 1 || counts["gemini"] != 1 {
		t.Errorf("mirror counts = %v, want one each of openai/anthropic/gemini", counts)
	}

	// Verify mirror entries carry the BARE id (prefix stripped) and same pricing.
	for _, u := range updates {
		switch {
		case u.Provider == "openai" && u.ModelID == "gpt-5":
			if u.Pricing.Input != 12.5 { // 0.0000125 × 1e6 = 12.5
				t.Errorf("openai/gpt-5 mirror Input = %v, want 12.5", u.Pricing.Input)
			}
		case u.Provider == "anthropic" && u.ModelID == "claude-opus-4.7":
			// Note: still has dot here — normalization happens later in resolveMirrorCandidates.
			if u.Pricing.Input != 5.0 {
				t.Errorf("anthropic mirror Input = %v, want 5.0", u.Pricing.Input)
			}
		case u.Provider == "gemini" && u.ModelID == "gemini-2.5-pro":
			if u.Pricing.Input != 1.25 {
				t.Errorf("gemini mirror Input = %v, want 1.25", u.Pricing.Input)
			}
		}
	}
}

// TestFetchAndPersist_MirrorsToExistingDirectRows — fix 20260514-pricing-mirror.
// Set up catalog_models with (openai, gpt-5) — simulating prior openai
// discovery via openai@openrouter-mirror lister. Feed an OpenRouter
// response containing openai/gpt-5. After FetchAndPersist, the
// catalog_pricing row for (openai, gpt-5) exists with source=openrouter
// and openrouter's prices.
func TestFetchAndPersist_MirrorsToExistingDirectRows(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Pre-seed the direct-provider model row (simulates prior discovery).
	if err := cs.UpsertModel(ctx, Model{
		Provider: "openai", ModelID: "gpt-5", Source: SourceDiscovery,
	}); err != nil {
		t.Fatalf("seed (openai, gpt-5): %v", err)
	}

	body := `{"data":[{"id":"openai/gpt-5","pricing":{"prompt":"0.00000125","completion":"0.00001","input_cache_read":"0.000000125"}}]}`
	url, closeSrv := newOpenRouterTestServer(t, []byte(body))
	defer closeSrv()

	r := &OpenRouterRefresher{Store: cs, URL: url + "/api/v1/models", Client: http.DefaultClient}
	if _, err := r.FetchAndPersist(ctx); err != nil {
		t.Fatalf("FetchAndPersist: %v", err)
	}

	// Direct-provider pricing exists with source=openrouter.
	p, err := cs.GetPricing(ctx, "openai", "gpt-5")
	if err != nil {
		t.Fatalf("GetPricing (openai, gpt-5): %v", err)
	}
	if p == nil {
		t.Fatal("mirror did not write (openai, gpt-5) pricing row")
	}
	if p.Source != SourceOpenRouter {
		t.Errorf("Source = %q, want %q", p.Source, SourceOpenRouter)
	}
	approxEq(t, "Input", p.Input, 1.25)
	approxEq(t, "Output", p.Output, 10.0)
}

// TestFetchAndPersist_AnthropicDateNormalizationLands — fix
// 20260514-pricing-mirror Bug 1 (anthropic dot/dash mismatch). Set up
// catalog_models with (anthropic, claude-opus-4-1-20250805) — the
// date-suffixed form our discovery writes. Feed OpenRouter response
// with anthropic/claude-opus-4.1 (the dot form OpenRouter emits).
// Normalization MUST match these and write pricing to the date-suffixed
// catalog row.
func TestFetchAndPersist_AnthropicDateNormalizationLands(t *testing.T) {
	cs, _, ctx := freshStore(t)

	if err := cs.UpsertModel(ctx, Model{
		Provider: "anthropic", ModelID: "claude-opus-4-1-20250805", Source: SourceDiscovery,
	}); err != nil {
		t.Fatalf("seed anthropic discovery: %v", err)
	}

	body := `{"data":[{"id":"anthropic/claude-opus-4.1","pricing":{"prompt":"0.000015","completion":"0.000075"}}]}`
	url, closeSrv := newOpenRouterTestServer(t, []byte(body))
	defer closeSrv()

	r := &OpenRouterRefresher{Store: cs, URL: url + "/api/v1/models", Client: http.DefaultClient}
	if _, err := r.FetchAndPersist(ctx); err != nil {
		t.Fatalf("FetchAndPersist: %v", err)
	}

	// Pricing landed on the DATE-SUFFIXED catalog row, not the bare form.
	p, err := cs.GetPricing(ctx, "anthropic", "claude-opus-4-1-20250805")
	if err != nil || p == nil {
		t.Fatalf("GetPricing date-suffixed row: err=%v p=%v", err, p)
	}
	if p.Source != SourceOpenRouter {
		t.Errorf("Source = %q, want openrouter", p.Source)
	}
	approxEq(t, "Input", p.Input, 15.0)
	approxEq(t, "Output", p.Output, 75.0)
}

// TestFetchAndPersist_SkipMirrorForEmptyDirectProviderRows — fix
// 20260514-pricing-mirror FK safety. No openai/anthropic/gemini rows
// in catalog_models. OpenRouter response contains entries for those
// namespaces. After FetchAndPersist, only openrouter-namespace rows
// are written; no mirror rows attempted (no FK violation, no error).
func TestFetchAndPersist_SkipMirrorForEmptyDirectProviderRows(t *testing.T) {
	cs, _, ctx := freshStore(t)
	// Deliberately do NOT seed any direct-provider rows.

	body := `{"data":[
		{"id":"openai/gpt-5","pricing":{"prompt":"0.00000125","completion":"0.00001"}},
		{"id":"anthropic/claude-opus-4.7","pricing":{"prompt":"0.000005","completion":"0.000025"}}
	]}`
	url, closeSrv := newOpenRouterTestServer(t, []byte(body))
	defer closeSrv()

	r := &OpenRouterRefresher{Store: cs, URL: url + "/api/v1/models", Client: http.DefaultClient}
	if _, err := r.FetchAndPersist(ctx); err != nil {
		t.Fatalf("FetchAndPersist returned error when it should silently skip mirrors: %v", err)
	}

	// Openrouter-namespace rows exist.
	if p, _ := cs.GetPricing(ctx, "openrouter", "openai/gpt-5"); p == nil {
		t.Error("openrouter/openai/gpt-5 pricing missing")
	}
	// Mirror rows DO NOT exist (no FK target).
	if p, _ := cs.GetPricing(ctx, "openai", "gpt-5"); p != nil {
		t.Errorf("mirror to (openai, gpt-5) was written despite no catalog_models row; got %+v", p)
	}
	if p, _ := cs.GetPricing(ctx, "anthropic", "claude-opus-4-7"); p != nil {
		t.Errorf("mirror to (anthropic, ...) was written despite no catalog_models row; got %+v", p)
	}
}

// TestFetchAndPersist_MirrorOverridesSeedPricing — fix
// 20260514-pricing-mirror. Pre-seed (openai, gpt-4o) catalog_models +
// pricing rows with source=seed at $2.50/$10.00. Run FetchAndPersist
// with OpenRouter response. Per ADR-2 pricing precedence (openrouter
// > seed), the seed pricing row gets overwritten with openrouter's
// values + source=openrouter.
func TestFetchAndPersist_MirrorOverridesSeedPricing(t *testing.T) {
	cs, _, ctx := freshStore(t)

	if err := cs.UpsertModel(ctx, Model{
		Provider: "openai", ModelID: "gpt-4o", Source: SourceDiscovery,
	}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if err := cs.UpsertPricing(ctx, "openai", "gpt-4o", Pricing{
		Input: 2.5, Output: 10.0, Source: SourceSeed,
	}); err != nil {
		t.Fatalf("seed pricing: %v", err)
	}

	body := `{"data":[{"id":"openai/gpt-4o","pricing":{"prompt":"0.0000025","completion":"0.00001"}}]}`
	url, closeSrv := newOpenRouterTestServer(t, []byte(body))
	defer closeSrv()

	r := &OpenRouterRefresher{Store: cs, URL: url + "/api/v1/models", Client: http.DefaultClient}
	if _, err := r.FetchAndPersist(ctx); err != nil {
		t.Fatalf("FetchAndPersist: %v", err)
	}

	p, _ := cs.GetPricing(ctx, "openai", "gpt-4o")
	if p == nil {
		t.Fatal("pricing missing after refresh")
	}
	if p.Source != SourceOpenRouter {
		t.Errorf("Source = %q, want %q (openrouter should override seed)", p.Source, SourceOpenRouter)
	}
	// Input is 0.0000025 × 1e6 = 2.5 — coincidentally same as seed.
	// Output is 0.00001 × 1e6 = 10.0 — same too. Use a distinctive value
	// to verify the WRITE actually happened:
	approxEq(t, "Input", p.Input, 2.5)
	approxEq(t, "Output", p.Output, 10.0)
}

// TestFetchAndPersist_PreservesDirectProviderModelSource — fix
// 20260514-pricing-mirror plan-review v2 critical fix. The mirror
// path must NOT call UpsertModel on direct-provider rows — only on
// openrouter-namespace rows. (openai, gpt-5) must keep source=discovery
// in catalog_models after a refresh that mirrors its pricing.
func TestFetchAndPersist_PreservesDirectProviderModelSource(t *testing.T) {
	cs, _, ctx := freshStore(t)

	if err := cs.UpsertModel(ctx, Model{
		Provider: "openai", ModelID: "gpt-5", Source: SourceDiscovery,
		Caps: Capabilities{SupportsImages: true, SupportsTools: true, SupportsThinking: false},
	}); err != nil {
		t.Fatalf("seed discovery row: %v", err)
	}

	body := `{"data":[{"id":"openai/gpt-5","pricing":{"prompt":"0.00000125","completion":"0.00001"}}]}`
	url, closeSrv := newOpenRouterTestServer(t, []byte(body))
	defer closeSrv()

	r := &OpenRouterRefresher{Store: cs, URL: url + "/api/v1/models", Client: http.DefaultClient}
	if _, err := r.FetchAndPersist(ctx); err != nil {
		t.Fatalf("FetchAndPersist: %v", err)
	}

	// catalog_models row STAYS source=discovery — mirror did not clobber it.
	m, err := cs.GetModel(ctx, "openai", "gpt-5")
	if err != nil || m == nil {
		t.Fatalf("GetModel: err=%v m=%v", err, m)
	}
	if m.Source != SourceDiscovery {
		t.Errorf("catalog_models source = %q, want %q (mirror MUST NOT call UpsertModel on direct-provider rows)",
			m.Source, SourceDiscovery)
	}
	// Capability flags preserved from discovery.
	if !m.Caps.SupportsImages || !m.Caps.SupportsTools {
		t.Errorf("capability flags clobbered: %+v (UPSERT zeroed them — bug)", m.Caps)
	}

	// AND catalog_pricing row exists with source=openrouter.
	p, err := cs.GetPricing(ctx, "openai", "gpt-5")
	if err != nil || p == nil {
		t.Fatalf("GetPricing: err=%v p=%v", err, p)
	}
	if p.Source != SourceOpenRouter {
		t.Errorf("catalog_pricing source = %q, want %q", p.Source, SourceOpenRouter)
	}
}
