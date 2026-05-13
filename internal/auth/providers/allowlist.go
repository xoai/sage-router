package providers

import "strings"

// wildcardSuffix is the marker used in registry allowlist entries to
// indicate "match any model that starts with the prefix before this
// suffix." Chosen as "-x" because it never appears naturally in any
// provider's model naming scheme (verified against current openai /
// anthropic / gemini / copilot catalogs).
const wildcardSuffix = "-x"

// ModelInAllowlist reports whether `model` is permitted by `allowed`.
//
// Semantics:
//   - Empty `allowed` (nil or len 0) permits everything — used for
//     api-key connections that aren't constrained by a subscription
//     tier.
//   - Empty `model` is permitted unconditionally — selection paths that
//     call into the filter without a target model shouldn't be punished.
//   - Exact match on any entry permits.
//   - An allowlist entry ending in `-x` is a wildcard; it matches
//     models whose name equals the prefix OR starts with `prefix + "-"`.
//     The hyphen boundary prevents false positives like
//     "claude-sonnet-40" matching "claude-sonnet-4-x".
func ModelInAllowlist(model string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	if model == "" {
		return true
	}
	for _, entry := range allowed {
		if entry == model {
			return true
		}
		if strings.HasSuffix(entry, wildcardSuffix) {
			prefix := strings.TrimSuffix(entry, wildcardSuffix)
			if model == prefix || strings.HasPrefix(model, prefix+"-") {
				return true
			}
		}
	}
	return false
}

// SubscriptionAllowed reports whether the given provider's subscription
// tier permits `model`. Returns false for unknown providers (failing
// closed — better to skip the connection than to let an unconfigured
// provider serve traffic).
func SubscriptionAllowed(providerID, model string) bool {
	cfg, ok := Providers[providerID]
	if !ok {
		return false
	}
	return ModelInAllowlist(model, cfg.SubscriptionAllowedModels)
}
