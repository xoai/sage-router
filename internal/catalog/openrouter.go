package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OpenRouterRefresher pulls https://openrouter.ai/api/v1/models on a
// schedule and persists per-model pricing under
// (provider='openrouter', model_id=<qualified>) with source='openrouter'.
//
// Conflict rules (enforced by Store.UpsertPricing's WHERE clause):
//   - user > openrouter — user pricing overrides survive.
//   - openrouter > discovery > seed — refresher wins over the seed
//     and over any per-provider discovery's pricing guesses.
//
// Catalog model rows are written FIRST so the catalog_pricing FK is
// satisfied. Capabilities (images/tools/thinking) are NOT inferred
// from the OpenRouter response; per ADR-3 §Part C openrouter rows
// keep zero-value capability flags by design.
//
// Bootstrap dependency: the `settings.openrouter_refresh_enabled` row
// is INSERTed by cmd/sage-router/catalog_wire.go (M1.11/Pm2). The
// refresher itself reads neither settings nor the DB at construction
// time — the main.go bootstrapper toggles whether Start runs.
type OpenRouterRefresher struct {
	Store    Store
	Registry Registry
	URL      string       // default "https://openrouter.ai/api/v1/models"
	Client   *http.Client // default http.DefaultClient (15s timeout recommended)
}

const (
	defaultOpenRouterURL = "https://openrouter.ai/api/v1/models"

	// openRouterBodyCap mirrors the lister limit (M2.2). 10 MiB
	// comfortably accommodates the ~432 KB fixture observed at
	// 365 models with room for catalog growth.
	openRouterBodyCap = 10 * 1024 * 1024

	// perTokenToPerMillion converts OpenRouter's "USD per token"
	// pricing strings into the per-1M-token floats sage-router's
	// catalog_pricing stores. The seed catalog uses the same units.
	perTokenToPerMillion = 1_000_000.0
)

