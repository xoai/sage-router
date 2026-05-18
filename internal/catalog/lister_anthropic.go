package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// Anthropic Models-List endpoint contract — verified against
// platform.claude.com docs on 2026-05-12. Response shape mapped per
// .sage/docs/anthropic-discovery-feasibility.md.
//
// Auth: x-api-key + anthropic-version. Pagination via after_id /
// before_id / limit (1..1000). Capability mapping:
//   - image_input.supported → SupportsImages
//   - thinking.supported    → SupportsThinking
//   - tools                 → hardcoded true (Anthropic models all
//                             support tool use; the API response has
//                             no explicit tools field).
//
// Tier inference uses the prefix-pattern table from the feasibility
// doc: opus / sonnet → TierFrontier, haiku → TierStrong, unknown
// → TierEfficient.

const (
	anthropicAPIVersion    = "2023-06-01"
	// BaseURL already includes /v1 (config.KnownProviders) — append only the
	// resource path. See plan 20260514-discovery-url-doubling.
	anthropicModelsPath    = "/models"
	anthropicModelsPerPage = 1000 // server max — minimises pagination
)

type anthropicModelsResp struct {
	Data    []anthropicModelEntry `json:"data"`
	FirstID string                `json:"first_id"`
	LastID  string                `json:"last_id"`
	HasMore bool                  `json:"has_more"`
}

type anthropicModelEntry struct {
	ID             string                 `json:"id"`
	DisplayName    string                 `json:"display_name"`
	MaxInputTokens int                    `json:"max_input_tokens"`
	MaxTokens      int                    `json:"max_tokens"`
	Capabilities   anthropicCapabilitySet `json:"capabilities"`
}

type anthropicCapabilitySet struct {
	ImageInput struct{ Supported bool } `json:"image_input"`
	Thinking   struct{ Supported bool } `json:"thinking"`
}

// listAnthropicModels queries the Anthropic /v1/models endpoint and
// maps the response into []catalog.Model. Pagination is intentionally
// omitted in M2.2 — the documented limit=1000 covers the current
// Anthropic catalog of <30 models with generous headroom. A
// follow-up task will add cursor pagination if the catalog grows.
//
// Errors are wrapped so the discovery loop can persist them to
// catalog_provider_meta.last_discovery_error verbatim.
func listAnthropicModels(ctx context.Context, creds ListerCredentials) ([]Model, error) {
	url := strings.TrimRight(creds.BaseURL, "/") + anthropicModelsPath +
		fmt.Sprintf("?limit=%d", anthropicModelsPerPage)
	slog.Debug("lister: request URL", "provider", "anthropic", "url", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("lister anthropic: build request: %w", err)
	}
	// Anthropic accepts x-api-key for API-key auth and Authorization:
	// Bearer for OAuth subscription tokens. Mirrors the executor pattern
	// at internal/executor/default.go:55-69. If creds.AccessToken is set
	// it's a subscription connection — use Bearer; otherwise use the
	// classic x-api-key header.
	//
	// R1 (per fix plan): if /v1/models rejects Bearer JWTs (only x-api-key),
	// this fix is no-worse-than-today (an empty x-api-key 401s the same
	// way an unauthenticated Bearer would). Follow-up: flip
	// SubscriptionDiscoverable=false for anthropic if empirically observed.
	if creds.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	} else {
		req.Header.Set("x-api-key", creds.APIKey)
	}
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	for k, v := range creds.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lister anthropic: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("lister anthropic: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, listerStatusError("anthropic", resp.StatusCode, body)
	}

	var parsed anthropicModelsResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("lister anthropic: parse JSON: %w", err)
	}

	if len(parsed.Data) == 0 {
		return nil, nil
	}

	out := make([]Model, 0, len(parsed.Data))
	for _, e := range parsed.Data {
		out = append(out, Model{
			Provider:      "anthropic",
			ModelID:       e.ID,
			DisplayName:   e.DisplayName,
			Tier:          anthropicTier(e.ID),
			ContextWindow: e.MaxInputTokens,
			MaxOutput:     e.MaxTokens,
			Caps: Capabilities{
				SupportsImages:   e.Capabilities.ImageInput.Supported,
				SupportsTools:    true, // hardcoded per feasibility doc
				SupportsThinking: e.Capabilities.Thinking.Supported,
			},
			// Source is left unset — the DiscoveryRunner stamps
			// SourceDiscovery before persisting.
		})
	}
	return out, nil
}

// anthropicTier maps a Claude model ID to a sage-router tier value.
// Heuristic per .sage/docs/anthropic-discovery-feasibility.md:
//   - opus / sonnet → TierFrontier (1)
//   - haiku        → TierStrong   (2)
//   - other        → TierEfficient (3) — conservative default for
//                                       future families
func anthropicTier(modelID string) int {
	switch {
	case strings.Contains(modelID, "opus"):
		return TierFrontier
	case strings.Contains(modelID, "sonnet"):
		return TierFrontier
	case strings.Contains(modelID, "haiku"):
		return TierStrong
	}
	return TierEfficient
}
