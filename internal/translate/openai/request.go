package openai

import (
	"encoding/json"
	"fmt"
	"sage-router/internal/translate"
	"sage-router/pkg/canonical"
	"strings"
)

// Translator handles OpenAI ↔ Canonical translation.
type Translator struct{}

func init() {
	// Self-register when imported
}

// New creates a new OpenAI translator.
func New() *Translator {
	return &Translator{}
}

func (t *Translator) Format() canonical.Format {
	return canonical.FormatOpenAI
}

func (t *Translator) DetectInbound(endpoint string, body []byte) bool {
	return strings.Contains(endpoint, "/v1/chat/completions")
}

// OpenAI wire types for request parsing
type openAIRequest struct {
	Model          string             `json:"model"`
	Messages       []openAIMessage    `json:"messages"`
	Tools          []openAITool       `json:"tools,omitempty"`
	ToolChoice     any                `json:"tool_choice,omitempty"`
	Stream         bool               `json:"stream"`
	MaxTokens      int                `json:"max_tokens,omitempty"`
	Temperature    *float64           `json:"temperature,omitempty"`
	TopP           *float64           `json:"top_p,omitempty"`
	Stop           any                `json:"stop,omitempty"`
	ResponseFormat *responseFormat    `json:"response_format,omitempty"`
}

type openAIMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"` // string or []contentBlock
	Name       string         `json:"name,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type openAIContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

type openAIImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responseFormat struct {
	Type       string          `json:"type"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

func (t *Translator) ToCanonical(body []byte, opts translate.TranslateOpts) (*canonical.Request, error) {
	var oai openAIRequest
	if err := json.Unmarshal(body, &oai); err != nil {
		return nil, fmt.Errorf("parse openai request: %w", err)
	}

	req := &canonical.Request{
		Model:       oai.Model,
		Stream:      oai.Stream,
		MaxTokens:   oai.MaxTokens,
		Temperature: oai.Temperature,
		TopP:        oai.TopP,
	}

	// Parse stop sequences
	if oai.Stop != nil {
		switch v := oai.Stop.(type) {
		case string:
			req.Stop = []string{v}
		case []any:
			for _, s := range v {
				if str, ok := s.(string); ok {
					req.Stop = append(req.Stop, str)
				}
			}
		}
	}

	// Parse response format
	if oai.ResponseFormat != nil {
		req.ResponseFormat = &canonical.ResponseFormat{
			Type:       oai.ResponseFormat.Type,
			JSONSchema: oai.ResponseFormat.JSONSchema,
		}
	}

	// Parse messages
	for _, msg := range oai.Messages {
		cm := canonical.Message{
			Role:    msg.Role,
			Content: []canonical.Content{},
		}

		// Extract system messages into canonical.System
		if msg.Role == canonical.RoleSystem {
			text := extractTextFromContent(msg.Content)
			req.System = append(req.System, canonical.SystemBlock{Text: text})
			continue
		}

		// Handle tool role
		if msg.Role == canonical.RoleTool {
			text := extractTextFromContent(msg.Content)
			cm.Content = append(cm.Content, canonical.ToolResultContent(msg.ToolCallID, text, false))
			req.Messages = append(req.Messages, cm)
			continue
		}

		// Parse content
		parseOpenAIContent(msg.Content, &cm)

		// Parse tool calls from assistant messages
		for _, tc := range msg.ToolCalls {
			cm.Content = append(cm.Content, canonical.ToolCallContent(
				tc.ID,
				tc.Function.Name,
				tc.Function.Arguments,
			))
		}

		req.Messages = append(req.Messages, cm)
	}

	// Parse tools
	for _, tool := range oai.Tools {
		req.Tools = append(req.Tools, canonical.Tool{
			Type:        tool.Type,
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}

	// Parse tool choice
	if oai.ToolChoice != nil {
		req.ToolChoice = parseOpenAIToolChoice(oai.ToolChoice)
	}

	return req, nil
}

func extractTextFromContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		for _, block := range v {
			if m, ok := block.(map[string]any); ok {
				if t, ok := m["type"].(string); ok && t == "text" {
					if text, ok := m["text"].(string); ok {
						return text
					}
				}
			}
		}
	}
	return ""
}