// FetchAndPersist downloads OpenRouter's catalog, parses each model
// entry's pricing, and upserts to (catalog_models, catalog_pricing).
// Returns the number of pricing rows persisted (NOT the number of
// rows attempted — the precedence WHERE clause may have skipped some
// when a user override or higher-precedence source was already
// present).
//
// Errors from the network or parser are returned; partial writes are
// minimized but not strictly atomic across the (models, pricing) pair
// because Store.UpsertModel is a per-row call. On failure mid-loop,
// previously-written rows stay — the next successful refresh will
// reconcile.
func (o *OpenRouterRefresher) FetchAndPersist(ctx context.Context) (int, error) {
	url := o.URL
	if url == "" {
		url = defaultOpenRouterURL
	}
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("openrouter: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("openrouter: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, openRouterBodyCap))
	if err != nil {
		return 0, fmt.Errorf("openrouter: read body: %w", err)
	}
	// io.LimitReader silently truncates at the cap. If the upstream
	// payload grew past 10 MiB, json.Unmarshal will fail downstream
	// with "unexpected end of JSON input" — an opaque error that's
	// indistinguishable from a malformed response. Surface the
	// truncation explicitly so the failure mode is debuggable from
	// logs alone. The == comparison is safe: a body shorter than the
	// cap reads its real length; an at-cap body either fits exactly
	// (rare but possible) or was truncated (the common case for the
	// diagnostic to fire).
	if len(body) == openRouterBodyCap {
		slog.Warn("openrouter response hit body cap; payload may be truncated",
			"size", len(body),
			"cap", openRouterBodyCap)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("openrouter: status %d: %s", resp.StatusCode, snippet(body))
	}

	updates, err := parseOpenRouterPricing(body)
	if err != nil {
		return 0, err
	}
	if len(updates) == 0 {
		return 0, nil
	}

	// Upsert model rows BEFORE pricing rows so the FK is satisfied for
	// the bulk pricing tx. Per ADR-3 §Part C this is the safe
	// ordering; the model rows have zero-default capability flags and
	// rely on direct-provider discovery / seed for the authoritative
	// capability values.
	//
	// Trade-off vs. ADR-3 "one transaction per model": the
	// implementation does N standalone UpsertModel calls (each is its
	// own implicit tx in SQLite) followed by one BulkUpsertPricing tx.
	// A failure mid-loop or in the pricing tx leaves the (catalog_models,
	// catalog_pricing) source pair inconsistent until the next 24h
	// refresh reconciles. The single-user / 24h-cadence design makes
	// the window acceptable; tightening it to one combined tx is a
	// future Store-interface change tracked as M2-close carry-over.
	//
	// Registry invalidation: any successful write (model or pricing)
	// mutates the catalog the Registry caches. Use a defer so partial-
	// write paths still invalidate. See fix 20260514-pricing-mirror —
	// Stage 5 below ensures mirror-only refreshes (no new openrouter
	// model rows; only pricing rows for existing direct-provider rows)
	// also trip this defer.
	var anyWritten bool
	defer func() {
		if anyWritten && o.Registry != nil {
			o.Registry.Invalidate()
		}
	}()

	// Stage 1: separate openrouter-namespace entries from mirror
	// candidates. Only openrouter entries get UpsertModel calls —
	// mirror candidates target direct-provider catalog_models rows
	// that already exist (and have richer capability flags from the
	// direct discovery lister; clobbering with source=openrouter would
	// zero those out via UPSERT). See fix 20260514-pricing-mirror.
	var openrouterEntries []PricingUpdate
	var mirrorCandidates []PricingUpdate
	for _, u := range updates {
		if u.Provider == "openrouter" {
			openrouterEntries = append(openrouterEntries, u)
		} else {
			mirrorCandidates = append(mirrorCandidates, u)
		}
	}

	// Stage 2: write openrouter model rows. Mirror candidates do NOT
	// pass through this loop — their direct-provider model rows are
	// already present via discovery, and writing source=openrouter
	// there would clobber discovery's capability flags.
	for _, u := range openrouterEntries {
		m := Model{
			Provider: u.Provider,
			ModelID:  u.ModelID,
			Source:   SourceOpenRouter,
		}
		if err := o.Store.UpsertModel(ctx, m); err != nil {
			return 0, fmt.Errorf("openrouter: upsert model %s/%s: %w", u.Provider, u.ModelID, err)
		}
		anyWritten = true
	}

	// Stage 3: resolve mirror candidates against existing
	// catalog_models rows via normalized-ID matching. Anthropic IDs
	// need dot→dash + date-suffix strip (see openrouter_normalize.go).
	// Pre-fetch is one SQL query per refresh.
	var resolvedMirror []PricingUpdate
	if len(mirrorCandidates) > 0 {
		candidateProviders := uniqueProviders(mirrorCandidates)
		existing, listErr := o.Store.ListModelIDsForProviders(ctx, candidateProviders)
		if listErr != nil {
			slog.Warn("openrouter: mirror prefetch failed; skipping mirror writes",
				"err", listErr, "providers", candidateProviders)
		} else {
			resolvedMirror = resolveMirrorCandidates(mirrorCandidates, existing)
		}
	}

	// Stage 4: bulk pricing write — openrouter rows + resolved mirror
	// rows. Both carry source=openrouter; ADR-2 pricing precedence
	// (user > openrouter > discovery > seed) handles the override
	// correctly. Mirror rows that don't match (e.g., gemini openrouter
	// IDs when no gemini connection exists) were filtered out at
	// Stage 3, so no FK violations here.
	allPricing := append(openrouterEntries, resolvedMirror...)
	if err := o.Store.BulkUpsertPricing(ctx, allPricing); err != nil {
		return 0, fmt.Errorf("openrouter: bulk upsert pricing: %w", err)
	}

	// Stage 5: ensure Registry cache invalidates on mirror-only
	// refreshes (no openrouter model rows written, but pricing rows
	// were written for existing direct-provider rows). The defer at
	// Stage 0 already fires when Stage 2 set anyWritten=true; this
	// extends the trigger to pricing-only writes too.
	if len(resolvedMirror) > 0 {
		anyWritten = true
	}

	return len(allPricing), nil
}

// Start spawns a background goroutine that does an initial refresh
// after a settle delay (default 30s — same shape as
// StartBackgroundRefresh in refresh.go), then refreshes every
// `interval` until ctx cancels. Errors are logged and non-fatal.
//
// Called from main.go ONLY when settings.openrouter_refresh_enabled
// is 'true'. Returns immediately; the caller's responsibility for the
// goroutine lifecycle is ctx cancellation.
func (o *OpenRouterRefresher) Start(ctx context.Context, settle, interval time.Duration) {
	go func() {
		t := time.NewTimer(settle)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		o.runOnce(ctx)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.runOnce(ctx)
			}
		}
	}()
}

// runOnce is FetchAndPersist with logging — used by Start's goroutine.
func (o *OpenRouterRefresher) runOnce(ctx context.Context) {
	count, err := o.FetchAndPersist(ctx)
	if err != nil {
		slog.Warn("openrouter refresh failed", "err", err)
		return
	}
	slog.Info("openrouter refresh completed", "rows", count)
}

