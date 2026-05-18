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

// Ollama /api/tags — lists locally-installed models on an Ollama
// server. Always local, no auth header. Every model is TierFree by
// design (Ollama runs on the user's hardware — zero marginal cost).
//
// Model IDs include the tag (e.g., "llama3.3:70b" — "name:tag" form).
// We preserve verbatim because that's the form Ollama uses for both
// /api/tags responses AND /api/generate requests; stripping the tag
// would break routing.

type ollamaTagsResp struct {
	Models []ollamaTagEntry `json:"models"`
}

type ollamaTagEntry struct {
	Name string `json:"name"`
}

func listOllamaModels(ctx context.Context, creds ListerCredentials) ([]Model, error) {
	// BaseURL already includes /api (config.KnownProviders); append
	// only the resource. See plan 20260514-discovery-url-doubling.
	url := strings.TrimRight(creds.BaseURL, "/") + "/tags"
	slog.Debug("lister: request URL", "provider", "ollama", "url", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("lister ollama: build request: %w", err)
	}
	for k, v := range creds.ExtraHeaders {
		req.Header.Set(k, v)
	}
	// Note: NO Authorization header — Ollama is local-only.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lister ollama: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("lister ollama: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, listerStatusError("ollama", resp.StatusCode, body)
	}

	var parsed ollamaTagsResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("lister ollama: parse JSON: %w", err)
	}
	if len(parsed.Models) == 0 {
		return nil, nil
	}

	out := make([]Model, 0, len(parsed.Models))
	for _, e := range parsed.Models {
		out = append(out, Model{
			Provider: "ollama",
			ModelID:  e.Name, // e.g., "llama3.3:70b"
			Tier:     TierFree,
			// Local models — capability flags vary by model family.
			// We don't infer here; seed values stay authoritative.
		})
	}
	return out, nil
}
