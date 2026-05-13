package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Gemini /v1beta/models — Google's models-list endpoint.
//
// Auth: API key via `?key=...` query parameter (Google's standard
// AI Studio auth; Vertex AI uses Bearer tokens which we'd handle in
// a separate lister once Vertex support lands).
//
// Response shape: each entry has `name: "models/<bare-id>"` and a
// `supportedGenerationMethods` list. We:
//   - strip the "models/" prefix so the ModelID matches the form
//     sage-router routes use elsewhere (e.g., "gemini-2.5-flash")
//   - filter out non-chat models (no "generateContent" method)
//   - infer tier from the ID: pro → frontier, flash → strong,
//     flash-lite / 2.0-flash / nano → efficient

type geminiModelsResp struct {
	Models []geminiModelEntry `json:"models"`
}

type geminiModelEntry struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	InputTokenLimit            int      `json:"inputTokenLimit"`
	OutputTokenLimit           int      `json:"outputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

func listGeminiModels(ctx context.Context, creds ListerCredentials) ([]Model, error) {
	url := strings.TrimRight(creds.BaseURL, "/") + "/v1beta/models?key=" + creds.APIKey
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("lister gemini: build request: %w", err)
	}
	for k, v := range creds.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lister gemini: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("lister gemini: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, listerStatusError("gemini", resp.StatusCode, body)
	}

	var parsed geminiModelsResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("lister gemini: parse JSON: %w", err)
	}
	if len(parsed.Models) == 0 {
		return nil, nil
	}

	out := make([]Model, 0, len(parsed.Models))
	for _, e := range parsed.Models {
		if !supportsGenerateContent(e.SupportedGenerationMethods) {
			continue // embedding-only / non-chat models excluded
		}
		bareID := strings.TrimPrefix(e.Name, "models/")
		out = append(out, Model{
			Provider:      "gemini",
			ModelID:       bareID,
			DisplayName:   e.DisplayName,
			Tier:          geminiTier(bareID),
			ContextWindow: e.InputTokenLimit,
			MaxOutput:     e.OutputTokenLimit,
			Caps: Capabilities{
				// All Gemini 2.x chat models accept images. Thinking
				// support is model-specific and not in this response
				// — seed values stay authoritative.
				SupportsImages: true,
				SupportsTools:  true,
			},
		})
	}
	return out, nil
}

func supportsGenerateContent(methods []string) bool {
	for _, m := range methods {
		if m == "generateContent" || m == "streamGenerateContent" {
			return true
		}
	}
	return false
}

// geminiTier maps a Gemini model ID (after "models/" stripping) to a
// sage-router tier:
//   - *pro → TierFrontier
//   - *flash (without -lite / 2.0) → TierStrong
//   - flash-lite, 2.0-flash, nano → TierEfficient
//   - unknown → TierEfficient (conservative)
func geminiTier(id string) int {
	switch {
	case strings.Contains(id, "pro"):
		return TierFrontier
	case strings.Contains(id, "flash-lite"):
		return TierEfficient
	case strings.Contains(id, "2.0-flash"):
		return TierEfficient
	case strings.Contains(id, "nano"):
		return TierEfficient
	case strings.Contains(id, "flash"):
		return TierStrong
	}
	return TierEfficient
}
