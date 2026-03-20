package config

// Capability tiers for smart routing. Lower = more capable.
const (
	TierFrontier  = 1 // Best quality: claude-opus, claude-sonnet, gpt-4.1, gpt-4o, gemini-pro, o3
	TierStrong    = 2 // Good balance: claude-haiku, gpt-4o-mini, gpt-4.1-mini, gemini-flash, o3-mini
	TierEfficient = 3 // Fast/cheap: gpt-4.1-nano, gemini-flash-lite, gemini-2.0-flash
	TierFree      = 4 // Local/free: ollama models
)

// ModelInfo describes a known model's capabilities and pricing.
type ModelInfo struct {
	// ID is the canonical model identifier sent to the provider.
	ID string `json:"id"`

	// Provider is the default provider ID for this model.
	Provider string `json:"provider"`

	// DisplayName is the human-readable model name.
	DisplayName string `json:"display_name"`

	// Tier is the capability tier for smart routing (1=frontier, 4=free).
	Tier int `json:"tier"`

	// ContextWindow is the maximum number of tokens (input + output) the model supports.
	ContextWindow int `json:"context_window"`

	// MaxOutput is the maximum number of output tokens the model can generate.
	MaxOutput int `json:"max_output"`

	// InputPrice is the cost per 1 million input tokens in USD.
	InputPrice float64 `json:"input_price"`

	// OutputPrice is the cost per 1 million output tokens in USD.
	OutputPrice float64 `json:"output_price"`

	// Capability flags for hard constraint filtering (Layer 2).
	SupportsImages   bool `json:"supports_images"`
	SupportsTools    bool `json:"supports_tools"`
	SupportsThinking bool `json:"supports_thinking"`
}