func parseOpenAIContent(content any, msg *canonical.Message) {
	switch v := content.(type) {
	case string:
		msg.Content = append(msg.Content, canonical.TextContent(v))
	case []any:
		for _, block := range v {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := m["type"].(string)
			switch blockType {
			case "text":
				text, _ := m["text"].(string)
				msg.Content = append(msg.Content, canonical.TextContent(text))
			case "image_url":
				if imgURL, ok := m["image_url"].(map[string]any); ok {
					url, _ := imgURL["url"].(string)
					if strings.HasPrefix(url, "data:") {
						// Parse data URI: data:image/png;base64,<data>
						parts := strings.SplitN(url, ",", 2)
						if len(parts) == 2 {
							mediaType := strings.TrimPrefix(strings.SplitN(parts[0], ";", 2)[0], "data:")
							msg.Content = append(msg.Content, canonical.ImageContent(mediaType, parts[1]))
						}
					} else {
						msg.Content = append(msg.Content, canonical.ImageURLContent(url))
					}
				}
			}
		}
	}

	// If no content was parsed, add empty text
	if len(msg.Content) == 0 {
		msg.Content = append(msg.Content, canonical.TextContent(""))
	}
}

func parseOpenAIToolChoice(tc any) *canonical.ToolChoice {
	switch v := tc.(type) {
	case string:
		return &canonical.ToolChoice{Type: v}
	case map[string]any:
		choice := &canonical.ToolChoice{}
		if t, ok := v["type"].(string); ok {
			choice.Type = t
		}
		if fn, ok := v["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				choice.Type = "tool"
				choice.Name = name
			}
		}
		return choice
	}
	return nil
}

func (t *Translator) FromCanonical(req *canonical.Request, opts translate.TranslateOpts) ([]byte, error) {
	oai := openAIRequest{
		Model:       req.Model,
		Stream:      req.Stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	if len(req.Stop) > 0 {
		if len(req.Stop) == 1 {
			oai.Stop = req.Stop[0]
		} else {
			oai.Stop = req.Stop
		}
	}

	if req.ResponseFormat != nil {
		oai.ResponseFormat = &responseFormat{
			Type:       req.ResponseFormat.Type,
			JSONSchema: req.ResponseFormat.JSONSchema,
		}
	}

	// Convert system blocks to system message
	for _, sys := range req.System {
		oai.Messages = append(oai.Messages, openAIMessage{
			Role:    canonical.RoleSystem,
			Content: sys.Text,
		})
	}

	// Convert messages
	for _, msg := range req.Messages {
		oaiMsg := openAIMessage{Role: msg.Role}

		var textParts []any
		var toolCalls []openAIToolCall

		for _, c := range msg.Content {
			switch c.Type {
			case canonical.TypeText:
				textParts = append(textParts, map[string]any{
					"type": "text",
					"text": c.Text,
				})
			case canonical.TypeImage:
				if c.ImageSource != nil {
					var url string
					if c.ImageSource.URL != "" {
						url = c.ImageSource.URL
					} else {
						url = fmt.Sprintf("data:%s;base64,%s", c.ImageSource.MediaType, c.ImageSource.Data)
					}
					textParts = append(textParts, map[string]any{
						"type": "image_url",
						"image_url": map[string]any{
							"url": url,
						},
					})
				}
			case canonical.TypeToolCall:
				toolCalls = append(toolCalls, openAIToolCall{
					ID:   c.ToolCallID,
					Type: "function",
					Function: openAIFunctionCall{
						Name:      c.ToolName,
						Arguments: c.Arguments,
					},
				})
			case canonical.TypeToolResult:
				// Tool results become separate messages in OpenAI format
				oai.Messages = append(oai.Messages, openAIMessage{
					Role:       canonical.RoleTool,
					Content:    c.Text,
					ToolCallID: c.ToolCallID,
				})
				continue
			case canonical.TypeThinking:
				// OpenAI doesn't support thinking blocks natively - skip
				continue
			}
		}

		// Set content
		if len(textParts) == 1 {
			if m, ok := textParts[0].(map[string]any); ok {
				if t, ok := m["type"].(string); ok && t == "text" {
					oaiMsg.Content = m["text"]
				} else {
					oaiMsg.Content = textParts
				}
			}
		} else if len(textParts) > 1 {
			oaiMsg.Content = textParts
		} else if len(toolCalls) > 0 {
			oaiMsg.Content = ""
		}

		if len(toolCalls) > 0 {
			oaiMsg.ToolCalls = toolCalls
		}

		// Skip tool result messages that were already added above
		if msg.Role == canonical.RoleTool {
			continue
		}

		oai.Messages = append(oai.Messages, oaiMsg)
	}

	// Convert tools
	for _, tool := range req.Tools {
		oai.Tools = append(oai.Tools, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			},
		})
	}

	// Convert tool choice
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "auto", "none", "required":
			oai.ToolChoice = req.ToolChoice.Type
		case "tool":
			oai.ToolChoice = map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": req.ToolChoice.Name,
				},
			}
		}
	}

	return json.Marshal(oai)
}
