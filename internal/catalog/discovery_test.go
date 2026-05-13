package catalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLister adapts a closure into a ModelLister so tests can
// programmatically control returned models / errors per call.
type fakeLister struct {
	call func(ctx context.Context, creds ListerCredentials) ([]Model, error)
}

func (f fakeLister) ListModels(ctx context.Context, creds ListerCredentials) ([]Model, error) {
	return f.call(ctx, creds)
}

// TestDiscoverProvider_PopulatesRows — happy path. The runner calls
// the lister, stamps SourceDiscovery on every model, and upserts
// them into catalog_models. last_discovered_at is set; last_error
// is cleared.
func TestDiscoverProvider_PopulatesRows(t *testing.T) {
	cs, _, ctx := freshStore(t)

	discovered := []Model{
		{Provider: "openai", ModelID: "gpt-4.1", DisplayName: "GPT-4.1", Tier: TierFrontier},
		{Provider: "openai", ModelID: "gpt-4.1-mini", DisplayName: "GPT-4.1 Mini", Tier: TierStrong},
		{Provider: "openai", ModelID: "o3", DisplayName: "o3", Tier: TierFrontier},
	}
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai": fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return discovered, nil
		}},
	})

	res := runner.DiscoverProvider(ctx, "openai", ListerCredentials{BaseURL: "https://x"})
	if res.Err != nil {
		t.Fatalf("DiscoverProvider err: %v", res.Err)
	}
	if res.Count != 3 {
		t.Errorf("Count = %d, want 3", res.Count)
	}

	// All three rows present with SourceDiscovery.
	models, err := cs.ListModels(ctx, "openai")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("upserted %d rows, want 3", len(models))
	}
	for _, m := range models {
		if m.Source != SourceDiscovery {
			t.Errorf("%s: Source = %q, want %q", m.ModelID, m.Source, SourceDiscovery)
		}
	}

	// ProviderMeta updated.
	meta, err := cs.GetProviderMeta(ctx, "openai")
	if err != nil {
		t.Fatalf("GetProviderMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("ProviderMeta nil after discovery")
	}
	if meta.LastDiscoveredAt.IsZero() {
		t.Error("LastDiscoveredAt not set")
	}
	if meta.LastDiscoveryError != "" {
		t.Errorf("LastDiscoveryError = %q, want \"\"", meta.LastDiscoveryError)
	}
}

// TestDiscoverProvider_EmptyListPreservesCatalog — AC14b. Seed N rows,
// run discovery with a lister that returns []. The N rows must remain
// untouched (no delete-not-in-set), AND last_discovery_error must be
// set to "empty model list" so the operator notices.
func TestDiscoverProvider_EmptyListPreservesCatalog(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Pre-seed two rows for the provider.
	for _, m := range []Model{
		{Provider: "openai", ModelID: "existing-1", DisplayName: "Existing 1", Source: SourceSeed},
		{Provider: "openai", ModelID: "existing-2", DisplayName: "Existing 2", Source: SourceSeed},
	} {
		if err := cs.UpsertModel(ctx, m); err != nil {
			t.Fatalf("UpsertModel %s: %v", m.ModelID, err)
		}
	}

	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai": fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return nil, nil // empty list — success-with-zero-models
		}},
	})

	res := runner.DiscoverProvider(ctx, "openai", ListerCredentials{})
	if res.Err != nil {
		t.Fatalf("expected nil err for empty list, got %v", res.Err)
	}
	if res.Count != 0 {
		t.Errorf("Count = %d, want 0", res.Count)
	}

	// Existing rows survive.
	models, _ := cs.ListModels(ctx, "openai")
	if len(models) != 2 {
		t.Errorf("AC14b violation: existing rows deleted (got %d, want 2)", len(models))
	}

	// last_discovery_error signals the empty result.
	meta, _ := cs.GetProviderMeta(ctx, "openai")
	if meta == nil {
		t.Fatal("ProviderMeta nil")
	}
	if meta.LastDiscoveryError != "empty model list" {
		t.Errorf("LastDiscoveryError = %q, want \"empty model list\"", meta.LastDiscoveryError)
	}
	// And the run timestamp IS set — discovery completed without
	// crashing; the empty list is signal, not failure.
	if meta.LastDiscoveredAt.IsZero() {
		t.Error("LastDiscoveredAt should be set on a successful (if empty) run")
	}
}

