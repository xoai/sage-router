package catalog

import (
	"context"
	"testing"
)

// Models Discovery M2.9 — Full source-precedence matrix backfill.
//
// ADR-2 §Conflict resolution defines a 4-source precedence partial
// order: user > openrouter > discovery > seed for pricing; user >
// (openrouter, discovery) > seed for models. M1 tests cover the 4
// most-critical cases (seed-loses, user-wins, openrouter-vs-discovery
// pricing preserve, user-beats-openrouter). This file backfills the
// remaining 12 cross-source transitions for both tables and pins the
// 4 self-refresh cases so a future refactor of the WHERE clauses
// can't silently break any precedence rule.
//
// Transition naming convention: `<existing>_then_<incoming>` so the
// test names sort to a readable matrix in test output.

// Matrix-test seeded identifiers. Sourced from a single pair of
// constants so the helper closures, the upsert helper, and the GetPricing
// read-back all reference the same (provider, model_id). A previous
// version hardcoded "p"/"m" in three closure locations; if any helper
// changed the seeded ID, the closures would silently write to a different
// row than GetPricing reads back, and the matrix would assert nothing.
const (
	matrixProvider = "p"
	matrixModelID  = "m"
)

// matrixCase describes one (existing, incoming) pair and the expected
// outcome. `overwrite=true` means the incoming row wins; false means
// the existing row is preserved.
type matrixCase struct {
	existing  string
	incoming  string
	overwrite bool
}

// modelsMatrix covers the 16 (existing, incoming) source pairs for
// catalog_models. Precedence (from sqlite.go: UpsertModel WHERE clause):
//
//	WHERE NOT (catalog_models.source = 'user' AND excluded.source != 'user')
//	  AND NOT (catalog_models.source != 'seed' AND excluded.source = 'seed')
//
// So:
//   - user → anything-not-user: ignore (block clause 1)
//   - non-seed → seed: ignore (block clause 2)
//   - everything else: overwrite (including self-refresh and seed → anything)
//
// Self-refresh rows (seed→seed, discovery→discovery, openrouter→openrouter,
// user→user) are present for completeness — they aren't precedence cases
// per se but they exercise the SET clause's idempotency. ADR-2's truth
// table was extended in M2.9 to include these explicitly.
var modelsMatrix = []matrixCase{
	// existing=seed: any incoming overwrites (including seed-refresh).
	{SourceSeed, SourceSeed, true},
	{SourceSeed, SourceDiscovery, true},
	{SourceSeed, SourceOpenRouter, true},
	{SourceSeed, SourceUser, true},
	// existing=discovery: seed loses (rule 2); others overwrite.
	{SourceDiscovery, SourceSeed, false},
	{SourceDiscovery, SourceDiscovery, true},
	{SourceDiscovery, SourceOpenRouter, true},
	{SourceDiscovery, SourceUser, true},
	// existing=openrouter: seed loses (rule 2); others overwrite.
	{SourceOpenRouter, SourceSeed, false},
	{SourceOpenRouter, SourceDiscovery, true},
	{SourceOpenRouter, SourceOpenRouter, true},
	{SourceOpenRouter, SourceUser, true},
	// existing=user: only user can overwrite (rule 1).
	{SourceUser, SourceSeed, false},
	{SourceUser, SourceDiscovery, false},
	{SourceUser, SourceOpenRouter, false},
	{SourceUser, SourceUser, true},
}

// pricingMatrix is the same 16 pairs as modelsMatrix EXCEPT for one
// extra precedence rule from UpsertPricing's WHERE clause:
//
//	AND NOT (catalog_pricing.source = 'openrouter' AND excluded.source = 'discovery')
//
// So (openrouter → discovery) flips from overwrite (in models) to
// ignore (in pricing). All other 15 pairs match models exactly.
var pricingMatrix = []matrixCase{
	{SourceSeed, SourceSeed, true},
	{SourceSeed, SourceDiscovery, true},
	{SourceSeed, SourceOpenRouter, true},
	{SourceSeed, SourceUser, true},
	{SourceDiscovery, SourceSeed, false},
	{SourceDiscovery, SourceDiscovery, true},
	{SourceDiscovery, SourceOpenRouter, true},
	{SourceDiscovery, SourceUser, true},
	{SourceOpenRouter, SourceSeed, false},
	{SourceOpenRouter, SourceDiscovery, false}, // pricing-specific: openrouter beats discovery
	{SourceOpenRouter, SourceOpenRouter, true},
	{SourceOpenRouter, SourceUser, true},
	{SourceUser, SourceSeed, false},
	{SourceUser, SourceDiscovery, false},
	{SourceUser, SourceOpenRouter, false},
	{SourceUser, SourceUser, true},
}

