package claude

import (
	"encoding/json"
	"fmt"
	"sage-router/internal/translate"
	"sage-router/pkg/canonical"
	"strings"
)

// Translator handles Claude ↔ Canonical translation.
type Translator struct{}

func New() *Translator {
	return &Translator{}
}

func (t *Translator) Format() canonical.Format {
	return canonical.FormatClaude
}

func (t *Translator) DetectInbound(endpoint string, body []byte) bool {
	if strings.Contains(endpoint, "/v1/messages") {
		return true
	}
	// Body-shape: Claude has "system" as separate field with messages array
	var probe struct {
		System   json.RawMessage `json:"system"`
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		return probe.System != nil && probe.Messages != nil
	}
	return false
}

// Claude wire types
type claudeRequest struct {
	Model      string             `json:"model"`
	System     any                `json:"system,omitempty"`
	Messages   []claudeMessage    `json:"messages"`
	Tools      []claudeTool       `json:"tools,omitempty"`
	ToolChoice *claudeToolChoice  `json:"tool_choice,omitempty"`
	MaxTokens  int                `json:"max_tokens"`
	Stream     bool               `json:"stream,omitempty"`
	Thinking   *claudeThinking    `json:"thinking,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	TopP       *float64           `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []claudeContentBlock
}

type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     any             `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *claudeImgSource `json:"source,omitempty"`
	CacheControl *canonical.CacheControl `json:"cache_control,omitempty"`
}

type claudeImgSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type claudeTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Type        string          `json:"type,omitempty"`
	CacheControl *canonical.CacheControl `json:"cache_control,omitempty"`
}

type claudeToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type claudeThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type claudeSystemBlock struct {
	Type         string                  `json:"type"`
	Text         string                  `json:"text"`
	CacheControl *canonical.CacheControl `json:"cache_control,omitempty"`
}

func (t *Translator) ToCanonical(body []byte, opts translate.TranslateOpts) (*canonical.Request, error) {
	var claude claudeRequest
	if err := json.Unmarshal(body, &claude); err != nil {
		return nil, fmt.Errorf("parse claude request: %w", err)
	}

	req := &canonical.Request{
		Model:       claude.Model,
		Stream:      claude.Stream,
		MaxTokens:   claude.MaxTokens,
		Temperature: claude.Temperature,
		TopP:        claude.TopP,
		Stop:        claude.StopSequences,
	}

	// Parse system prompt
	req.System = parseClaudeSystem(claude.System)

	// Parse thinking
	if claude.Thinking != nil {
		req.Thinking = &canonical.ThinkingConfig{
			Type:         claude.Thinking.Type,
			BudgetTokens: claude.Thinking.BudgetTokens,
		}
	}

	// Parse messages
	for _, msg := range claude.Messages {
		cm := canonical.Message{
			Role:    msg.Role,
			Content: []canonical.Content{},
		}

		blocks := parseClaudeContent(msg.Content)
		for _, block := range blocks {
			switch block.Type {
			case "text":
				cm.Content = append(cm.Content, canonical.TextContent(block.Text))

			case "thinking", "redacted_thinking":
				cm.Content = append(cm.Content, canonical.ThinkingContent(block.Thinking))

			case "image":
				if block.Source != nil {
					cm.Content = append(cm.Content, canonical.ImageContent(
						block.Source.MediaType,
						block.Source.Data,
					))
				}

			case "tool_use":
				// CRITICAL: Claude input is parsed object → canonical is JSON string
				argsBytes, _ := json.Marshal(block.Input)
				cm.Content = append(cm.Content, canonical.ToolCallContent(
					block.ID,
					block.Name,
					string(argsBytes),
				))

			case "tool_result":
				resultText := extractToolResultText(block.Content)
				cm.Content = append(cm.Content, canonical.ToolResultContent(
					block.ToolUseID,
					resultText,
					block.IsError,
				))

			case "server_tool_use", "server_tool_result":
				// Pass through as tool calls/results
				if block.Type == "server_tool_use" {
					argsBytes, _ := json.Marshal(block.Input)
					cm.Content = append(cm.Content, canonical.ToolCallContent(
						block.ID,
						block.Name,
						string(argsBytes),
					))
				}
			}
		}

		if len(cm.Content) == 0 {
			cm.Content = append(cm.Content, canonical.TextContent(""))
		}

		req.Messages = append(req.Messages, cm)
	}

	// Parse tools
	for _, tool := range claude.Tools {
		req.Tools = append(req.Tools, canonical.Tool{
			Type:        tool.Type,
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.InputSchema,
		})
	}

	// Parse tool choice
	if claude.ToolChoice != nil {
		req.ToolChoice = mapClaudeToolChoiceToCanonical(claude.ToolChoice)
	}

	return req, nil
}

func parseClaudeSystem(system any) []canonical.SystemBlock {
	if system == nil {
		return nil
	}

	switch v := system.(type) {
	case string:
		return []canonical.SystemBlock{{Text: v}}
	case []any:
		var blocks []canonical.SystemBlock
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				sb := canonical.SystemBlock{}
				if text, ok := m["text"].(string); ok {
					sb.Text = text
				}
				if cc, ok := m["cache_control"].(map[string]any); ok {
					sb.CacheControl = &canonical.CacheControl{}
					if t, ok := cc["type"].(string); ok {
						sb.CacheControl.Type = t
					}
				}
				blocks = append(blocks, sb)
			}
		}
		return blocks
	}

	// Try to unmarshal as JSON
	data, err := json.Marshal(system)
	if err != nil {
		return nil
	}
	var str string
	if json.Unmarshal(data, &str) == nil {
		return []canonical.SystemBlock{{Text: str}}
	}
	var blocks []claudeSystemBlock
	if json.Unmarshal(data, &blocks) == nil {
		var result []canonical.SystemBlock
		for _, b := range blocks {
			result = append(result, canonical.SystemBlock{
				Text:         b.Text,
				CacheControl: b.CacheControl,
			})
		}
		return result
	}
	return nil
}

func parseClaudeContent(content any) []claudeContentBlock {
	switch v := content.(type) {
	case string:
		return []claudeContentBlock{{Type: "text", Text: v}}
	case []any:
		var blocks []claudeContentBlock
		data, _ := json.Marshal(v)
		json.Unmarshal(data, &blocks)
		return blocks
	}
	data, err := json.Marshal(content)
	if err != nil {
		return nil
	}
	var blocks []claudeContentBlock
	if json.Unmarshal(data, &blocks) == nil {
		return blocks
	}
	var str string
	if json.Unmarshal(data, &str) == nil {
		return []claudeContentBlock{{Type: "text", Text: str}}
	}
	return nil
}

func extractToolResultText(content any) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var texts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["type"].(string); ok && t == "text" {
					if text, ok := m["text"].(string); ok {
						texts = append(texts, text)
					}
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return fmt.Sprintf("%v", content)
}

func mapClaudeToolChoiceToCanonical(tc *claudeToolChoice) *canonical.ToolChoice {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "any":
		return &canonical.ToolChoice{Type: "required"}
	case "tool":
		return &canonical.ToolChoice{Type: "tool", Name: tc.Name}
	case "auto":
		return &canonical.ToolChoice{Type: "auto"}
	case "none":
		return &canonical.ToolChoice{Type: "none"}
	default:
		return &canonical.ToolChoice{Type: tc.Type}
	}
}

func (t *Translator) FromCanonical(req *canonical.Request, opts translate.TranslateOpts) ([]byte, error) {
	claude := claudeRequest{
		Model:     req.Model,
		Stream:    req.Stream,
		MaxTokens: req.MaxTokens,
	}

	if claude.MaxTokens == 0 {
		claude.MaxTokens = 8192 // Claude requires max_tokens
	}

	claude.Temperature = req.Temperature
	claude.TopP = req.TopP
	claude.StopSequences = req.Stop

	// System prompt
	if len(req.System) > 0 {
		if len(req.System) == 1 && req.System[0].CacheControl == nil {
			claude.System = req.System[0].Text
		} else {
			var blocks []claudeSystemBlock
			for _, sb := range req.System {
				blocks = append(blocks, claudeSystemBlock{
					Type:         "text",
					Text:         sb.Text,
					CacheControl: sb.CacheControl,
				})
			}
			claude.System = blocks
		}
	}

	// Thinking
	if req.Thinking != nil {
		claude.Thinking = &claudeThinking{
			Type:         req.Thinking.Type,
			BudgetTokens: req.Thinking.BudgetTokens,
		}
		// Ensure max_tokens > budget_tokens
		if claude.Thinking.BudgetTokens > 0 && claude.MaxTokens <= claude.Thinking.BudgetTokens {
			claude.MaxTokens = claude.Thinking.BudgetTokens + 1024
		}
	}

	// Convert messages
	for _, msg := range req.Messages {
		cm := claudeMessage{Role: msg.Role}
		var blocks []claudeContentBlock

		for _, c := range msg.Content {
			switch c.Type {
			case canonical.TypeText:
				blocks = append(blocks, claudeContentBlock{
					Type: "text",
					Text: c.Text,
				})

			case canonical.TypeThinking:
				blocks = append(blocks, claudeContentBlock{
					Type:     "thinking",
					Thinking: c.Text,
					// Signature must be added by the caller or omitted for new generation
				})

			case canonical.TypeImage:
				if c.ImageSource != nil {
					blocks = append(blocks, claudeContentBlock{
						Type: "image",
						Source: &claudeImgSource{
							Type:      "base64",
							MediaType: c.ImageSource.MediaType,
							Data:      c.ImageSource.Data,
						},
					})
				}

			case canonical.TypeToolCall:
				// CRITICAL: canonical arguments is JSON string → Claude input is parsed object
				var input any
				if err := json.Unmarshal([]byte(c.Arguments), &input); err != nil {
					input = map[string]any{}
				}
				blocks = append(blocks, claudeContentBlock{
					Type:  "tool_use",
					ID:    c.ToolCallID,
					Name:  c.ToolName,
					Input: input,
				})

			case canonical.TypeToolResult:
				block := claudeContentBlock{
					Type:      "tool_result",
					ToolUseID: c.ToolCallID,
					IsError:   c.IsError,
				}
				if c.Text != "" {
					block.Content = c.Text
				}
				blocks = append(blocks, block)
			}
		}

		if len(blocks) == 1 && blocks[0].Type == "text" {
			cm.Content = blocks[0].Text
		} else {
			cm.Content = blocks
		}

		claude.Messages = append(claude.Messages, cm)
	}

	// Merge consecutive same-role messages (Claude requirement)
	claude.Messages = mergeConsecutiveMessages(claude.Messages)

	// Ensure alternating roles (Claude requirement)
	claude.Messages = ensureAlternating(claude.Messages)

	// Convert tools
	for _, tool := range req.Tools {
		ct := claudeTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Parameters,
		}
		if tool.Type != "" && tool.Type != "function" {
			ct.Type = tool.Type
		}
		claude.Tools = append(claude.Tools, ct)
	}

	// Convert tool choice
	if req.ToolChoice != nil {
		claude.ToolChoice = mapCanonicalToolChoiceToClaude(req.ToolChoice)
	}

	return json.Marshal(claude)
}

func mapCanonicalToolChoiceToClaude(tc *canonical.ToolChoice) *claudeToolChoice {
	switch tc.Type {
	case "required":
		return &claudeToolChoice{Type: "any"}
	case "tool":
		return &claudeToolChoice{Type: "tool", Name: tc.Name}
	case "auto":
		return &claudeToolChoice{Type: "auto"}
	case "none":
		return &claudeToolChoice{Type: "none"}
	default:
		return &claudeToolChoice{Type: tc.Type}
	}
}

func mergeConsecutiveMessages(msgs []claudeMessage) []claudeMessage {
	if len(msgs) <= 1 {
		return msgs
	}

	var merged []claudeMessage
	for _, msg := range msgs {
		if len(merged) > 0 && merged[len(merged)-1].Role == msg.Role {
			// Merge content blocks
			lastBlocks := contentToBlocks(merged[len(merged)-1].Content)
			newBlocks := contentToBlocks(msg.Content)
			lastBlocks = append(lastBlocks, newBlocks...)
			merged[len(merged)-1].Content = lastBlocks
		} else {
			merged = append(merged, msg)
		}
	}
	return merged
}

func contentToBlocks(content any) []claudeContentBlock {
	switch v := content.(type) {
	case string:
		return []claudeContentBlock{{Type: "text", Text: v}}
	case []claudeContentBlock:
		return v
	case []any:
		data, _ := json.Marshal(v)
		var blocks []claudeContentBlock
		json.Unmarshal(data, &blocks)
		return blocks
	}
	return nil
}

func ensureAlternating(msgs []claudeMessage) []claudeMessage {
	if len(msgs) == 0 {
		return msgs
	}

	var result []claudeMessage
	for i, msg := range msgs {
		if i > 0 && msg.Role == result[len(result)-1].Role {
			// Insert a placeholder of the opposite role
			if msg.Role == "user" {
				result = append(result, claudeMessage{
					Role:    "assistant",
					Content: "...",
				})
			} else {
				result = append(result, claudeMessage{
					Role:    "user",
					Content: "...",
				})
			}
		}
		result = append(result, msg)
	}

	// Ensure first message is user (Claude requirement)
	if len(result) > 0 && result[0].Role != "user" {
		result = append([]claudeMessage{{
			Role:    "user",
			Content: "...",
		}}, result...)
	}

	return result
}
