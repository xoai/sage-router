package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenRouter /api/v1/models — the OpenRouter catalog lister.
//
// OpenRouter is a meta-provider: every model is identified by a
// qualified ID like "anthropic/claude-sonnet-4" or
// "meta-llama/llama-3.3-70b-instruct". The lister stamps Provider
// as "openrouter" (NOT the underlying upstream's name) and keeps the
// qualified ID verbatim in ModelID. This is by design — the catalog
// uses (provider, model_id) as the composite key, so an OpenRouter
// row and a direct-provider row for the same model live side by side.
//
// Pricing extraction lives in a separate refresher (M2.8) — this
// lister covers only catalog metadata: model id, display name,
// context window, max output. Tier inference is skipped; OpenRouter
// covers too wide a range of upstreams to give meaningful tiering
// without a lookup table. The seed catalog covers the few OpenRouter
// rows we care about; everything else defaults to TierEfficient.

type openrouterModelsResp struct {
	Data []openrouterModelEntry `json:"data"`
}

type openrouterModelEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	TopProvider   *struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

func listOpenRouterModels(ctx context.Context, creds ListerCredentials) ([]Model, error) {
	url := strings.TrimRight(creds.BaseURL, "/") + "/api/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("lister openrouter: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+creds.APIKey)
	for k, v := range creds.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lister openrouter: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("lister openrouter: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, listerStatusError("openrouter", resp.StatusCode, body)
	}

	var parsed openrouterModelsResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("lister openrouter: parse JSON: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, nil
	}

	out := make([]Model, 0, len(parsed.Data))
	for _, e := range parsed.Data {
		m := Model{
			Provider:      "openrouter",
			ModelID:       e.ID,
			DisplayName:   e.Name,
			Tier:          TierEfficient, // conservative default for the long tail
			ContextWindow: e.ContextLength,
		}
		if e.TopProvider != nil {
			m.MaxOutput = e.TopProvider.MaxCompletionTokens
		}
		// Capabilities are not in the response; the OpenRouter
		// per-row data isn't authoritative for image/tool/thinking
		// support anyway (it depends on the underlying provider).
		// Leave the flags at zero — the catalog source-precedence
		// rules let seed values stand.
		out = append(out, m)
	}
	return out, nil
}
