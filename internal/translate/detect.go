package translate

import (
	"encoding/json"
	"sage-router/pkg/canonical"
	"strings"
)

// DetectSourceFormat determines the source API format from endpoint and body shape.
func DetectSourceFormat(endpoint string, body []byte) canonical.Format {
	// Endpoint-based detection
	if strings.Contains(endpoint, "/v1/messages") {
		return canonical.FormatClaude
	}
	if strings.Contains(endpoint, "/v1/responses") {
		return canonical.FormatResponses
	}
	if strings.Contains(endpoint, "/v1beta/models") || strings.Contains(endpoint, ":generateContent") || strings.Contains(endpoint, ":streamGenerateContent") {
		return canonical.FormatGemini
	}
	if strings.Contains(endpoint, "/api/chat") || strings.Contains(endpoint, "/api/generate") {
		return canonical.FormatOllama
	}

	// Body-shape detection for /v1/chat/completions
	var probe struct {
		System   json.RawMessage `json:"system"`
		Messages json.RawMessage `json:"messages"`
		Contents json.RawMessage `json:"contents"`
		Input    json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		if probe.System != nil && probe.Messages != nil {
			// Claude uses system as separate field
			return canonical.FormatClaude
		}
		if probe.Contents != nil {
			return canonical.FormatGemini
		}
		if probe.Input != nil {
			return canonical.FormatResponses
		}
	}

	return canonical.FormatOpenAI
}

// DetectTargetFormat returns the wire format for a given provider.
func DetectTargetFormat(provider string) canonical.Format {
	switch provider {
	case "anthropic", "claude", "claude-code":
		return canonical.FormatClaude
	case "gemini", "gemini-cli", "vertex":
		return canonical.FormatGemini
	case "kiro":
		return canonical.FormatKiro
	case "cursor":
		return canonical.FormatCursor
	case "ollama":
		return canonical.FormatOllama
	default:
		return canonical.FormatOpenAI
	}
}
