package config

// GetModelPrice returns the per-million-token input and output prices in USD
// for a given provider and model. It first checks the built-in ModelCatalog
// by model ID. If not found, it returns (0, 0).
//
// The provider parameter is accepted for future use (e.g. the same model may
// have different pricing on different providers) but is currently unused for
// the built-in catalog lookup.
func GetModelPrice(provider, model string) (inputPrice, outputPrice float64) {
	info := GetModel(model)
	if info == nil {
		return 0, 0
	}
	return info.InputPrice, info.OutputPrice
}

// EstimateCost calculates the estimated cost in USD for a request given the
// provider, model, and token counts. Prices are per 1 million tokens.
func EstimateCost(provider, model string, inputTokens, outputTokens int) float64 {
	inPrice, outPrice := GetModelPrice(provider, model)
	if inPrice == 0 && outPrice == 0 {
		return 0
	}
	cost := (float64(inputTokens) * inPrice / 1_000_000) +
		(float64(outputTokens) * outPrice / 1_000_000)
	return cost
}
