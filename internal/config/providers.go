package config

// ProviderDef describes a known upstream LLM provider.
type ProviderDef struct {
	// ID is the unique machine-readable identifier (e.g. "openai").
	ID string `json:"id"`

	// Name is the human-readable display name.
	Name string `json:"name"`

	// Format is the API wire format this provider speaks (matches canonical.Format values).
	Format string `json:"format"`

	// BaseURL is the default API base URL (no trailing slash).
	BaseURL string `json:"base_url"`

	// AuthTypes lists the authentication methods this provider supports
	// (e.g. "bearer", "x-api-key", "none").
	AuthTypes []string `json:"auth_types"`

	// Models lists the model IDs available from this provider by default.
	Models []string `json:"models"`
}

// KnownProviders is the built-in registry of supported providers.
var KnownProviders = map[string]ProviderDef{
	"openai": {
		ID:      "openai",
		Name:    "OpenAI",
		Format:  "openai",
		BaseURL: "https://api.openai.com/v1",
		AuthTypes: []string{"bearer"},
		Models: []string{
			"gpt-4.1",
			"gpt-4.1-mini",
			"gpt-4.1-nano",
			"gpt-4o",
			"gpt-4o-mini",
			"o3",
			"o3-mini",
			"o4-mini",
		},
	},
	"anthropic": {
		ID:      "anthropic",
		Name:    "Anthropic",
		Format:  "claude",
		BaseURL: "https://api.anthropic.com/v1",
		AuthTypes: []string{"x-api-key"},
		Models: []string{
			"claude-opus-4-6",
			"claude-sonnet-4-6",
			"claude-haiku-4-5-20251001",
			"claude-sonnet-4-20250514",
			"claude-opus-4-20250514",
		},
	},
	"gemini": {
		ID:      "gemini",
		Name:    "Google Gemini",
		Format:  "gemini",
		BaseURL: "https://generativelanguage.googleapis.com/v1beta",
		AuthTypes: []string{"bearer", "api-key-param"},
		Models: []string{
			"gemini-2.5-flash",
			"gemini-2.5-pro",
			"gemini-2.5-flash-lite",
			"gemini-2.0-flash",
		},
	},
	"github-copilot": {
		ID:      "github-copilot",
		Name:    "GitHub Copilot",
		Format:  "openai",
		BaseURL: "https://api.githubcopilot.com",
		AuthTypes: []string{"bearer"},
		Models: []string{
			"gpt-4o",
			"gpt-4.1",
			"claude-sonnet-4-6",
		},
	},
	"openrouter": {
		ID:      "openrouter",
		Name:    "OpenRouter",
		Format:  "openai",
		BaseURL: "https://openrouter.ai/api/v1",
		AuthTypes: []string{"bearer"},
		Models: []string{
			"openai/gpt-4.1",
			"openai/gpt-4o",
			"anthropic/claude-sonnet-4-6",
			"anthropic/claude-haiku-4-5-20251001",
			"google/gemini-2.5-flash",
			"google/gemini-2.5-pro",
		},
	},
	"ollama": {
		ID:      "ollama",
		Name:    "Ollama",
		Format:  "ollama",
		BaseURL: "http://127.0.0.1:11434/api",
		AuthTypes: []string{"none"},
		Models:  []string{},
	},
}

// GetProvider returns the ProviderDef for the given provider ID, or nil if
// the provider is not known.
func GetProvider(id string) *ProviderDef {
	p, ok := KnownProviders[id]
	if !ok {
		return nil
	}
	return &p
}