// ModelCatalog is the built-in registry of well-known models and their metadata.
// Pricing as of March 2026. Sources: Anthropic, OpenAI, Google AI docs.
var ModelCatalog = map[string]ModelInfo{
	// ── Anthropic (all support images, tools, thinking) ──
	"claude-opus-4-6":           {ID: "claude-opus-4-6", Provider: "anthropic", DisplayName: "Claude Opus 4.6", Tier: TierFrontier, ContextWindow: 1000000, MaxOutput: 128000, InputPrice: 5.00, OutputPrice: 25.00, SupportsImages: true, SupportsTools: true, SupportsThinking: true},
	"claude-sonnet-4-6":         {ID: "claude-sonnet-4-6", Provider: "anthropic", DisplayName: "Claude Sonnet 4.6", Tier: TierFrontier, ContextWindow: 1000000, MaxOutput: 64000, InputPrice: 3.00, OutputPrice: 15.00, SupportsImages: true, SupportsTools: true, SupportsThinking: true},
	"claude-haiku-4-5-20251001": {ID: "claude-haiku-4-5-20251001", Provider: "anthropic", DisplayName: "Claude Haiku 4.5", Tier: TierStrong, ContextWindow: 200000, MaxOutput: 64000, InputPrice: 1.00, OutputPrice: 5.00, SupportsImages: true, SupportsTools: true, SupportsThinking: true},
	"claude-sonnet-4-20250514":  {ID: "claude-sonnet-4-20250514", Provider: "anthropic", DisplayName: "Claude Sonnet 4", Tier: TierFrontier, ContextWindow: 200000, MaxOutput: 64000, InputPrice: 3.00, OutputPrice: 15.00, SupportsImages: true, SupportsTools: true, SupportsThinking: true},
	"claude-opus-4-20250514":    {ID: "claude-opus-4-20250514", Provider: "anthropic", DisplayName: "Claude Opus 4", Tier: TierFrontier, ContextWindow: 200000, MaxOutput: 32000, InputPrice: 15.00, OutputPrice: 75.00, SupportsImages: true, SupportsTools: true, SupportsThinking: true},

	// ── OpenAI (all support images + tools; o-series supports thinking) ──
	"gpt-4.1":      {ID: "gpt-4.1", Provider: "openai", DisplayName: "GPT-4.1", Tier: TierFrontier, ContextWindow: 1000000, MaxOutput: 32768, InputPrice: 2.00, OutputPrice: 8.00, SupportsImages: true, SupportsTools: true, SupportsThinking: false},
	"gpt-4.1-mini": {ID: "gpt-4.1-mini", Provider: "openai", DisplayName: "GPT-4.1 Mini", Tier: TierStrong, ContextWindow: 1000000, MaxOutput: 32768, InputPrice: 0.20, OutputPrice: 0.80, SupportsImages: true, SupportsTools: true, SupportsThinking: false},
	"gpt-4.1-nano": {ID: "gpt-4.1-nano", Provider: "openai", DisplayName: "GPT-4.1 Nano", Tier: TierEfficient, ContextWindow: 1000000, MaxOutput: 32768, InputPrice: 0.05, OutputPrice: 0.20, SupportsImages: false, SupportsTools: true, SupportsThinking: false},
	"gpt-4o":       {ID: "gpt-4o", Provider: "openai", DisplayName: "GPT-4o", Tier: TierFrontier, ContextWindow: 128000, MaxOutput: 16384, InputPrice: 2.50, OutputPrice: 10.00, SupportsImages: true, SupportsTools: true, SupportsThinking: false},
	"gpt-4o-mini":  {ID: "gpt-4o-mini", Provider: "openai", DisplayName: "GPT-4o Mini", Tier: TierStrong, ContextWindow: 128000, MaxOutput: 16384, InputPrice: 0.15, OutputPrice: 0.60, SupportsImages: true, SupportsTools: true, SupportsThinking: false},
	"o3":           {ID: "o3", Provider: "openai", DisplayName: "o3", Tier: TierFrontier, ContextWindow: 200000, MaxOutput: 100000, InputPrice: 2.00, OutputPrice: 8.00, SupportsImages: true, SupportsTools: true, SupportsThinking: true},
	"o3-mini":      {ID: "o3-mini", Provider: "openai", DisplayName: "o3 Mini", Tier: TierStrong, ContextWindow: 200000, MaxOutput: 100000, InputPrice: 0.55, OutputPrice: 2.20, SupportsImages: false, SupportsTools: true, SupportsThinking: true},
	"o4-mini":      {ID: "o4-mini", Provider: "openai", DisplayName: "o4 Mini", Tier: TierStrong, ContextWindow: 200000, MaxOutput: 100000, InputPrice: 1.10, OutputPrice: 4.40, SupportsImages: true, SupportsTools: true, SupportsThinking: true},

	// ── Google Gemini (all support images + tools; no extended thinking) ──
	"gemini-2.5-flash":      {ID: "gemini-2.5-flash", Provider: "gemini", DisplayName: "Gemini 2.5 Flash", Tier: TierStrong, ContextWindow: 1048576, MaxOutput: 65536, InputPrice: 0.15, OutputPrice: 0.60, SupportsImages: true, SupportsTools: true, SupportsThinking: false},
	"gemini-2.5-pro":        {ID: "gemini-2.5-pro", Provider: "gemini", DisplayName: "Gemini 2.5 Pro", Tier: TierFrontier, ContextWindow: 1048576, MaxOutput: 65536, InputPrice: 1.25, OutputPrice: 10.00, SupportsImages: true, SupportsTools: true, SupportsThinking: false},
	"gemini-2.5-flash-lite": {ID: "gemini-2.5-flash-lite", Provider: "gemini", DisplayName: "Gemini 2.5 Flash-Lite", Tier: TierEfficient, ContextWindow: 1048576, MaxOutput: 65536, InputPrice: 0.02, OutputPrice: 0.10, SupportsImages: true, SupportsTools: false, SupportsThinking: false},
	"gemini-2.0-flash":      {ID: "gemini-2.0-flash", Provider: "gemini", DisplayName: "Gemini 2.0 Flash", Tier: TierEfficient, ContextWindow: 1048576, MaxOutput: 8192, InputPrice: 0.10, OutputPrice: 0.40, SupportsImages: true, SupportsTools: true, SupportsThinking: false},
}

// GetModel returns the ModelInfo for the given model ID, or nil if the model
// is not in the catalog.
func GetModel(id string) *ModelInfo {
	m, ok := ModelCatalog[id]
	if !ok {
		return nil
	}
	return &m
}