// TestDiscoverProvider_FailureRecordsError — AC14. When the lister
// returns err, existing rows survive AND the error is persisted to
// last_discovery_error.
func TestDiscoverProvider_FailureRecordsError(t *testing.T) {
	cs, _, ctx := freshStore(t)

	// Pre-seed one row.
	if err := cs.UpsertModel(ctx, Model{
		Provider: "openai", ModelID: "existing", Source: SourceSeed,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}

	const failureMsg = "HTTP 401: unauthorized"
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai": fakeLister{call: func(_ context.Context, _ ListerCredentials) ([]Model, error) {
			return nil, errors.New(failureMsg)
		}},
	})

	res := runner.DiscoverProvider(ctx, "openai", ListerCredentials{})
	if res.Err == nil {
		t.Fatal("expected error, got nil")
	}

	// Existing row survives.
	models, _ := cs.ListModels(ctx, "openai")
	if len(models) != 1 {
		t.Errorf("AC14 violation: rows deleted on failure (got %d, want 1)", len(models))
	}

	// last_discovery_error captured.
	meta, _ := cs.GetProviderMeta(ctx, "openai")
	if meta == nil {
		t.Fatal("ProviderMeta nil")
	}
	if meta.LastDiscoveryError == "" {
		t.Error("LastDiscoveryError not set on failure")
	}
}

// TestDiscoverProvider_UnknownProviderIsError — DiscoverProvider for a
// provider with no registered lister returns an error (not a silent
// no-op). Otherwise a typo in BuiltinListers would silently disable
// discovery for that provider.
func TestDiscoverProvider_UnknownProviderIsError(t *testing.T) {
	cs, _, ctx := freshStore(t)
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{})

	res := runner.DiscoverProvider(ctx, "no-such-provider", ListerCredentials{})
	if res.Err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
}

// TestDiscoverProvider_UnsupportedProviderIsHandledGracefully —
// github-copilot returns ErrDiscoveryUnsupported. The runner must
// NOT treat that as a real failure (it's a contract): no error in
// the result, no last_discovery_error update, the row simply doesn't
// participate. Otherwise repeated 24h ticks would flood
// last_discovery_error with a known-permanent state.
func TestDiscoverProvider_UnsupportedProviderIsHandledGracefully(t *testing.T) {
	cs, _, ctx := freshStore(t)
	runner := NewDiscoveryRunner(cs, BuiltinListers())

	res := runner.DiscoverProvider(ctx, "github-copilot", ListerCredentials{})
	if res.Err != nil {
		t.Errorf("DiscoverProvider should not propagate ErrDiscoveryUnsupported as result.Err, got %v", res.Err)
	}

	// No row written (provider not discoverable).
	meta, _ := cs.GetProviderMeta(ctx, "github-copilot")
	if meta != nil && meta.LastDiscoveryError != "" {
		t.Errorf("LastDiscoveryError = %q for unsupported provider; want \"\"",
			meta.LastDiscoveryError)
	}
}

// TestDiscoverProvider_RespectsTimeout — the configured per-call
// timeout is enforced via context. A slow lister exceeds the
// runner's Timeout and the result carries a ctx-deadline error.
func TestDiscoverProvider_RespectsTimeout(t *testing.T) {
	cs, _, ctx := freshStore(t)
	runner := NewDiscoveryRunner(cs, map[string]ModelLister{
		"openai": fakeLister{call: func(callCtx context.Context, _ ListerCredentials) ([]Model, error) {
			// Block until the runner's timeout fires.
			select {
			case <-callCtx.Done():
				return nil, callCtx.Err()
			case <-time.After(5 * time.Second):
				t.Error("lister was NOT cancelled by runner timeout")
				return nil, nil
			}
		}},
	})
	runner.Timeout = 50 * time.Millisecond

	res := runner.DiscoverProvider(ctx, "openai", ListerCredentials{})
	if res.Err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Errorf("res.Err = %v, want context.DeadlineExceeded", res.Err)
	}
}
