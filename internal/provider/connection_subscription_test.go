package provider

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"sage-router/internal/auth"
)


func TestTransitionLocked_RoundTrip(t *testing.T) {
	c := NewConnection("c1", "openai", "test", 0, "subscription")
	// Drive Idle → Active via the public method (a Lifecycle facet transition).
	if err := c.MarkUsed(); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if got, want := c.Lifecycle(), LifecycleActive; got != want {
		t.Errorf("after MarkUsed: lifecycle = %v, want %v", got, want)
	}
	// Active → Idle via MarkSuccess (also a facet transition).
	if err := c.MarkSuccess(); err != nil {
		t.Fatalf("MarkSuccess: %v", err)
	}
	if got, want := c.Lifecycle(), LifecycleIdle; got != want {
		t.Errorf("after MarkSuccess: lifecycle = %v, want %v", got, want)
	}
}

func TestTransitionLocked_RejectionReturnsSentinel(t *testing.T) {
	// M1-CO1: rejected transitions return a wrapped ErrTransitionRejected so
	// callers can use errors.Is to distinguish state-machine races from
	// real failures.
	c := NewConnection("c1", "openai", "test", 0, "subscription")
	// MarkRefreshing from a fresh connection is rejected — it requires the
	// Auth facet to be AuthExpired, and a fresh connection is AuthValid.
	err := c.MarkRefreshing()
	if err == nil {
		t.Fatal("MarkRefreshing from Idle should have failed")
	}
	if !errors.Is(err, ErrTransitionRejected) {
		t.Errorf("want errors.Is(err, ErrTransitionRejected); got err=%v", err)
	}
}

func TestInvalidateCredential(t *testing.T) {
	c := NewConnection("c1", "openai", "test", 0, "subscription")
	// Seed cache via the test-only setter.
	c.SetCredentialForTest(&auth.Credential{Provider: "openai", AccessToken: "tok"})
	if got := c.cachedCredential(); got == nil {
		t.Fatal("precondition: cred should be set")
	}
	c.InvalidateCredential()
	if got := c.cachedCredential(); got != nil {
		t.Errorf("after InvalidateCredential, cred = %+v, want nil", got)
	}
}

func TestRecordModelRejection_BlocksFuture(t *testing.T) {
	c := NewConnection("c1", "openai", "test", 0, "subscription")
	if !c.CanServeModel("gpt-5") {
		t.Fatal("precondition: CanServeModel should be true for empty denylist")
	}
	c.RecordModelRejection("gpt-5")
	if c.CanServeModel("gpt-5") {
		t.Error("after RecordModelRejection, CanServeModel(gpt-5) = true, want false")
	}
	// A different model is still allowed.
	if !c.CanServeModel("gpt-4o") {
		t.Error("CanServeModel(gpt-4o) should be true; denylist entry for gpt-5 leaked")
	}
}

func TestCanServeModel_DenylistEntryExpires(t *testing.T) {
	c := NewConnection("c1", "openai", "test", 0, "subscription")
	c.RecordModelRejection("gpt-5")
	// Force the denylist entry into the past to simulate TTL expiry.
	c.mu.Lock()
	c.modelDenylist["gpt-5"] = time.Now().Add(-1 * time.Minute)
	c.mu.Unlock()
	if !c.CanServeModel("gpt-5") {
		t.Error("expired denylist entry should not block; CanServeModel returned false")
	}
}

func TestConnection_ConcurrentDenylistAndFacetRead(t *testing.T) {
	// Race-detector sweep: many goroutines stress the single c.mu.
	c := NewConnection("c1", "openai", "test", 0, "subscription")

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			model := "gpt-5"
			if i%4 == 0 {
				c.RecordModelRejection(model)
			} else if i%4 == 1 {
				_ = c.CanServeModel(model)
			} else if i%4 == 2 {
				_ = c.Lifecycle()
			} else {
				c.InvalidateCredential()
			}
		}(i)
	}
	wg.Wait()
}

func TestModelDenylistAcceptsCustomTTL(t *testing.T) {
	// Sanity: an entry added via the internal addModelDenylistFor helper
	// blocks within its window and clears outside.
	c := NewConnection("c1", "openai", "test", 0, "subscription")
	c.addModelDenylistFor("gpt-5", 50*time.Millisecond)
	if c.CanServeModel("gpt-5") {
		t.Error("CanServeModel returned true inside the denylist window")
	}
	time.Sleep(70 * time.Millisecond)
	if !c.CanServeModel("gpt-5") {
		t.Error("CanServeModel returned false after the denylist window")
	}
}

