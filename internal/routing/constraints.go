package routing

// RequestConstraints describes what a request requires from a model.
// All fields are binary: true means the request needs this capability.
type RequestConstraints struct {
	NeedsImages   bool
	NeedsTools    bool
	NeedsThinking bool
	NeedsLongCtx  bool // estimated tokens > 128K
	EstTokens     int  // estimated total token count
}

// DetectConstraints scans a request body (already parsed as bypass.Req-like fields)
// to determine what capabilities are needed. This is a lightweight check (<1ms).
func DetectConstraints(hasImages, hasTools, hasThinking bool, estTokens int) RequestConstraints {
	return RequestConstraints{
		NeedsImages:   hasImages,
		NeedsTools:    hasTools,
		NeedsThinking: hasThinking,
		NeedsLongCtx:  estTokens > 128000,
		EstTokens:     estTokens,
	}
}

// FilterByConstraints removes candidates that cannot handle the request.
// Every constraint is binary — a model either supports it or doesn't.
func FilterByConstraints(candidates []ModelCandidate, constraints RequestConstraints) []ModelCandidate {
	if !constraints.NeedsImages && !constraints.NeedsTools && !constraints.NeedsThinking && !constraints.NeedsLongCtx {
		return candidates // no constraints to filter
	}

	var filtered []ModelCandidate
	for _, c := range candidates {
		if constraints.NeedsImages && !c.SupportsImages {
			continue
		}
		if constraints.NeedsTools && !c.SupportsTools {
			continue
		}
		if constraints.NeedsThinking && !c.SupportsThinking {
			continue
		}
		if constraints.NeedsLongCtx && c.ContextWindow <= 200000 {
			continue
		}
		filtered = append(filtered, c)
	}

	// If filtering removed everything, return original list as fallback
	// (better to try an imperfect model than return nothing)
	if len(filtered) == 0 {
		return candidates
	}

	return filtered
}
