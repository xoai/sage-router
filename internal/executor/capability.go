package executor

// Capabilities describes the model-level capability flags consumed by
// the smart router. It mirrors `catalog.Capabilities` field-for-field
// but lives in the executor package so executors can override catalog
// defaults at route time without importing the catalog package
// (preserves the executor → catalog non-dependency).
//
// Added in Models Discovery M3.4b. Per-executor implementations of
// CapabilityOverrider land in M3.5 (claude / gemini / default).
type Capabilities struct {
	SupportsImages   bool
	SupportsTools    bool
	SupportsThinking bool
}

// CapabilityOverrider is an optional Executor extension: when an
// executor implements it, `server.buildSmartCandidates` calls
// `OverrideCapabilities(modelID, base)` and uses the returned struct
// in place of the catalog's flags. Used when newer model variants
// support capabilities the seeded catalog row hasn't been updated
// for yet (e.g., claude-sonnet-4-6 gaining thinking when the seed
// still reflects claude-sonnet-4).
//
// Executors that don't override capabilities simply omit this
// method; the type-assertion in buildSmartCandidates short-circuits
// to identity (no override). M3.5 wires Claude/Gemini/default
// implementations.
type CapabilityOverrider interface {
	OverrideCapabilities(model string, base Capabilities) Capabilities
}
