package usage

import "sage-router/internal/config"

// CalculateCost computes the cost for a request based on token usage.
func CalculateCost(provider, model string, inputTokens, outputTokens int) float64 {
	return config.EstimateCost(provider, model, inputTokens, outputTokens)
}