// TestUpsertModel_PrecedenceMatrix — exercises all 16 (existing, incoming)
// source transitions for catalog_models. Each iteration uses a fresh
// store so prior cases don't interfere. The display_name field carries
// the iteration's marker so we can assert which write actually landed.
func TestUpsertModel_PrecedenceMatrix(t *testing.T) {
	for _, c := range modelsMatrix {
		c := c
		t.Run(c.existing+"_then_"+c.incoming, func(t *testing.T) {
			cs, _, ctx := freshStore(t)

			// First write: the "existing" row.
			first := Model{
				Provider:    matrixProvider, ModelID: matrixModelID,
				DisplayName: "EXISTING-" + c.existing,
				Source:      c.existing,
			}
			if err := cs.UpsertModel(ctx, first); err != nil {
				t.Fatalf("UpsertModel existing(%s): %v", c.existing, err)
			}

			// Second write: the "incoming" row with a distinct display_name
			// so we can tell which one survived.
			second := Model{
				Provider:    matrixProvider, ModelID: matrixModelID,
				DisplayName: "INCOMING-" + c.incoming,
				Source:      c.incoming,
			}
			if err := cs.UpsertModel(ctx, second); err != nil {
				t.Fatalf("UpsertModel incoming(%s): %v", c.incoming, err)
			}

			got, err := cs.GetModel(ctx, matrixProvider, matrixModelID)
			if err != nil {
				t.Fatalf("GetModel: %v", err)
			}
			if got == nil {
				t.Fatal("GetModel returned nil after upsert")
			}

			wantName := "EXISTING-" + c.existing
			wantSrc := c.existing
			if c.overwrite {
				wantName = "INCOMING-" + c.incoming
				wantSrc = c.incoming
			}
			if got.DisplayName != wantName {
				t.Errorf("DisplayName = %q, want %q (overwrite=%v)",
					got.DisplayName, wantName, c.overwrite)
			}
			if got.Source != wantSrc {
				t.Errorf("Source = %q, want %q (overwrite=%v)",
					got.Source, wantSrc, c.overwrite)
			}
		})
	}
}

// TestUpsertPricing_PrecedenceMatrix — exercises all 16 source
// transitions for catalog_pricing via the single-row UpsertPricing
// path. Distinct input_price values mark the iteration.
func TestUpsertPricing_PrecedenceMatrix(t *testing.T) {
	runPricingMatrix(t, func(cs Store, ctx context.Context, p Pricing) error {
		return cs.UpsertPricing(ctx, matrixProvider, matrixModelID, p)
	})
}

// TestBulkUpsertPricing_PrecedenceMatrix — same matrix, exercised
// through the single-tx bulk path used by the OpenRouter refresher
// (M2.8). The WHERE clause in BulkUpsertPricing is duplicated from
// UpsertPricing; without this test, an edit to one without the other
// would ship a silent precedence divergence on the load-bearing
// production path. (M2.9-review M-2.)
func TestBulkUpsertPricing_PrecedenceMatrix(t *testing.T) {
	runPricingMatrix(t, func(cs Store, ctx context.Context, p Pricing) error {
		return cs.BulkUpsertPricing(ctx, []PricingUpdate{
			{Provider: matrixProvider, ModelID: matrixModelID, Pricing: p},
		})
	})
}

