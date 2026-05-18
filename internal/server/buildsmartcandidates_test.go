package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"sage-router/internal/catalog"
	"sage-router/internal/executor"
	"sage-router/internal/routing"
	"sage-router/internal/store"
)

// Task 1.9-baseline (plan): capture today's buildSmartCandidates
// output as a golden file BEFORE task 1.9a rewires routes_v1.go:787.
// Re-run after the rewire (and again after the M3.4a refit) verifies
// the iteration source change preserves observable output.
//
// The function's natural output is order-non-deterministic today
// (config.ModelCatalog is a Go map). We sort by (Provider, Model) ASC
// before marshalling so the snapshot is stable across runs.
//
// Workflow:
//   - On first run with no golden file: the test writes the file.
//   - On subsequent runs: the test loads the file and asserts equality.
// To regenerate intentionally, delete testdata/buildsmartcandidates_golden.json
// or run with `UPDATE_GOLDEN=1 go test ...`.
func TestBuildSmartCandidates_Golden(t *testing.T) {
	st := buildSmartCandidatesFixture(t)
	cat := buildSmartCandidatesCatalog(t, st)
	s := &Server{
		deps: Dependencies{Store: st, Catalog: cat},
	}

	got := s.buildSmartCandidates(context.Background(), routing.StrategyBalanced)
	sort.Slice(got, func(i, j int) bool {
		if got[i].Provider != got[j].Provider {
			return got[i].Provider < got[j].Provider
		}
		return got[i].Model < got[j].Model
	})

	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}
	gotJSON = append(gotJSON, '\n')

	goldenPath := filepath.Join("testdata", "buildsmartcandidates_golden.json")

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, gotJSON, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("UPDATE_GOLDEN: wrote %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if errors.Is(err, fs.ErrNotExist) {
		// First-run capture: create the file. The next invocation
		// (post-rewire) will load and compare.
		if err := os.WriteFile(goldenPath, gotJSON, 0o644); err != nil {
			t.Fatalf("write initial golden: %v", err)
		}
		t.Logf("captured initial golden snapshot at %s — re-run after rewire to verify", goldenPath)
		return
	}
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	if string(want) != string(gotJSON) {
		// Write the actual output next to the golden for easier diffing.
		actualPath := filepath.Join("testdata", "buildsmartcandidates_actual.json")
		_ = os.WriteFile(actualPath, gotJSON, 0o644)
		t.Errorf("buildSmartCandidates output drift — see %s vs %s\n(diff with: diff %s %s)\n(regenerate with: UPDATE_GOLDEN=1 go test -run TestBuildSmartCandidates_Golden ./internal/server/...)",
			goldenPath, actualPath, goldenPath, actualPath)
	}
}

// buildSmartCandidatesFixture seeds a deterministic set of connections
// covering: two providers, one subscription connection, one disabled
// connection (filter check). The fixture is small and stable; any
// drift in the golden file should reflect actual logic changes, not
// fixture noise.
func buildSmartCandidatesFixture(t *testing.T) store.Store {
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
		{ID: "c1-openai-apikey", Provider: "openai", Name: "openai-1", AuthType: "apikey", APIKey: "sk-test-1", State: "idle"},
		{ID: "c2-anthropic-apikey", Provider: "anthropic", Name: "anthropic-1", AuthType: "apikey", APIKey: "sk-test-2", State: "idle"},
		{ID: "c3-anthropic-sub", Provider: "anthropic", Name: "anthropic-sub", AuthType: "subscription", AccessToken: "sub-token", State: "idle"},
		// Disabled — must be filtered out of activeProviders.
		{ID: "c4-openai-disabled", Provider: "openai", Name: "openai-disabled", AuthType: "apikey", APIKey: "sk-test-4", State: "disabled"},
	}
	for i := range conns {
		if err := st.CreateConnection(&conns[i]); err != nil {
			t.Fatalf("CreateConnection %s: %v", conns[i].ID, err)
		}
	}
	return st
}

// buildSmartCandidatesCatalog seeds the catalog tables from
// config.ModelCatalog + config.KnownProviders (same seeder M1.11 will
// invoke in production bootstrap), then returns a Registry wrapped
// around the same SQLite connection used by the Store fixture. This
// guarantees the post-rewire iteration source has the same data
// as the pre-rewire static config map.
func buildSmartCandidatesCatalog(t *testing.T, st store.Store) catalog.Registry {
	t.Helper()
	cs := catalog.NewSQLiteStore(st.DB())
	if err := catalog.SeedFromConstants(context.Background(), cs); err != nil {
		t.Fatalf("SeedFromConstants: %v", err)
	}
	if err := catalog.SeedProviderMeta(context.Background(), cs); err != nil {
		t.Fatalf("SeedProviderMeta: %v", err)
	}
	return catalog.NewRegistry(cs)
}

// Ensure routing.ModelCandidate stays referenced — the imported
// package is used by buildSmartCandidates's signature.
var _ routing.ModelCandidate

