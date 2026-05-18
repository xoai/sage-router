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

// OpenAI /v1/models — the canonical OpenAI-compatible models endpoint.
// Bearer auth, no capability fields in the response. Tier is inferred
// from the model-ID prefix; capability flags default to true for
// images/tools and false for thinking (OpenAI's o-series models
// support thinking but we don't gate it here — the catalog seed
// row sets supports_thinking correctly for known models, and
// discovery should not overwrite that via a capability column it
// can't determine).
//
// Strictly speaking the OpenAI response doesn't tell us whether a
// model supports images, tools, or thinking. We hardcode reasonable
// defaults per the ID family; if discovery refreshes a row whose
// capability flags were already set (by seed or a richer source like
// OpenRouter), the source-precedence WHERE clauses preserve the
// stronger info.

type openaiModelsResp struct {
	Data []openaiModelEntry `json:"data"`
}

type openaiModelEntry struct {
	ID string `json:"id"`
}

func listOpenAIModels(ctx context.Context, creds ListerCredentials) ([]Model, error) {
	// BaseURL already includes the /v1 segment (config.KnownProviders);
	// append only the resource path. Mirrors the request executor pattern
	// at internal/executor/default.go:42-44. Pre-fix the lister appended
	// "/v1/models" and produced "/v1/v1/models" → 404. See plan
	// 20260514-discovery-url-doubling.
	url := strings.TrimRight(creds.BaseURL, "/") + "/models"
	slog.Debug("lister: request URL", "provider", "openai", "url", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("lister openai: build request: %w", err)
	}
	// Prefer the subscription access token when present; fall back to
	// the API key. Mirrors the executor's auth-type-aware precedence at
	// internal/executor/default.go:55-69 — the docstring contract at
	// listers.go:21-27 promises "AccessToken takes precedence" and this
	// is where it's enforced.
	token := creds.AccessToken
	if token == "" {
		token = creds.APIKey
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range creds.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lister openai: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("lister openai: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, listerStatusError("openai", resp.StatusCode, body)
	}

	var parsed openaiModelsResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("lister openai: parse JSON: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, nil
	}

	out := make([]Model, 0, len(parsed.Data))
	for _, e := range parsed.Data {
		out = append(out, Model{
			Provider: "openai",
			ModelID:  e.ID,
			Tier:     openaiTier(e.ID),
			// OpenAI catalog-level: all current GPT-4+ models support
			// images and tools; o-series adds thinking. Discovery's
			// capability columns are a hint — the source-precedence
			// rules let seed-set capabilities survive if they're richer.
			Caps: Capabilities{
				SupportsImages:   openaiSupportsImages(e.ID),
				SupportsTools:    true,
				SupportsThinking: strings.HasPrefix(e.ID, "o"),
			},
		})
	}
	return out, nil
}

// openaiTier maps an OpenAI model ID to a tier:
//   - gpt-4.1, gpt-4o, o3 (no -mini/-nano suffix) → TierFrontier
//   - *-mini, o-series mini → TierStrong
//   - *-nano → TierEfficient
//   - unknown → TierEfficient (conservative)
func openaiTier(id string) int {
	switch {
	case strings.HasSuffix(id, "-nano"):
		return TierEfficient
	case strings.HasSuffix(id, "-mini"):
		return TierStrong
	case id == "gpt-4.1" || id == "gpt-4o" || id == "o3":
		return TierFrontier
	}
	return TierEfficient
}

// openaiSupportsImages is a small heuristic: vision support on gpt-4*
// and gpt-4o families; nano variants drop it; o-series varies (o3 yes,
// o4-mini yes). When in doubt the seed catalog row remains the
// authority via source precedence.
func openaiSupportsImages(id string) bool {
	if strings.HasSuffix(id, "-nano") {
		return false
	}
	if strings.HasPrefix(id, "gpt-4") || strings.HasPrefix(id, "o3") || strings.HasPrefix(id, "o4") {
		return true
	}
	return false
}