// runPricingMatrix is the inner helper shared by single-row and bulk
// matrix tests. `write` is the under-test write path; both call paths
// must respect the same ADR-2 precedence rules.
func runPricingMatrix(t *testing.T, write func(Store, context.Context, Pricing) error) {
	t.Helper()
	for _, c := range pricingMatrix {
		c := c
		t.Run(c.existing+"_then_"+c.incoming, func(t *testing.T) {
			cs, _, ctx := freshStore(t)

			// FK satisfaction: seed a non-user model row that lets ANY
			// incoming pricing source land. Using seed here is safe
			// because we never write a model row at SourceUser in these
			// tests (which would block the matrix).
			if err := cs.UpsertModel(ctx, Model{
				Provider: matrixProvider, ModelID: matrixModelID, Source: SourceSeed,
			}); err != nil {
				t.Fatalf("seed model: %v", err)
			}

			// First pricing write (the "existing" source).
			const existingInput = 1.0
			if err := write(cs, ctx, Pricing{
				Input: existingInput, Source: c.existing,
			}); err != nil {
				t.Fatalf("write existing(%s): %v", c.existing, err)
			}

			// Second pricing write (the "incoming" source) — distinct
			// Input value pins which row survived.
			const incomingInput = 99.0
			if err := write(cs, ctx, Pricing{
				Input: incomingInput, Source: c.incoming,
			}); err != nil {
				t.Fatalf("write incoming(%s): %v", c.incoming, err)
			}

			got, err := cs.GetPricing(ctx, matrixProvider, matrixModelID)
			if err != nil {
				t.Fatalf("GetPricing: %v", err)
			}
			if got == nil {
				t.Fatal("GetPricing returned nil")
			}

			wantInput := existingInput
			wantSrc := c.existing
			if c.overwrite {
				wantInput = incomingInput
				wantSrc = c.incoming
			}
			if got.Input != wantInput {
				t.Errorf("Input = %g, want %g (overwrite=%v)",
					got.Input, wantInput, c.overwrite)
			}
			if got.Source != wantSrc {
				t.Errorf("Source = %q, want %q (overwrite=%v)",
					got.Source, wantSrc, c.overwrite)
			}
		})
	}
}

// TestPrecedenceMatrices_AgreeExceptOnOpenRouterVsDiscovery — a meta
// assertion that the only legitimate difference between modelsMatrix
// and pricingMatrix is the (openrouter, discovery) pair. If anyone
// changes the precedence rules in one table without the other, this
// test fires.
func TestPrecedenceMatrices_AgreeExceptOnOpenRouterVsDiscovery(t *testing.T) {
	if len(modelsMatrix) != len(pricingMatrix) {
		t.Fatalf("matrix lengths differ: models=%d pricing=%d",
			len(modelsMatrix), len(pricingMatrix))
	}
	for i := range modelsMatrix {
		m := modelsMatrix[i]
		p := pricingMatrix[i]
		if m.existing != p.existing || m.incoming != p.incoming {
			t.Fatalf("matrix row %d order mismatch: models=%+v pricing=%+v", i, m, p)
		}
		if m.overwrite == p.overwrite {
			continue
		}
		// Difference is acceptable only for openrouter → discovery.
		if m.existing == SourceOpenRouter && m.incoming == SourceDiscovery {
			if m.overwrite != true || p.overwrite != false {
				t.Errorf("openrouter→discovery: models should overwrite (got %v), pricing should preserve (got %v)",
					m.overwrite, p.overwrite)
			}
			continue
		}
		t.Errorf("unexpected matrix divergence at (%s→%s): models.overwrite=%v pricing.overwrite=%v",
			m.existing, m.incoming, m.overwrite, p.overwrite)
	}
}

// reuse: pin that the matrix uses the canonical source constants and
// hasn't drifted to magic strings — IsValidSource is the source-of-
// truth enum. If anyone adds a new source value without extending the
// matrix, this fails loudly.
func TestPrecedenceMatrix_SourcesAreCanonical(t *testing.T) {
	seen := make(map[string]bool)
	for _, c := range modelsMatrix {
		seen[c.existing] = true
		seen[c.incoming] = true
	}
	expected := ValidSources()
	if len(seen) != len(expected) {
		t.Errorf("matrix covers %d sources, want %d (canonical: %v)",
			len(seen), len(expected), expected)
	}
	for _, s := range expected {
		if !seen[s] {
			t.Errorf("matrix missing source %q — extend modelsMatrix to cover it", s)
		}
		if !IsValidSource(s) {
			t.Errorf("source %q failed IsValidSource — drift between ValidSources() and IsValidSource", s)
		}
	}
}