// ────────────────────────────────────────────────────────────────────
// Models Discovery M3.4a — sort + iterate (behavior-equivalent
// enrichment). New tests verify the post-rewrite buildSmartCandidates
// continues to populate the same fields it always did AND that the
// new `pickSampleConnByProvider` helper picks the lowest-ID active
// connection deterministically regardless of ListConnections's
// ordering (NM-r3-6 fix from the plan-review round).
//
// AC10 (no-regression) is covered by the existing
// TestBuildSmartCandidates_Golden above — if the golden file drifts
// after M3.4a, the rewrite changed observable output.
// ────────────────────────────────────────────────────────────────────

// TestBuildSmartCandidates_PopulatesFromCatalog — seed three models
// in the catalog under one provider plus an active connection for
// that provider; assert buildSmartCandidates returns those three
// candidates with provider/model/tier/input_price wired correctly.
func TestBuildSmartCandidates_PopulatesFromCatalog(t *testing.T) {
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := st.CreateConnection(&store.Connection{
		ID: "c-openai", Provider: "openai", Name: "openai-1",
		AuthType: "apikey", APIKey: "sk-test", State: "idle",
	}); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	cs := catalog.NewSQLiteStore(st.DB())
	// Seed exactly three openai models so the test pins a specific
	// candidate count rather than depending on full SeedFromConstants.
	for _, m := range []catalog.Model{
		{Provider: "openai", ModelID: "gpt-4.1", Tier: catalog.TierFrontier,
			Pricing: catalog.Pricing{Input: 5.0, Output: 15.0, Source: catalog.SourceSeed},
			Source: catalog.SourceSeed},
		{Provider: "openai", ModelID: "gpt-4o", Tier: catalog.TierStrong,
			Pricing: catalog.Pricing{Input: 2.5, Output: 10.0, Source: catalog.SourceSeed},
			Source: catalog.SourceSeed},
		{Provider: "openai", ModelID: "gpt-4o-mini", Tier: catalog.TierEfficient,
			Pricing: catalog.Pricing{Input: 0.15, Output: 0.6, Source: catalog.SourceSeed},
			Source: catalog.SourceSeed},
	} {
		if err := cs.UpsertModel(context.Background(), m); err != nil {
			t.Fatalf("UpsertModel %s: %v", m.ModelID, err)
		}
		if err := cs.UpsertPricing(context.Background(), m.Provider, m.ModelID, m.Pricing); err != nil {
			t.Fatalf("UpsertPricing %s: %v", m.ModelID, err)
		}
	}

	s := &Server{deps: Dependencies{Store: st, Catalog: catalog.NewRegistry(cs)}}
	got := s.buildSmartCandidates(context.Background(), routing.StrategyBalanced)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3; got %+v", len(got), got)
	}

	byModel := map[string]routing.ModelCandidate{}
	for _, c := range got {
		byModel[c.Model] = c
	}
	for _, want := range []struct {
		model      string
		tier       int
		inputPrice float64
	}{
		{"gpt-4.1", int(catalog.TierFrontier), 5.0},
		{"gpt-4o", int(catalog.TierStrong), 2.5},
		{"gpt-4o-mini", int(catalog.TierEfficient), 0.15},
	} {
		got, ok := byModel[want.model]
		if !ok {
			t.Errorf("missing candidate for %s", want.model)
			continue
		}
		if got.Provider != "openai" {
			t.Errorf("%s: Provider = %q, want openai", want.model, got.Provider)
		}
		if got.Tier != want.tier {
			t.Errorf("%s: Tier = %d, want %d", want.model, got.Tier, want.tier)
		}
		if got.InputPrice != want.inputPrice {
			t.Errorf("%s: InputPrice = %g, want %g", want.model, got.InputPrice, want.inputPrice)
		}
		// HasSubscriptionConnection must be false — no subscription
		// connection registered.
		if got.HasSubscriptionConnection {
			t.Errorf("%s: HasSubscriptionConnection = true, want false (no subscription connection)",
				want.model)
		}
	}
}

