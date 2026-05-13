// Package cost implements pipeline stages ③b (CostRoute) and ⑤b (CacheHints)
// for the sage-router request pipeline.
//
// v1 scope: prompt caching injection for Claude/Anthropic.
// v2 scope: batch API routing, request deduplication, budget alerts.
package cost

import (
	"log/slog"
	"sync"

	"sage-router/pkg/canonical"
)

// minTokensForCache is the minimum estimated token count for a system block
// to be worth caching. Anthropic charges for cache creation, so small prompts
// don't benefit. The break-even is roughly 1024 tokens (based on Anthropic's
// cache pricing: creation costs 25% extra, reads save 90%).
const minTokensForCache = 1024

// estimateTokens gives a rough token count for a string.
// Approximation: 1 token ≈ 4 characters for English text.
func estimateTokens(s string) int {
	return (len(s) + 3) / 4
}

// InjectCacheHints modifies a canonical request in-place to add cache_control
// hints where they would reduce cost. This is Stage ⑤b of the pipeline.
//
// Currently supports:
//   - Claude/Anthropic (apikey): adds cache_control on system blocks that
//     exceed the token threshold. Only the last qualifying block gets the
//     hint (Anthropic requires cache breakpoints on the last block in a
//     cacheable prefix).
//   - Claude/Anthropic (subscription): same injection — verified at M2
//     checkpoint that the subscription endpoints accept the beta header.
//     If a future Anthropic change breaks this, swap to the Gemini-sub
//     pattern (skip injection).
//   - Gemini (subscription): SKIPPED. Gemini's prompt-caching path uses
//     a separate SetupCache/TeardownCache HTTP call (when implemented)
//     that bypasses the authTransport. Until that path is wired into
//     the canonical pipeline, this branch is a no-op forward-compat hook.
//
// Returns true if any hints were injected.
//
// authType is the connection's canonical AuthType ("apikey" | "subscription"
// | "none"). Pass empty string for "don't know / don't care" — used in
// existing tests that pre-date subscription auth.
func InjectCacheHints(req *canonical.Request, provider, authType string) bool {
	if req == nil {
		return false
	}

	switch provider {
	case "anthropic":
		return injectClaudeCacheHints(req)
	case "gemini":
		if authType == "subscription" {
			// Forward-compat: when Gemini caching ships in the canonical
			// pipeline, we need to skip it for subscription connections
			// to avoid the SetupCache HTTP bypass issue (spec §M3.2).
			logCachingDisabled(provider, "gemini-subscription")
			return false
		}
		return false
	default:
		return false
	}
}

// logCachingDisabled emits a one-time INFO when caching is skipped for a
// reason worth surfacing to operators. Deduplicated by (provider, reason)
// so a steady stream of subscription requests doesn't flood logs.
var (
	cacheLogMu   sync.Mutex
	cacheLogSeen = map[string]bool{}
)

func logCachingDisabled(provider, reason string) {
	key := provider + ":" + reason
	cacheLogMu.Lock()
	defer cacheLogMu.Unlock()
	if cacheLogSeen[key] {
		return
	}
	cacheLogSeen[key] = true
	slog.Info("prompt caching disabled",
		"provider", provider,
		"reason", reason,
	)
}

// injectClaudeCacheHints adds ephemeral cache_control to Claude system blocks.
//
// Strategy: estimate total system prompt tokens. If above threshold, mark the
// last system block with cache_control. Anthropic caches everything up to and
// including the marked block.
//
// We only inject if no cache_control is already present (respect user config).
func injectClaudeCacheHints(req *canonical.Request) bool {
	if len(req.System) == 0 {
		return false
	}

	// Don't inject if client already uses caching on any block
	if clientUsesCaching(req) {
		return false
	}

	// Estimate total system prompt size
	totalTokens := 0
	for _, sb := range req.System {
		totalTokens += estimateTokens(sb.Text)
	}

	if totalTokens < minTokensForCache {
		return false
	}

	// Mark the last system block as cacheable
	last := len(req.System) - 1
	req.System[last].CacheControl = &canonical.CacheControl{
		Type: "ephemeral",
	}

	return true
}

// clientUsesCaching checks if the client already set cache_control on any
// system block or message content block. If so, we skip injection entirely
// to avoid exceeding Anthropic's 4-breakpoint limit.
func clientUsesCaching(req *canonical.Request) bool {
	for _, sb := range req.System {
		if sb.CacheControl != nil {
			return true
		}
	}
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			if c.CacheControl != nil {
				return true
			}
		}
	}
	return false
}