// TestBulkUpsertPricing_MixedPrecedenceInTx — M2.9 review #2 new-m-1.
// The matrix tests exercise BulkUpsertPricing with len(updates) == 1,
// so a regression that processed only the first row of a multi-row tx
// (e.g., a misplaced `return` or `break`) would not be caught. This test
// seeds three pre-existing rows of mixed sources, issues ONE bulk-upsert
// with three incoming rows whose source-precedence outcomes differ
// (one accept, one reject, one openrouter-blocked-from-discovery), and
// asserts every final row matches the per-row precedence rules. The tx
// itself committing is implicit in the read-back succeeding.
func TestBulkUpsertPricing_MixedPrecedenceInTx(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Three (provider, model_id) pairs, each with a different existing
	// source. FK satisfaction first.
	seedRows := []struct {
		provider, modelID, existing string
		existingInput               float64
	}{
		{"p1", "m1", SourceSeed, 1.0},        // bulk-row will be discovery → ACCEPT
		{"p2", "m2", SourceUser, 10.0},       // bulk-row will be openrouter → REJECT (user wins)
		{"p3", "m3", SourceOpenRouter, 20.0}, // bulk-row will be discovery → REJECT (openrouter blocks discovery)
	}
	for _, r := range seedRows {
		if err := cs.UpsertModel(ctx, Model{
			Provider: r.provider, ModelID: r.modelID, Source: SourceSeed,
		}); err != nil {
			t.Fatalf("seed model %s/%s: %v", r.provider, r.modelID, err)
		}
		if err := cs.UpsertPricing(ctx, r.provider, r.modelID, Pricing{
			Input: r.existingInput, Source: r.existing,
		}); err != nil {
			t.Fatalf("seed pricing %s/%s: %v", r.provider, r.modelID, err)
		}
	}

	// Single bulk tx with three incoming rows. Distinct Input values
	// pin which actually landed.
	const incomingMarker = 999.0
	updates := []PricingUpdate{
		{Provider: "p1", ModelID: "m1", Pricing: Pricing{Input: incomingMarker, Source: SourceDiscovery}},
		{Provider: "p2", ModelID: "m2", Pricing: Pricing{Input: incomingMarker, Source: SourceOpenRouter}},
		{Provider: "p3", ModelID: "m3", Pricing: Pricing{Input: incomingMarker, Source: SourceDiscovery}},
	}
	if err := cs.BulkUpsertPricing(ctx, updates); err != nil {
		t.Fatalf("BulkUpsertPricing: %v", err)
	}

	// Per-row expected outcomes.
	want := []struct {
		provider, modelID string
		expectedInput     float64
		expectedSource    string
		reason            string
	}{
		{"p1", "m1", incomingMarker, SourceDiscovery, "seed → discovery: discovery wins (ADR-2 rule)"},
		{"p2", "m2", 10.0, SourceUser, "user → openrouter: user wins (ADR-2 rule 1)"},
		{"p3", "m3", 20.0, SourceOpenRouter, "openrouter → discovery: openrouter wins (pricing-only rule)"},
	}
	for _, w := range want {
		got, err := cs.GetPricing(ctx, w.provider, w.modelID)
		if err != nil {
			t.Fatalf("GetPricing %s/%s: %v", w.provider, w.modelID, err)
		}
		if got == nil {
			t.Fatalf("GetPricing %s/%s returned nil after bulk tx", w.provider, w.modelID)
		}
		if got.Input != w.expectedInput {
			t.Errorf("%s/%s: Input = %g, want %g (%s)", w.provider, w.modelID, got.Input, w.expectedInput, w.reason)
		}
		if got.Source != w.expectedSource {
			t.Errorf("%s/%s: Source = %q, want %q (%s)", w.provider, w.modelID, got.Source, w.expectedSource, w.reason)
		}
	}
}

// TestValidSources_AgreesWithIsValidSource is the drift-detector
// between the two enum-membership surfaces in types.go/store.go.
// IsValidSource is a hand-written switch (no allocation, hot-path);
// ValidSources allocates a slice for test iteration. Both derive from
// the same Source* constants, but nothing forces them to stay aligned
// — this test does.
func TestValidSources_AgreesWithIsValidSource(t *testing.T) {
	for _, s := range ValidSources() {
		if !IsValidSource(s) {
			t.Errorf("ValidSources() returned %q but IsValidSource(%q) = false", s, s)
		}
	}
	for _, bogus := range []string{"", "Seed", "USER", "openrouter ", "unknown"} {
		if IsValidSource(bogus) {
			t.Errorf("IsValidSource(%q) = true; expected only canonical values", bogus)
		}
	}
}

