package catalog

import (
	"regexp"
	"strings"
)

// openrouterPrefixToProvider maps OpenRouter's namespace prefixes to
// sage-router's canonical provider keys. Only namespaces in this map
// have their pricing mirrored into direct-provider rows by the
// OpenRouterRefresher.
//
// "google" → "gemini" rename matches config.KnownProviders.
//
// Hardcoded today; if a new direct-provider is added to KnownProviders
// (e.g., xAI), update this map in lockstep. See fix
// 20260514-pricing-mirror.
var openrouterPrefixToProvider = map[string]string{
	"openai":    "openai",
	"anthropic": "anthropic",
	"google":    "gemini",
}

// anthropicDateSuffix matches a trailing "-YYYYMMDD" date version
// stamp on anthropic model IDs. Examples that should match:
//
//	claude-opus-4-1-20250805  →  claude-opus-4-1
//	claude-sonnet-4-5-20250929 → claude-sonnet-4-5
//	claude-haiku-4-5-20251001 → claude-haiku-4-5
//
// Restricted to plausible year range (2020-2099) AND valid month/day
// boundaries so generic 8-digit suffixes don't false-positive. The
// regex doesn't validate Feb-30 / Apr-31 combinations — acceptable
// (model release IDs never construct calendar-invalid suffixes).
var anthropicDateSuffix = regexp.MustCompile(`-20\d{2}(0[1-9]|1[0-2])(0[1-9]|[12]\d|3[01])$`)

// normalizeID brings a model ID into a canonical form for matching
// across the OpenRouter / direct-provider boundary:
//
//   - openai:    no-op (clean; OpenRouter and direct discovery agree)
//   - anthropic: dots→dashes, then strip trailing -YYYYMMDD
//   - gemini:    no-op (google→gemini handled by openrouterPrefixToProvider)
//   - default:   unknown provider, pass through unchanged
//
// Both sides (OpenRouter IDs after stripping the namespace prefix, and
// catalog_models IDs) are normalized using this function. Match on
// equal normalized forms via resolveMirrorCandidates.
func normalizeID(provider, id string) string {
	switch provider {
	case "anthropic":
		// Replace dots with dashes first so the date-suffix regex
		// matches consistently across OpenRouter's dot-form and our
		// dash-form IDs.
		id = strings.ReplaceAll(id, ".", "-")
		return anthropicDateSuffix.ReplaceAllString(id, "")
	default:
		// openai, gemini, and any future provider: pass through.
		// If a new provider introduces ID drift, extend this switch.
		return id
	}
}

// resolveMirrorCandidates maps each mirror candidate to the matching
// catalog_models row(s) by normalized-ID equality. Returns the subset
// of candidates whose direct-provider row exists, with model_id
// rewritten to the EXACT catalog_models key (so the BulkUpsertPricing
// FK target is correct).
//
// If multiple catalog_models rows normalize to the same form (e.g.,
// claude-opus-4-7 AND a hypothetical claude-opus-4-7-20251119 both
// normalize to claude-opus-4-7), all of them receive the same pricing.
// This is the right behavior — the canonical-date variant and the
// short-form should be priced identically.
//
// Complexity: O(N×M) where N = mirror candidates, M = existing rows
// per provider. In practice N ≤ ~30, M ≤ ~20, so ≤600 normalization
// calls per refresh. Negligible cost given OpenRouterRefresher's
// 24h tick.
func resolveMirrorCandidates(
	candidates []PricingUpdate,
	existing map[string][]string, // provider → list of catalog model_ids
) []PricingUpdate {
	var out []PricingUpdate
	for _, c := range candidates {
		normCand := normalizeID(c.Provider, c.ModelID)
		for _, mid := range existing[c.Provider] {
			if normalizeID(c.Provider, mid) == normCand {
				out = append(out, PricingUpdate{
					Provider: c.Provider,
					ModelID:  mid, // use the catalog row's exact ID
					Pricing:  c.Pricing,
				})
			}
		}
	}
	return out
}

// uniqueProviders extracts the distinct provider values from a slice
// of PricingUpdate. Small input in practice (≤3: openai, anthropic, gemini).
// Used to build the IN-list for ListModelIDsForProviders.
func uniqueProviders(updates []PricingUpdate) []string {
	seen := make(map[string]bool, 3)
	out := make([]string, 0, 3)
	for _, u := range updates {
		if !seen[u.Provider] {
			seen[u.Provider] = true
			out = append(out, u.Provider)
		}
	}
	return out
}