// parseOpenRouterPricing converts an OpenRouter `/api/v1/models`
// response into PricingUpdates. Per-token USD strings become per-1M
// floats; missing/null `pricing` objects are skipped (we don't have
// data, so we don't persist a guess); free models (all-zero prices)
// are persisted because $0 is a real answer to "what does this cost?"
//
// Unknown JSON fields are tolerated — `encoding/json` ignores them by
// default. This keeps the parser stable when OpenRouter adds new
// pricing dimensions (e.g., a future `audio_cache` key); recapture
// the fixture only if a new key is load-bearing for sage-router.
//
// Malformed price strings (non-empty, non-parseable) are logged at
// Warn and treated as zero. The reasoning: silently dropping the
// whole row on one bad field is worse than persisting partial data
// (the other valid prices stay correct); silently zeroing without a
// log hides data corruption from operators. The log surfaces it
// without taking down the refresh cycle. See review finding MAJOR-2.
func parseOpenRouterPricing(body []byte) ([]PricingUpdate, error) {
	var resp openrouterPricingResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("openrouter: parse JSON: %w", err)
	}
	out := make([]PricingUpdate, 0, len(resp.Data))
	for _, e := range resp.Data {
		// Entries with no pricing object or no ID are not actionable —
		// the (provider='openrouter', model_id='') row would collide
		// across every empty-ID entry, and an upsert without a target
		// model_id is meaningless. Both are skipped silently; OpenRouter
		// has never emitted either shape against the vendored fixture.
		if e.Pricing == nil || e.ID == "" {
			continue
		}
		pricing := Pricing{
			Input:      parseORFloat(e.ID, "prompt", e.Pricing.Prompt),
			Output:     parseORFloat(e.ID, "completion", e.Pricing.Completion),
			CacheRead:  parseORFloat(e.ID, "input_cache_read", e.Pricing.InputCacheRead),
			CacheWrite: parseORFloat(e.ID, "input_cache_write", e.Pricing.InputCacheWrite),
			Thinking:   parseORFloat(e.ID, "internal_reasoning", e.Pricing.InternalReasoning),
			Source:     SourceOpenRouter,
		}
		// Always emit the openrouter-namespace entry.
		out = append(out, PricingUpdate{
			Provider: "openrouter",
			ModelID:  e.ID,
			Pricing:  pricing,
		})
		// Additionally emit a mirror candidate when the prefix maps to
		// a sage-router provider key. The candidate's model_id is the
		// raw bare ID — FetchAndPersist Stage 3 will normalize it and
		// resolve against existing catalog_models rows. Unknown prefixes
		// (e.g., meta-llama/*, mistralai/*) are silently skipped — no
		// direct-provider row to mirror to. See fix 20260514-pricing-mirror.
		if slash := strings.IndexByte(e.ID, '/'); slash > 0 {
			if prov, ok := openrouterPrefixToProvider[e.ID[:slash]]; ok {
				out = append(out, PricingUpdate{
					Provider: prov,
					ModelID:  e.ID[slash+1:], // bare ID; normalized in resolveMirrorCandidates
					Pricing:  pricing,
				})
			}
		}
	}
	return out, nil
}

// parseORFloat converts an OpenRouter per-token USD price string into
// a per-1M USD float. Three cases:
//
//   - Empty string ("" — JSON field absent or null): silent zero.
//     OpenRouter omits keys that don't apply to a model (e.g., a
//     non-reasoning model has no `internal_reasoning` field). This is
//     normal, not corruption.
//   - "0" or any valid float: parsed and multiplied by 1M.
//   - Non-parseable string (e.g., "not-a-number", "$0.0001"): logged
//     at Warn with model ID + field name + raw value; returned as
//     zero so the rest of the entry's fields still persist. Surfaces
//     data corruption without dropping the row.
func parseORFloat(modelID, field, s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		slog.Warn("openrouter: malformed price",
			"model", modelID, "field", field, "raw", s, "err", err)
		return 0
	}
	return v * perTokenToPerMillion
}

// snippet returns the first ~120 bytes of a body for error messages,
// without leaking large payloads into logs.
func snippet(b []byte) string {
	const n = 120
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// JSON shapes — minimal subset of the OpenRouter response. New keys
// are ignored by encoding/json; we add fields here only when
// sage-router actually consumes them.
type openrouterPricingResp struct {
	Data []openrouterPricingEntry `json:"data"`
}

type openrouterPricingEntry struct {
	ID      string             `json:"id"`
	Pricing *openrouterPricing `json:"pricing"`
}

type openrouterPricing struct {
	Prompt            string `json:"prompt"`             // USD per input token
	Completion        string `json:"completion"`         // USD per output token
	InputCacheRead    string `json:"input_cache_read"`   // USD per cache-read token
	InputCacheWrite   string `json:"input_cache_write"`  // USD per cache-write token
	InternalReasoning string `json:"internal_reasoning"` // USD per thinking token
}