// AC22: subscription connections are filtered out for models not in their
// allowlist; api-key connections for the same provider continue to serve.
// Test target switched from openai to anthropic in cycle
// 20260517-provider-auth-variants — openai's SubscriptionAllowedModels was
// removed (M5.10 pulled forward to M2); openai subscription connections
// now pass-through any model and rely on backend gate at the codex
// surface. Anthropic still uses the static allowlist.
func TestCanServeModel_SubscriptionAllowlistEnforced(t *testing.T) {
	sub := NewConnection("s1", "anthropic", "Subscription", 0, "subscription")
	apiKey := NewConnection("a1", "anthropic", "ApiKey", 0, "apikey")

	// A model that IS in Anthropic's subscription allowlist (claude-sonnet-4-x).
	if !sub.CanServeModel("claude-sonnet-4-6") {
		t.Error("subscription connection should serve claude-sonnet-4-6 (matches claude-sonnet-4-x wildcard)")
	}
	if !apiKey.CanServeModel("claude-sonnet-4-6") {
		t.Error("apikey connection should always serve any model")
	}

	// A model that is NOT in Anthropic's subscription allowlist
	// (claude-2 is a legacy model not exposed via subscription).
	if sub.CanServeModel("claude-2") {
		t.Error("subscription connection should NOT serve claude-2 (not in allowlist)")
	}
	if !apiKey.CanServeModel("claude-2") {
		t.Error("apikey connection should still serve claude-2 (no subscription restriction)")
	}
}

// TestCanServeModel_OpenAISubscriptionPassesThroughAllModels — pins the
// post-M5.10-pulled-forward behavior: openai subscription has no static
// allowlist; CanServeModel returns true for any model, and the codex
// backend's per-account whitelist gates the actual resolution. M0.8 live
// validation evidence: prolite-tier account accepts gpt-5.4, rejects
// gpt-5.2 — neither was in the previous static allowlist; both should
// now reach the backend for a clean error.
func TestCanServeModel_OpenAISubscriptionPassesThroughAllModels(t *testing.T) {
	sub := NewConnection("s1", "openai", "Subscription", 0, "subscription")
	// gpt-5.4 — M0-validated working model.
	if !sub.CanServeModel("gpt-5.4") {
		t.Error("openai subscription should serve gpt-5.4 (M0-validated; static allowlist removed)")
	}
	// gpt-3.5-turbo-instruct — retired model; backend will reject with a
	// clear error. CanServeModel passes through; the gate is at the
	// upstream backend, not sage-router.
	if !sub.CanServeModel("gpt-3.5-turbo-instruct") {
		t.Error("openai subscription should pass-through any model (backend gates); got CanServeModel=false")
	}
}

func TestCanServeModel_SubscriptionAllowlistRespectsWildcard(t *testing.T) {
	sub := NewConnection("s1", "anthropic", "Subscription", 0, "subscription")
	// Anthropic registry has "claude-sonnet-4-x" — should match dated variants.
	if !sub.CanServeModel("claude-sonnet-4-20251015") {
		t.Error("wildcard claude-sonnet-4-x should match claude-sonnet-4-20251015")
	}
	if sub.CanServeModel("claude-sonnet-3-5-haiku") {
		// Whether this matches depends on the registry. Confirm by reading
		// the registry — if claude-3-5-haiku-latest IS allowed but
		// claude-sonnet-3-5-haiku isn't, this branch should hold.
		t.Error("claude-sonnet-3-5-haiku (no exact or wildcard match) should NOT be served")
	}
}

// AC23 prep: model-level rejection adds the specific model to denylist
// without affecting other models. The denylist takes precedence over the
// allowlist — i.e., an in-allowlist model can still be denied after a 403.
func TestCanServeModel_DenylistOverridesAllowlist(t *testing.T) {
	sub := NewConnection("s1", "openai", "Subscription", 0, "subscription")
	// Verify the allowlist would normally permit this model.
	if !sub.CanServeModel("gpt-5") {
		t.Fatal("precondition: gpt-5 should be in subscription allowlist")
	}
	sub.RecordModelRejection("gpt-5")
	if sub.CanServeModel("gpt-5") {
		t.Error("denylist should override allowlist after a runtime 403")
	}
	// Other allowlist models continue working.
	if !sub.CanServeModel("gpt-4o") {
		t.Error("denylist entry for gpt-5 leaked to gpt-4o")
	}
}

func TestErrTransitionRejected_WrapsReason(t *testing.T) {
	c := NewConnection("c1", "openai", "test", 0, "subscription")
	err := c.MarkRefreshing()
	if err == nil {
		t.Fatal("expected MarkRefreshing from Idle to fail")
	}
	if !strings.Contains(err.Error(), "MarkRefreshing") {
		t.Errorf("wrapped error should mention the calling method; got %q", err.Error())
	}
}
