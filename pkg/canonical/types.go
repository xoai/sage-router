package canonical

import (
	"encoding/json"
	"time"
)

// Format identifies an API wire format.
type Format string

const (
	FormatOpenAI    Format = "openai"
	FormatClaude    Format = "claude"
	FormatGemini    Format = "gemini"
	FormatResponses Format = "openai-responses"
	FormatKiro      Format = "kiro"
	FormatCursor    Format = "cursor"
	FormatOllama    Format = "ollama"
)

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Content block types.
const (
	TypeText       = "text"
	TypeImage      = "image"
	TypeToolCall   = "tool_call"
	TypeToolResult = "tool_result"
	TypeThinking   = "thinking"
)

// Request is the canonical intermediate representation for all chat/completion requests.
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	// System prompt — separated from messages because Claude treats it specially.
	System []SystemBlock `json:"system,omitempty"`

	// Tool definitions
	Tools      []Tool      `json:"tools,omitempty"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	// Generation parameters
	Stream      bool     `json:"stream"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Stop        []string `json:"stop,omitempty"`

	// Extended thinking
	Thinking *ThinkingConfig `json:"thinking,omitempty"`

	// Response format constraint (JSON mode)
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	// Internal metadata — never serialized to upstream providers.
	Meta *RequestMeta `json:"-"`
}

// SystemBlock is a single block within the system prompt.
type SystemBlock struct {
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl for Claude's prompt caching feature.
type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// RequestMeta holds internal routing data that never leaves sage-router.
type RequestMeta struct {
	RequestID    string
	SourceFormat Format
	Endpoint     string
	UserAgent    string
	ReceivedAt   time.Time
}

// Message is a single conversational turn.
// INVARIANT: Content is ALWAYS an array, never a bare string.
type Message struct {
	Role    string    `json:"role"`
	Content []Content `json:"content"`
}

// Content is a polymorphic content block within a message.
type Content struct {
	Type string `json:"type"`

	// Text & Thinking
	Text string `json:"text,omitempty"`

	// Image
	ImageSource *ImageSource `json:"image_source,omitempty"`

	// Tool Call (assistant → requests tool execution)
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	// Arguments is ALWAYS a JSON string, never a parsed object.
	Arguments string `json:"arguments,omitempty"`

	// Tool Result (tool → reports result back)
	IsError bool `json:"is_error,omitempty"`

	// Cache control (provider-specific, passed through for detection)
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// ImageSource represents an image in a message.
type ImageSource struct {
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Tool describes a callable function the model can invoke.
type Tool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolChoice controls how the model selects tools.
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// ThinkingConfig controls Claude's extended thinking feature.
type ThinkingConfig struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// ResponseFormat constrains the model's output format.
type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

// TextContent creates a text content block.
func TextContent(text string) Content {
	return Content{Type: TypeText, Text: text}
}

// ThinkingContent creates a thinking content block.
func ThinkingContent(text string) Content {
	return Content{Type: TypeThinking, Text: text}
}

// ImageContent creates an image content block.
func ImageContent(mediaType, data string) Content {
	return Content{
		Type:        TypeImage,
		ImageSource: &ImageSource{MediaType: mediaType, Data: data},
	}
}

// ImageURLContent creates an image URL content block.
func ImageURLContent(url string) Content {
	return Content{
		Type:        TypeImage,
		ImageSource: &ImageSource{URL: url},
	}
}

// ToolCallContent creates a tool call content block.
func ToolCallContent(id, name, args string) Content {
	return Content{
		Type:       TypeToolCall,
		ToolCallID: id,
		ToolName:   name,
		Arguments:  args,
	}
}

// ToolResultContent creates a tool result content block.
func ToolResultContent(id, text string, isError bool) Content {
	return Content{
		Type:       TypeToolResult,
		ToolCallID: id,
		Text:       text,
		IsError:    isError,
	}
}