// TestBuildSmartCandidates_SubscriptionAllowlistRespected — AC28.
// A subscription openai connection makes openai candidates
// SUBSCRIPTION-eligible — but only for models in the
// providers.SubscriptionAllowed(openai, ...) static allowlist.
// Models OUTSIDE that allowlist (e.g., openai/gpt-4.1-fictional)
// must have HasSubscriptionConnection=false even though the
// provider has a subscription connection.
func TestBuildSmartCandidates_SubscriptionAllowlistRespected(t *testing.T) {
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// One subscription connection for anthropic — test target switched
	// from openai in cycle 20260517-provider-auth-variants (openai's
	// static SubscriptionAllowedModels was removed; backend gates instead).
	// Anthropic still uses the static allowlist.
	if err := st.CreateConnection(&store.Connection{
		ID: "c-sub", Provider: "anthropic", Name: "anthropic-sub",
		AuthType: "subscription", AccessToken: "sub-token", State: "idle",
	}); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	cs := catalog.NewSQLiteStore(st.DB())
	// Seed one allowlisted model (claude-sonnet-4-x wildcard matches
	// claude-sonnet-4-6) and one NOT-allowlisted (claude-2, legacy).
	for _, m := range []catalog.Model{
		{Provider: "anthropic", ModelID: "claude-sonnet-4-6", Tier: catalog.TierStrong,
			Pricing: catalog.Pricing{Input: 3.0, Source: catalog.SourceSeed},
			Source: catalog.SourceSeed},
		{Provider: "anthropic", ModelID: "claude-2", Tier: catalog.TierFrontier,
			Pricing: catalog.Pricing{Input: 10.0, Source: catalog.SourceSeed},
			Source: catalog.SourceSeed},
	} {
		if err := cs.UpsertModel(context.Background(), m); err != nil {
			t.Fatalf("UpsertModel: %v", err)
		}
		if err := cs.UpsertPricing(context.Background(), m.Provider, m.ModelID, m.Pricing); err != nil {
			t.Fatalf("UpsertPricing: %v", err)
		}
	}

	s := &Server{deps: Dependencies{Store: st, Catalog: catalog.NewRegistry(cs)}}
	got := s.buildSmartCandidates(context.Background(), routing.StrategyBalanced)
	byModel := map[string]routing.ModelCandidate{}
	for _, c := range got {
		byModel[c.Model] = c
	}

	allowlisted, ok := byModel["claude-sonnet-4-6"]
	if !ok {
		t.Fatal("missing claude-sonnet-4-6 candidate")
	}
	if !allowlisted.HasSubscriptionConnection {
		t.Errorf("claude-sonnet-4-6: HasSubscriptionConnection = false, want true (allowlisted via claude-sonnet-4-x wildcard)")
	}

	notAllowlisted, ok := byModel["claude-2"]
	if !ok {
		t.Fatal("missing claude-2 candidate")
	}
	if notAllowlisted.HasSubscriptionConnection {
		t.Errorf("claude-2: HasSubscriptionConnection = true, want false (NOT in anthropic subscription allowlist; AC28)")
	}
}

// TestBuildSmartCandidates_DeterministicSampleConn — NM-r3-6 fix.
// `pickSampleConnByProvider` must return the lowest-ID active
// connection per provider regardless of input order. The test
// shuffles the same fixture N times and asserts the picked sample
// is identical every time.
//
// Why this matters: in M3.4b, the sample connection's cache-hit-rate
// (via Store.GetCacheHitRate) determines the cheap-strategy
// `effectivePrice` ranking. If the sample swings between connections
// from the same provider, the cheap routing rank for identical
// requests becomes non-deterministic — the same model could rank
// first or third on consecutive requests. Deterministic sample
// selection is load-bearing for routing reproducibility.
func TestBuildSmartCandidates_DeterministicSampleConn(t *testing.T) {
	// Three openai connections with non-monotonic IDs. Sorting by ID
	// produces conn-a < conn-b < conn-c — the test asserts conn-a
	// wins regardless of input slice order.
	all := []store.Connection{
		{ID: "conn-b", Provider: "openai", Name: "b", AuthType: "apikey", State: "idle"},
		{ID: "conn-c", Provider: "openai", Name: "c", AuthType: "apikey", State: "idle"},
		{ID: "conn-a", Provider: "openai", Name: "a", AuthType: "apikey", State: "idle"},
	}

	// Enumerate ALL 6 permutations of the 3-element slice. Earlier
	// version used a deterministic in-loop shuffle that only covered
	// 3 of 6 orderings (M3.4a review minor #1). The full enumeration
	// proves what the test docstring claims: independence from input
	// ordering.
	permutations := [][3]int{
		{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0},
	}
	for _, p := range permutations {
		shuffled := []store.Connection{all[p[0]], all[p[1]], all[p[2]]}
		got := pickSampleConnByProvider(shuffled)
		if got["openai"] != "conn-a" {
			t.Errorf("permutation %v: sample for openai = %q, want %q",
				p, got["openai"], "conn-a")
		}
	}

	// A disabled lowest-ID connection MUST NOT be picked — only
	// active connections qualify as the sample. Two sub-cases verify
	// the helper has no positional bias (disabled at start vs middle).
	for _, c := range []struct {
		name string
		seq  []store.Connection
	}{
		{
			name: "disabled at slice position 0",
			seq: []store.Connection{
				{ID: "conn-a-disabled", Provider: "openai", AuthType: "apikey", State: "disabled"},
				{ID: "conn-b", Provider: "openai", AuthType: "apikey", State: "idle"},
				{ID: "conn-c", Provider: "openai", AuthType: "apikey", State: "idle"},
			},
		},
		{
			name: "disabled in middle",
			seq: []store.Connection{
				{ID: "conn-b", Provider: "openai", AuthType: "apikey", State: "idle"},
				{ID: "conn-a-disabled", Provider: "openai", AuthType: "apikey", State: "disabled"},
				{ID: "conn-c", Provider: "openai", AuthType: "apikey", State: "idle"},
			},
		},
	} {
		got := pickSampleConnByProvider(c.seq)
		if got["openai"] != "conn-b" {
			t.Errorf("%s: sample = %q, want conn-b (disabled-lowest skipped)", c.name, got["openai"])
		}
	}

	// An empty input produces an empty map (not nil) — callers can
	// safely index into the result without a nil-check.
	if got := pickSampleConnByProvider(nil); got == nil {
		t.Errorf("nil input: got nil map, want empty map (callers index safely)")
	}
}

