package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"sage-router/internal/auth/providers"
)

// openrouterMirrorBaseURL is the URL the mirror lister hits. It's a
// package-private var (not a const) so tests can redirect it at an
// httptest server. Production callers never override it.
//
// PARALLEL-SAFETY: mutations of this var (via t.Cleanup-restored test
// overrides) are NOT safe across t.Parallel boundaries. None of the
// current tests in this package use t.Parallel. If you add t.Parallel
// to any test in this package, refactor the var to a per-call argument
// (e.g., expose ListerCredentials.BaseURL as the source of truth and
// drop the package var entirely).
var openrouterMirrorBaseURL = "https://openrouter.ai/api/v1/models"

// listOpenAIViaOpenRouter fetches OpenAI's catalog VIA OpenRouter's
// public /api/v1/models endpoint. Used as the discovery source for
// OpenAI ChatGPT subscription connections — those tokens cannot read
// api.openai.com/v1/models due to an OAuth scope mismatch (subscription
// tokens lack `api.model.read`). See fix 20260514-openrouter-fallback.
//
// The result is filtered through two layers:
//  1. Namespace: OpenRouter IDs starting with "openai/" only.
//  2. Allowlist: bare-ID must pass providers.ModelInAllowlist against
//     providers.Providers["openai"].SubscriptionAllowedModels — so the
//     catalog mirrors what the route-time selector will actually accept.
//
// Pricing extraction is intentionally OUT of scope. The existing
// OpenRouterRefresher (internal/catalog/openrouter.go, M2.8) already
// pulls pricing from the same upstream into separate
// (provider=openrouter, ...) rows. This lister covers only
// catalog-metadata for (provider=openai, ...) rows.
//
// Provider stamping: rows are stamped `Provider: "openai"` (NOT
// "openrouter") so the source-precedence rule at sqlite.go:84-86 fires
// against existing seed rows for the same (provider, model_id).
//
// Authentication: NONE. OpenRouter's /api/v1/models is public.
// Critical: the request must NOT carry any Authorization header from
// the caller's ListerCredentials — leaking a subscription JWT to
// OpenRouter would be a credential-spill bug.
func listOpenAIViaOpenRouter(ctx context.Context, _ ListerCredentials) ([]Model, error) {
	url := openrouterMirrorBaseURL
	slog.Debug("lister: request URL", "provider", "openai@openrouter-mirror", "url", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("lister openai@openrouter-mirror: build request: %w", err)
	}
	// NO Authorization header — endpoint is public, and adding one
	// would leak the caller's subscription token to OpenRouter.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lister openai@openrouter-mirror: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("lister openai@openrouter-mirror: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, listerStatusError("openai@openrouter-mirror", resp.StatusCode, body)
	}

	// Reuse the same response shape as the openrouter lister — same
	// upstream endpoint, same JSON contract.
	var parsed openrouterModelsResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("lister openai@openrouter-mirror: parse JSON: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, nil
	}

	// Fail-closed if the openai provider config is missing — a
	// nil-or-empty SubscriptionAllowedModels in providers.Providers
	// Cycle 20260517-provider-auth-variants pulled M5.10 forward:
	// providers.Providers["openai"].SubscriptionAllowedModels was
	// REMOVED (backend gates instead of sage-router's static list).
	// The fail-closed guard against an empty allowlist was retired
	// alongside it. This lister now returns ALL openai/* rows from
	// OpenRouter; the route-time selector + backend handle gating.
	//
	// Note: this lister is itself LEGACY — DiscoveryListerKey at
	// internal/catalog/dispatch.go now routes (openai, subscription) to
	// "openai@codex-subscription" instead. The mirror lister stays
	// registered for backward-compat during the M2 migration window
	// (full removal deferred to a future cycle).
	_ = providers.Providers["openai"] // documented dependency on registry shape
	out := make([]Model, 0, len(parsed.Data))
	excluded := 0
	for _, e := range parsed.Data {
		if !strings.HasPrefix(e.ID, "openai/") {
			continue
		}
		bareID := strings.TrimPrefix(e.ID, "openai/")
		out = append(out, Model{
			Provider: "openai", // critical: NOT "openrouter"
			ModelID:  bareID,
			Tier:     openaiTier(bareID), // reuse from lister_openai.go (same package)
			Caps: Capabilities{
				SupportsImages:   openaiSupportsImages(bareID),
				SupportsTools:    true,
				SupportsThinking: strings.HasPrefix(bareID, "o"),
			},
			// DisplayName + context window left at zero — the seed
			// values for these are richer; source-precedence at
			// catalog_models leaves seed-set non-null fields alone
			// when an upsert source row has them as defaults.
		})
	}
	if excluded > 0 {
		slog.Info("lister: openai@openrouter-mirror complete",
			"included", len(out), "excluded_by_allowlist", excluded)
	}
	return out, nil
}
