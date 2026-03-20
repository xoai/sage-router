package gemini

import (
	"encoding/json"
	"fmt"
	"sage-router/internal/translate"
	"sage-router/pkg/canonical"
	"strings"
)

// Translator handles Gemini ↔ Canonical translation.
type Translator struct{}

// New creates a new Gemini translator.
func New() *Translator {
	return &Translator{}
}

func (t *Translator) Format() canonical.Format {
	return canonical.FormatGemini
}

func (t *Translator) DetectInbound(endpoint string, body []byte) bool {
	if strings.Contains(endpoint, "/v1beta/models") ||
		strings.Contains(endpoint, ":generateContent") ||
		strings.Contains(endpoint, ":streamGenerateContent") {
		return true
	}
	// Body-shape detection: Gemini uses "contents" instead of "messages"
	var probe struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		return probe.Contents != nil
	}
	return false
}

// --- Gemini wire types ---

type geminiRequest struct {
	Contents          []geminiContent    `json:"contents"`
	SystemInstruction *geminiContent     `json:"system_instruction,omitempty"`
	Tools             []geminiToolSet    `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig  `json:"tool_config,omitempty"`
	GenerationConfig  *geminiGenConfig   `json:"generationConfig,omitempty"`
	// Model is not in the body — it's part of the URL path, but some wrappers include it.
	Model string `json:"model,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string              `json:"text,omitempty"`
	InlineData       *geminiInlineData   `json:"inline_data,omitempty"`
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResponse `json:"functionResponse,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFuncResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiToolSet struct {
	FunctionDeclarations []geminiFunctionDecl `json:"function_declarations,omitempty"`
}

type geminiFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig *geminiFuncCallingConfig `json:"function_calling_config,omitempty"`
}

type geminiFuncCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowed_function_names,omitempty"`
}

type geminiGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	ResponseMimeType string  `json:"responseMimeType,omitempty"`
}

// ToCanonical converts a Gemini request body to canonical form.
func (t *Translator) ToCanonical(body []byte, opts translate.TranslateOpts) (*canonical.Request, error) {
	var gem geminiRequest
	if err := json.Unmarshal(body, &gem); err != nil {
		return nil, fmt.Errorf("parse gemini request: %w", err)
	}

	req := &canonical.Request{
		Model: gem.Model,
	}

	// Use model from opts if body didn't have it (Gemini model is usually in URL path).
	if req.Model == "" && opts.Model != "" {
		req.Model = opts.Model
	}

	// Generation config
	if gc := gem.GenerationConfig; gc != nil {
		req.Temperature = gc.Temperature
		req.TopP = gc.TopP
		req.MaxTokens = gc.MaxOutputTokens
		req.Stop = gc.StopSequences
		if gc.ResponseMimeType == "application/json" {
			req.ResponseFormat = &canonical.ResponseFormat{Type: "json_object"}
		}
	}

	// System instruction → canonical System blocks
	if gem.SystemInstruction != nil {
		for _, part := range gem.SystemInstruction.Parts {
			if part.Text != "" {
				req.System = append(req.System, canonical.SystemBlock{Text: part.Text})
			}
		}
	}

	// Contents → canonical Messages
	for _, content := range gem.Contents {
		cm := canonical.Message{
			Role:    geminiRoleToCanonical(content.Role),
			Content: []canonical.Content{},
		}

		for _, part := range content.Parts {
			switch {
			case part.FunctionCall != nil:
				argsBytes, _ := json.Marshal(part.FunctionCall.Args)
				// Gemini doesn't provide tool call IDs natively; synthesize one.
				callID := "call_" + part.FunctionCall.Name
				cm.Content = append(cm.Content, canonical.ToolCallContent(
					callID,
					part.FunctionCall.Name,
					string(argsBytes),
				))

			case part.FunctionResponse != nil:
				callID := "call_" + part.FunctionResponse.Name
				respBytes, _ := json.Marshal(part.FunctionResponse.Response)
				cm.Content = append(cm.Content, canonical.ToolResultContent(
					callID,
					string(respBytes),
					false,
				))

			case part.InlineData != nil:
				cm.Content = append(cm.Content, canonical.ImageContent(
					part.InlineData.MimeType,
					part.InlineData.Data,
				))

			case part.Text != "":
				cm.Content = append(cm.Content, canonical.TextContent(part.Text))

			default:
				// Empty text part
				cm.Content = append(cm.Content, canonical.TextContent(""))
			}
		}

		if len(cm.Content) == 0 {
			cm.Content = append(cm.Content, canonical.TextContent(""))
		}

		req.Messages = append(req.Messages, cm)
	}

	// Tools → canonical Tools
	for _, ts := range gem.Tools {
		for _, fd := range ts.FunctionDeclarations {
			req.Tools = append(req.Tools, canonical.Tool{
				Type:        "function",
				Name:        fd.Name,
				Description: fd.Description,
				Parameters:  fd.Parameters,
			})
		}
	}

	// Tool config → canonical ToolChoice
	if gem.ToolConfig != nil && gem.ToolConfig.FunctionCallingConfig != nil {
		req.ToolChoice = geminiToolConfigToCanonical(gem.ToolConfig.FunctionCallingConfig)
	}

	return req, nil
}