// TestBuildSmartCandidates_MixedSubscriptionAndApikey — review minor
// #2. A provider has BOTH a subscription connection AND an apikey
// connection. For a model in the subscription allowlist, the candidate
// gets `HasSubscriptionConnection=true`. For a model OUTSIDE the
// allowlist, the subscription doesn't apply but the candidate is still
// emitted (apikey route works) with `HasSubscriptionConnection=false`.
// This pins the composition contract that AC28 implies but neither
// `_PopulatesFromCatalog` (apikey-only) nor
// `_SubscriptionAllowlistRespected` (sub-only) exercises directly.
func TestBuildSmartCandidates_MixedSubscriptionAndApikey(t *testing.T) {
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// One apikey + one subscription connection, both for anthropic.
	// Switched from openai in cycle 20260517-provider-auth-variants —
	// openai's static SubscriptionAllowedModels was removed; anthropic
	// still has the static allowlist.
	for _, c := range []store.Connection{
		{ID: "c-apikey", Provider: "anthropic", Name: "anthropic-key",
			AuthType: "apikey", APIKey: "sk-test", State: "idle"},
		{ID: "c-sub", Provider: "anthropic", Name: "anthropic-sub",
			AuthType: "subscription", AccessToken: "sub-token", State: "idle"},
	} {
		if err := st.CreateConnection(&c); err != nil {
			t.Fatalf("CreateConnection %s: %v", c.ID, err)
		}
	}

	cs := catalog.NewSQLiteStore(st.DB())
	// One allowlisted (claude-sonnet-4-x matches via wildcard) + one not.
	for _, m := range []catalog.Model{
		{Provider: "anthropic", ModelID: "claude-sonnet-4-6", Tier: catalog.TierStrong,
			Pricing: catalog.Pricing{Input: 3.0, Source: catalog.SourceSeed},
			Source: catalog.SourceSeed},
		{Provider: "anthropic", ModelID: "claude-2", Tier: catalog.TierFrontier,
			Pricing: catalog.Pricing{Input: 10.0, Source: catalog.SourceSeed},
			Source: catalog.SourceSeed},
	} {
		if err := cs.UpsertModel(context.Background(), m); err != nil {
			t.Fatalf("UpsertModel: %v", err)
		}
		if err := cs.UpsertPricing(context.Background(), m.Provider, m.ModelID, m.Pricing); err != nil {
			t.Fatalf("UpsertPricing: %v", err)
		}
	}

	s := &Server{deps: Dependencies{Store: st, Catalog: catalog.NewRegistry(cs)}}
	got := s.buildSmartCandidates(context.Background(), routing.StrategyBalanced)

	byModel := map[string]routing.ModelCandidate{}
	for _, c := range got {
		byModel[c.Model] = c
	}

	// Allowlisted model: subscription applies → HasSubscriptionConnection=true.
	if c, ok := byModel["claude-sonnet-4-6"]; !ok {
		t.Error("claude-sonnet-4-6 candidate missing")
	} else if !c.HasSubscriptionConnection {
		t.Errorf("claude-sonnet-4-6: HasSubscriptionConnection=false, want true (allowlisted via claude-sonnet-4-x wildcard + has subscription connection)")
	}

	// Non-allowlisted model: subscription doesn't apply BUT the
	// candidate is still emitted via the apikey connection.
	// HasSubscriptionConnection must be false.
	if c, ok := byModel["claude-2"]; !ok {
		t.Error("claude-2 candidate missing — apikey route should still emit it")
	} else if c.HasSubscriptionConnection {
		t.Errorf("claude-2: HasSubscriptionConnection=true, want false (not in anthropic allowlist; apikey route only)")
	}
}

// ────────────────────────────────────────────────────────────────────
// Models Discovery M3.4b — capability override + cache-hit-rate
// enrichment.
//
// Three tests cover:
//  1. CapabilityOverride — an Executor implementing
//     CapabilityOverrider flips a flag the catalog says is false.
//  2. CachedRatioQueried — for StrategyCheap on a model with
//     CacheRead > 0, GetCacheHitRate is invoked and CachedRatio is
//     populated on the candidate.
//  3. CachedRatioSkippedNonCheap — for non-cheap strategies, the
//     query is NOT issued (CachedRatio stays 0) even when the same
//     usage_log seed is in place.
// ────────────────────────────────────────────────────────────────────

// fakeOverrideExecutor implements both Executor and CapabilityOverrider.
// Override flips SupportsThinking for the configured model only; all
// other fields pass through unchanged. The Execute method is unused in
// these tests (buildSmartCandidates never calls Execute), but the
// interface requires it.
type fakeOverrideExecutor struct {
	provider     string
	overrideFor  string
	flipThinking bool
}

func (f *fakeOverrideExecutor) Provider() string { return f.provider }
func (f *fakeOverrideExecutor) Execute(_ context.Context, _ *executor.ExecuteRequest) (*executor.Result, error) {
	return &executor.Result{StatusCode: http.StatusOK, Body: io.NopCloser(nil)}, nil
}
func (f *fakeOverrideExecutor) OverrideCapabilities(model string, base executor.Capabilities) executor.Capabilities {
	if model == f.overrideFor && f.flipThinking {
		base.SupportsThinking = !base.SupportsThinking
	}
	return base
}

