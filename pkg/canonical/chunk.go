package canonical

// Chunk is a single incremental event in a streaming response.
type Chunk struct {
	ID           string `json:"id"`
	Model        string `json:"model,omitempty"`
	Delta        *Delta `json:"delta,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
	Role         string `json:"role,omitempty"`
}

// Delta carries the incremental content of a streaming chunk.
type Delta struct {
	Text       string `json:"text,omitempty"`
	Thinking   string `json:"thinking,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
}

// Usage tracks token consumption for a completed request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// Claude-specific decomposed tokens
	InputTokens         int `json:"input_tokens,omitempty"`
	OutputTokens        int `json:"output_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_input_tokens,omitempty"`
}