// FromCanonical converts a canonical request to Gemini wire format.
func (t *Translator) FromCanonical(req *canonical.Request, opts translate.TranslateOpts) ([]byte, error) {
	gem := geminiRequest{
		Model: req.Model,
	}

	// System blocks → system_instruction
	if len(req.System) > 0 {
		si := &geminiContent{}
		for _, sb := range req.System {
			si.Parts = append(si.Parts, geminiPart{Text: sb.Text})
		}
		gem.SystemInstruction = si
	}

	// Generation config
	if req.Temperature != nil || req.TopP != nil || req.MaxTokens > 0 || len(req.Stop) > 0 || req.ResponseFormat != nil {
		gc := &geminiGenConfig{
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			MaxOutputTokens: req.MaxTokens,
			StopSequences:   req.Stop,
		}
		if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
			gc.ResponseMimeType = "application/json"
		}
		gem.GenerationConfig = gc
	}

	// Messages → contents
	// Gemini requires that tool result messages use the "user" role,
	// and tool-call-bearing messages use "model".
	for _, msg := range req.Messages {
		gc := geminiContent{
			Role: canonicalRoleToGemini(msg.Role),
		}

		for _, c := range msg.Content {
			switch c.Type {
			case canonical.TypeText:
				gc.Parts = append(gc.Parts, geminiPart{Text: c.Text})

			case canonical.TypeImage:
				if c.ImageSource != nil && c.ImageSource.Data != "" {
					gc.Parts = append(gc.Parts, geminiPart{
						InlineData: &geminiInlineData{
							MimeType: c.ImageSource.MediaType,
							Data:     c.ImageSource.Data,
						},
					})
				}

			case canonical.TypeToolCall:
				// canonical arguments is JSON string → Gemini args is parsed object
				var args map[string]any
				if err := json.Unmarshal([]byte(c.Arguments), &args); err != nil {
					args = map[string]any{}
				}
				gc.Parts = append(gc.Parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: c.ToolName,
						Args: args,
					},
				})
				// Gemini requires model role for function call parts
				gc.Role = "model"

			case canonical.TypeToolResult:
				// Gemini requires user role for function response parts
				gc.Role = "user"
				var resp map[string]any
				if err := json.Unmarshal([]byte(c.Text), &resp); err != nil {
					// If the result is not valid JSON, wrap it
					resp = map[string]any{"result": c.Text}
				}
				name := c.ToolName
				if name == "" {
					// Try to derive from tool call ID (our synthetic IDs use "call_<name>")
					name = strings.TrimPrefix(c.ToolCallID, "call_")
				}
				gc.Parts = append(gc.Parts, geminiPart{
					FunctionResponse: &geminiFuncResponse{
						Name:     name,
						Response: resp,
					},
				})

			case canonical.TypeThinking:
				// Gemini doesn't support thinking blocks — skip
				continue
			}
		}

		if len(gc.Parts) == 0 {
			gc.Parts = append(gc.Parts, geminiPart{Text: ""})
		}

		gem.Contents = append(gem.Contents, gc)
	}

	// Tools → Gemini function declarations
	if len(req.Tools) > 0 {
		ts := geminiToolSet{}
		for _, tool := range req.Tools {
			ts.FunctionDeclarations = append(ts.FunctionDeclarations, geminiFunctionDecl{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			})
		}
		gem.Tools = []geminiToolSet{ts}
	}

	// Tool choice → Gemini tool config
	if req.ToolChoice != nil {
		gem.ToolConfig = canonicalToolChoiceToGemini(req.ToolChoice)
	}

	return json.Marshal(gem)
}

// --- Role mapping helpers ---

func geminiRoleToCanonical(role string) string {
	switch role {
	case "model":
		return canonical.RoleAssistant
	case "user":
		return canonical.RoleUser
	default:
		return role
	}
}

func canonicalRoleToGemini(role string) string {
	switch role {
	case canonical.RoleAssistant:
		return "model"
	case canonical.RoleUser, canonical.RoleTool:
		return "user"
	case canonical.RoleSystem:
		// System messages should be handled via system_instruction, not here.
		// If we get one anyway, map to user.
		return "user"
	default:
		return role
	}
}

// --- Tool choice mapping ---

func geminiToolConfigToCanonical(cfg *geminiFuncCallingConfig) *canonical.ToolChoice {
	if cfg == nil {
		return nil
	}
	switch cfg.Mode {
	case "AUTO":
		return &canonical.ToolChoice{Type: "auto"}
	case "NONE":
		return &canonical.ToolChoice{Type: "none"}
	case "ANY":
		if len(cfg.AllowedFunctionNames) == 1 {
			return &canonical.ToolChoice{Type: "tool", Name: cfg.AllowedFunctionNames[0]}
		}
		return &canonical.ToolChoice{Type: "required"}
	default:
		return &canonical.ToolChoice{Type: "auto"}
	}
}

func canonicalToolChoiceToGemini(tc *canonical.ToolChoice) *geminiToolConfig {
	if tc == nil {
		return nil
	}
	cfg := &geminiFuncCallingConfig{}
	switch tc.Type {
	case "auto":
		cfg.Mode = "AUTO"
	case "none":
		cfg.Mode = "NONE"
	case "required":
		cfg.Mode = "ANY"
	case "tool":
		cfg.Mode = "ANY"
		if tc.Name != "" {
			cfg.AllowedFunctionNames = []string{tc.Name}
		}
	default:
		cfg.Mode = "AUTO"
	}
	return &geminiToolConfig{FunctionCallingConfig: cfg}
}