// TestBuildSmartCandidates_CapabilityOverride — when an Executor
// implements CapabilityOverrider, buildSmartCandidates calls
// OverrideCapabilities(modelID, base) and the returned struct
// REPLACES the catalog's capability flags on the candidate. Pin: the
// catalog says SupportsThinking=false; the override flips it to true.
func TestBuildSmartCandidates_CapabilityOverride(t *testing.T) {
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := st.CreateConnection(&store.Connection{
		ID: "c-openai", Provider: "openai", Name: "openai-1",
		AuthType: "apikey", APIKey: "sk-test", State: "idle",
	}); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	cs := catalog.NewSQLiteStore(st.DB())
	// Catalog row: SupportsThinking explicitly FALSE. Override must
	// flip it for this exact model.
	if err := cs.UpsertModel(context.Background(), catalog.Model{
		Provider: "openai", ModelID: "gpt-thinking-future", Tier: catalog.TierFrontier,
		Caps:    catalog.Capabilities{SupportsTools: true, SupportsThinking: false},
		Pricing: catalog.Pricing{Input: 1.0, Source: catalog.SourceSeed},
		Source:  catalog.SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := cs.UpsertPricing(context.Background(), "openai", "gpt-thinking-future",
		catalog.Pricing{Input: 1.0, Source: catalog.SourceSeed}); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	// Also seed a SECOND openai model without thinking — to confirm
	// the override is per-model, not per-provider.
	if err := cs.UpsertModel(context.Background(), catalog.Model{
		Provider: "openai", ModelID: "gpt-no-thinking", Tier: catalog.TierStrong,
		Caps:    catalog.Capabilities{SupportsThinking: false},
		Pricing: catalog.Pricing{Input: 0.5, Source: catalog.SourceSeed},
		Source:  catalog.SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel sibling: %v", err)
	}
	if err := cs.UpsertPricing(context.Background(), "openai", "gpt-no-thinking",
		catalog.Pricing{Input: 0.5, Source: catalog.SourceSeed}); err != nil {
		t.Fatalf("UpsertPricing sibling: %v", err)
	}

	variants := executor.NewVariants()
	variants.Register(
		executor.VariantKey{Provider: "openai", AuthType: ""},
		&fakeOverrideExecutor{provider: "openai", overrideFor: "gpt-thinking-future", flipThinking: true},
	)
	s := &Server{deps: Dependencies{
		Store:    st,
		Catalog:  catalog.NewRegistry(cs),
		Variants: variants,
	}}
	got := s.buildSmartCandidates(context.Background(), routing.StrategyBalanced)

	byModel := map[string]routing.ModelCandidate{}
	for _, c := range got {
		byModel[c.Model] = c
	}

	flipped, ok := byModel["gpt-thinking-future"]
	if !ok {
		t.Fatal("missing gpt-thinking-future candidate")
	}
	if !flipped.SupportsThinking {
		t.Errorf("gpt-thinking-future: SupportsThinking=false, want true (executor override flipped catalog flag)")
	}

	sibling, ok := byModel["gpt-no-thinking"]
	if !ok {
		t.Fatal("missing gpt-no-thinking candidate")
	}
	if sibling.SupportsThinking {
		t.Errorf("gpt-no-thinking: SupportsThinking=true, want false (override is per-model; sibling left untouched)")
	}
}

// TestBuildSmartCandidates_CapabilityOverride_ThroughRetryWrap — production
// wires every executor in `executor.NewRetryExecutor(inner, cfg)`. Without
// RetryExecutor's OverrideCapabilities delegation, the type-assertion in
// buildSmartCandidates would silently miss the override on inner executors.
// This test exercises the full production wrap path to pin the contract.
func TestBuildSmartCandidates_CapabilityOverride_ThroughRetryWrap(t *testing.T) {
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := st.CreateConnection(&store.Connection{
		ID: "c-openai", Provider: "openai", AuthType: "apikey", APIKey: "k", State: "idle",
	}); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	cs := catalog.NewSQLiteStore(st.DB())
	if err := cs.UpsertModel(context.Background(), catalog.Model{
		Provider: "openai", ModelID: "gpt-future", Tier: catalog.TierFrontier,
		Caps:    catalog.Capabilities{SupportsThinking: false},
		Pricing: catalog.Pricing{Input: 1.0, Source: catalog.SourceSeed},
		Source:  catalog.SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := cs.UpsertPricing(context.Background(), "openai", "gpt-future",
		catalog.Pricing{Input: 1.0, Source: catalog.SourceSeed}); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	// Wrap the fake in RetryExecutor — mirroring main.go's production
	// wiring (cmd/sage-router/main.go: deps.Executors[provider] =
	// executor.NewRetryExecutor(inner, retryCfg)).
	inner := &fakeOverrideExecutor{provider: "openai", overrideFor: "gpt-future", flipThinking: true}
	wrapped := executor.NewRetryExecutor(inner, executor.RetryConfig{})

	variants := executor.NewVariants()
	variants.Register(executor.VariantKey{Provider: "openai", AuthType: ""}, wrapped)
	s := &Server{deps: Dependencies{
		Store:    st,
		Catalog:  catalog.NewRegistry(cs),
		Variants: variants,
	}}
	got := s.buildSmartCandidates(context.Background(), routing.StrategyBalanced)
	if len(got) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(got))
	}
	if !got[0].SupportsThinking {
		t.Errorf("through-retry-wrap: SupportsThinking=false, want true "+
			"(RetryExecutor.OverrideCapabilities must delegate to inner; got candidate %+v)",
			got[0])
	}
}

// TestBuildSmartCandidates_CapabilityOverride_ExecutorNotImplementer
// — when the executor doesn't implement CapabilityOverrider, the
// type-assertion short-circuits and the catalog's flags pass through
// untouched. Also covers the case where deps.Executors has no entry
// for the provider at all.
func TestBuildSmartCandidates_CapabilityOverride_ExecutorNotImplementer(t *testing.T) {
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := st.CreateConnection(&store.Connection{
		ID: "c-openai", Provider: "openai", AuthType: "apikey", APIKey: "k", State: "idle",
	}); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	cs := catalog.NewSQLiteStore(st.DB())
	if err := cs.UpsertModel(context.Background(), catalog.Model{
		Provider: "openai", ModelID: "gpt-img", Tier: catalog.TierStrong,
		Caps:    catalog.Capabilities{SupportsImages: true, SupportsTools: true},
		Pricing: catalog.Pricing{Input: 1.0, Source: catalog.SourceSeed},
		Source:  catalog.SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := cs.UpsertPricing(context.Background(), "openai", "gpt-img",
		catalog.Pricing{Input: 1.0, Source: catalog.SourceSeed}); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	// Variants registry intentionally empty — no entry for openai, so
	// the override path must short-circuit cleanly without blowing up.
	s := &Server{deps: Dependencies{
		Store:    st,
		Catalog:  catalog.NewRegistry(cs),
		Variants: executor.NewVariants(),
	}}
	got := s.buildSmartCandidates(context.Background(), routing.StrategyBalanced)
	if len(got) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(got))
	}
	c := got[0]
	if !c.SupportsImages || !c.SupportsTools {
		t.Errorf("catalog flags lost: SupportsImages=%v SupportsTools=%v, want both true",
			c.SupportsImages, c.SupportsTools)
	}
	if c.SupportsThinking {
		t.Errorf("SupportsThinking=true unexpected — catalog had false and no override registered")
	}
}

// TestBuildSmartCandidates_CachedRatioQueried — for StrategyCheap on a
// model with non-zero CacheRead pricing, buildSmartCandidates calls
// Store.GetCacheHitRate(ctx, sampleConn, 24h) and populates
// CachedRatio. Seeded usage_log rows produce a known ratio.
func TestBuildSmartCandidates_CachedRatioQueried(t *testing.T) {
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// One openai connection.
	if err := st.CreateConnection(&store.Connection{
		ID: "c-openai-1", Provider: "openai", Name: "openai-1",
		AuthType: "apikey", APIKey: "sk-test", State: "idle",
	}); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}

	// Seed usage_log: 100 input + 50 cache_read → ratio = 50/150 = 0.333...
	if err := st.RecordUsage(&store.UsageEntry{
		ID: "u1", RequestID: "r1", Provider: "openai", Model: "gpt-cheap",
		ConnectionID: "c-openai-1", InputTokens: 100, OutputTokens: 10,
		CacheReadTokens: 50, Status: "ok",
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	cs := catalog.NewSQLiteStore(st.DB())
	// Model has CacheRead > 0 — gates the cache-hit-rate query on.
	if err := cs.UpsertModel(context.Background(), catalog.Model{
		Provider: "openai", ModelID: "gpt-cheap", Tier: catalog.TierEfficient,
		Pricing: catalog.Pricing{Input: 1.0, CacheRead: 0.1, Source: catalog.SourceSeed},
		Source:  catalog.SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := cs.UpsertPricing(context.Background(), "openai", "gpt-cheap",
		catalog.Pricing{Input: 1.0, CacheRead: 0.1, Source: catalog.SourceSeed}); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	s := &Server{deps: Dependencies{Store: st, Catalog: catalog.NewRegistry(cs)}}
	got := s.buildSmartCandidates(context.Background(), routing.StrategyCheap)
	if len(got) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(got))
	}
	c := got[0]
	if c.CacheReadPrice != 0.1 {
		t.Errorf("CacheReadPrice = %v, want 0.1 (copied from catalog)", c.CacheReadPrice)
	}
	want := 50.0 / 150.0
	if c.CachedRatio < want-1e-9 || c.CachedRatio > want+1e-9 {
		t.Errorf("CachedRatio = %v, want %v (50 cache_read / (100 input + 50 cache_read))",
			c.CachedRatio, want)
	}
}

// TestBuildSmartCandidates_CachedRatioSkippedNonCheap — same seed as
// above but invoked with StrategyBalanced. CachedRatio must stay 0
// because the query is gated on Strategy == StrategyCheap. This
// guards against the query firing unconditionally — a wasted SQL hit
// per candidate per balanced/fast/best request would be significant.
func TestBuildSmartCandidates_CachedRatioSkippedNonCheap(t *testing.T) {
	for _, strat := range []routing.Strategy{
		routing.StrategyBalanced, routing.StrategyFast, routing.StrategyBest,
	} {
		t.Run(string(strat), func(t *testing.T) {
			st, err := store.NewSQLiteStore(":memory:")
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { st.Close() })
			if err := st.Migrate(); err != nil {
				t.Fatalf("migrate: %v", err)
			}

			if err := st.CreateConnection(&store.Connection{
				ID: "c-openai-1", Provider: "openai", AuthType: "apikey",
				APIKey: "sk-test", State: "idle",
			}); err != nil {
				t.Fatalf("CreateConnection: %v", err)
			}
			if err := st.RecordUsage(&store.UsageEntry{
				ID: "u1", RequestID: "r1", Provider: "openai", Model: "gpt-cheap",
				ConnectionID: "c-openai-1", InputTokens: 100, OutputTokens: 10,
				CacheReadTokens: 50, Status: "ok",
			}); err != nil {
				t.Fatalf("RecordUsage: %v", err)
			}

			cs := catalog.NewSQLiteStore(st.DB())
			if err := cs.UpsertModel(context.Background(), catalog.Model{
				Provider: "openai", ModelID: "gpt-cheap", Tier: catalog.TierEfficient,
				Pricing: catalog.Pricing{Input: 1.0, CacheRead: 0.1, Source: catalog.SourceSeed},
				Source:  catalog.SourceSeed,
			}); err != nil {
				t.Fatalf("UpsertModel: %v", err)
			}
			if err := cs.UpsertPricing(context.Background(), "openai", "gpt-cheap",
				catalog.Pricing{Input: 1.0, CacheRead: 0.1, Source: catalog.SourceSeed}); err != nil {
				t.Fatalf("UpsertPricing: %v", err)
			}

			s := &Server{deps: Dependencies{Store: st, Catalog: catalog.NewRegistry(cs)}}
			got := s.buildSmartCandidates(context.Background(), strat)
			if len(got) != 1 {
				t.Fatalf("len(candidates) = %d, want 1", len(got))
			}
			c := got[0]
			// CacheReadPrice should still be copied (it's additive and
			// cheap (no pun) to populate; the *active use* is gated by
			// CachedRatio > 0 in effectivePrice).
			if c.CacheReadPrice != 0.1 {
				t.Errorf("CacheReadPrice = %v, want 0.1 (always copied)", c.CacheReadPrice)
			}
			if c.CachedRatio != 0 {
				t.Errorf("CachedRatio = %v, want 0 (strategy %s should not trigger GetCacheHitRate)",
					c.CachedRatio, strat)
			}
		})
	}
}

// cacheHitRateSpy wraps a real store.Store and counts
// GetCacheHitRate invocations per connectionID. Used to assert the
// memoization contract in buildSmartCandidates — under StrategyCheap
// with N catalog models sharing a connection, the query should fire
// exactly ONCE for that connection regardless of N.
type cacheHitRateSpy struct {
	store.Store
	mu    sync.Mutex
	calls map[string]int
}

func (s *cacheHitRateSpy) GetCacheHitRate(ctx context.Context, connectionID string, lookback time.Duration) (float64, error) {
	s.mu.Lock()
	s.calls[connectionID]++
	s.mu.Unlock()
	return s.Store.GetCacheHitRate(ctx, connectionID, lookback)
}

// TestBuildSmartCandidates_CachedRatioMemoizedPerConn — post-review
// fix. Under StrategyCheap, N models for the same provider must
// trigger exactly one GetCacheHitRate query per connection (not N).
// Without memoization, a 50-model OpenRouter catalog with cache
// pricing would issue 50 redundant SQL hits per cheap request.
func TestBuildSmartCandidates_CachedRatioMemoizedPerConn(t *testing.T) {
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := st.CreateConnection(&store.Connection{
		ID: "c-openai-1", Provider: "openai", AuthType: "apikey", APIKey: "k", State: "idle",
	}); err != nil {
		t.Fatalf("CreateConnection openai: %v", err)
	}
	if err := st.CreateConnection(&store.Connection{
		ID: "c-anthropic-1", Provider: "anthropic", AuthType: "apikey", APIKey: "k", State: "idle",
	}); err != nil {
		t.Fatalf("CreateConnection anthropic: %v", err)
	}
	if err := st.RecordUsage(&store.UsageEntry{
		ID: "u1", RequestID: "r1", Provider: "openai", Model: "gpt-a",
		ConnectionID: "c-openai-1", InputTokens: 100, CacheReadTokens: 50, Status: "ok",
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	cs := catalog.NewSQLiteStore(st.DB())
	// 4 openai models with non-zero CacheRead — all should hit the
	// SAME sample connection (c-openai-1) and the SAME cached ratio
	// from the memo. Plus 2 anthropic models — different connID, so
	// one additional query for that provider.
	models := []catalog.Model{
		{Provider: "openai", ModelID: "gpt-a", Tier: catalog.TierStrong,
			Pricing: catalog.Pricing{Input: 1.0, CacheRead: 0.1, Source: catalog.SourceSeed}, Source: catalog.SourceSeed},
		{Provider: "openai", ModelID: "gpt-b", Tier: catalog.TierStrong,
			Pricing: catalog.Pricing{Input: 1.0, CacheRead: 0.1, Source: catalog.SourceSeed}, Source: catalog.SourceSeed},
		{Provider: "openai", ModelID: "gpt-c", Tier: catalog.TierStrong,
			Pricing: catalog.Pricing{Input: 1.0, CacheRead: 0.1, Source: catalog.SourceSeed}, Source: catalog.SourceSeed},
		{Provider: "openai", ModelID: "gpt-d", Tier: catalog.TierStrong,
			Pricing: catalog.Pricing{Input: 1.0, CacheRead: 0.1, Source: catalog.SourceSeed}, Source: catalog.SourceSeed},
		{Provider: "anthropic", ModelID: "claude-x", Tier: catalog.TierFrontier,
			Pricing: catalog.Pricing{Input: 3.0, CacheRead: 0.3, Source: catalog.SourceSeed}, Source: catalog.SourceSeed},
		{Provider: "anthropic", ModelID: "claude-y", Tier: catalog.TierStrong,
			Pricing: catalog.Pricing{Input: 3.0, CacheRead: 0.3, Source: catalog.SourceSeed}, Source: catalog.SourceSeed},
	}
	for _, m := range models {
		if err := cs.UpsertModel(context.Background(), m); err != nil {
			t.Fatalf("UpsertModel %s: %v", m.ModelID, err)
		}
		if err := cs.UpsertPricing(context.Background(), m.Provider, m.ModelID, m.Pricing); err != nil {
			t.Fatalf("UpsertPricing %s: %v", m.ModelID, err)
		}
	}

	spy := &cacheHitRateSpy{Store: st, calls: map[string]int{}}
	s := &Server{deps: Dependencies{Store: spy, Catalog: catalog.NewRegistry(cs)}}
	got := s.buildSmartCandidates(context.Background(), routing.StrategyCheap)

	if len(got) != 6 {
		t.Fatalf("len(candidates) = %d, want 6", len(got))
	}
	// Memo contract — exactly one query per connection, not per model.
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.calls["c-openai-1"] != 1 {
		t.Errorf("GetCacheHitRate calls for c-openai-1 = %d, want 1 (4 openai models must share one query via memo)",
			spy.calls["c-openai-1"])
	}
	if spy.calls["c-anthropic-1"] != 1 {
		t.Errorf("GetCacheHitRate calls for c-anthropic-1 = %d, want 1 (2 anthropic models must share one query)",
			spy.calls["c-anthropic-1"])
	}
	totalCalls := 0
	for _, n := range spy.calls {
		totalCalls += n
	}
	if totalCalls != 2 {
		t.Errorf("total GetCacheHitRate calls = %d, want 2 (one per provider — memo must collapse the 6-model loop)",
			totalCalls)
	}
}

// TestBuildSmartCandidates_CachedRatioSkippedZeroCacheReadPrice —
// even under StrategyCheap, if the model has CacheRead = 0 (provider
// doesn't price cache reads separately, or the seed hasn't filled
// the value), the GetCacheHitRate query is skipped. Verifies the
// `m.Pricing.CacheRead > 0` short-circuit per plan §3.4b.
func TestBuildSmartCandidates_CachedRatioSkippedZeroCacheReadPrice(t *testing.T) {
	st, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.CreateConnection(&store.Connection{
		ID: "c-openai-1", Provider: "openai", AuthType: "apikey", APIKey: "k", State: "idle",
	}); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if err := st.RecordUsage(&store.UsageEntry{
		ID: "u1", RequestID: "r1", Provider: "openai", Model: "gpt-no-cache",
		ConnectionID: "c-openai-1", InputTokens: 100, OutputTokens: 10,
		CacheReadTokens: 50, Status: "ok",
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	cs := catalog.NewSQLiteStore(st.DB())
	if err := cs.UpsertModel(context.Background(), catalog.Model{
		Provider: "openai", ModelID: "gpt-no-cache", Tier: catalog.TierStrong,
		Pricing: catalog.Pricing{Input: 1.0, CacheRead: 0, Source: catalog.SourceSeed},
		Source:  catalog.SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	if err := cs.UpsertPricing(context.Background(), "openai", "gpt-no-cache",
		catalog.Pricing{Input: 1.0, CacheRead: 0, Source: catalog.SourceSeed}); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	s := &Server{deps: Dependencies{Store: st, Catalog: catalog.NewRegistry(cs)}}
	got := s.buildSmartCandidates(context.Background(), routing.StrategyCheap)
	if len(got) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(got))
	}
	c := got[0]
	if c.CachedRatio != 0 {
		t.Errorf("CachedRatio = %v, want 0 (CacheRead=0 should skip GetCacheHitRate even under StrategyCheap)",
			c.CachedRatio)
	}
}
