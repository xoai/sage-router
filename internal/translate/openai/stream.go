package openai

import (
	"encoding/json"
	"fmt"
	"sage-router/internal/translate"
	"sage-router/pkg/canonical"
)

// OpenAI streaming response types
type streamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *streamUsage   `json:"usage,omitempty"`
}

type streamChoice struct {
	Index        int          `json:"index"`
	Delta        streamDelta  `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
}

type streamDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          *string          `json:"content,omitempty"`
	ReasoningContent *string          `json:"reasoning_content,omitempty"`
	ToolCalls        []streamToolCall `json:"tool_calls,omitempty"`
}

type streamToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function streamFunctionCall `json:"function"`
}

type streamFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type streamUsage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type promptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

func (t *Translator) StreamChunkToCanonical(data []byte, state *translate.StreamState) ([]canonical.Chunk, error) {
	var chunk streamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("parse openai stream chunk: %w", err)
	}

	if state.MessageID == "" {
		state.MessageID = chunk.ID
		state.Model = chunk.Model
	}

	var results []canonical.Chunk

	for _, choice := range chunk.Choices {
		delta := choice.Delta

		// Role announcement
		if delta.Role != "" {
			results = append(results, canonical.Chunk{
				ID:    state.MessageID,
				Model: state.Model,
				Role:  delta.Role,
			})
		}

		// Text content
		if delta.Content != nil && *delta.Content != "" {
			results = append(results, canonical.Chunk{
				ID:    state.MessageID,
				Model: state.Model,
				Delta: &canonical.Delta{Text: *delta.Content},
			})
		}

		// Reasoning/thinking content
		if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
			results = append(results, canonical.Chunk{
				ID:    state.MessageID,
				Model: state.Model,
				Delta: &canonical.Delta{Thinking: *delta.ReasoningContent},
			})
		}

		// Tool calls
		for _, tc := range delta.ToolCalls {
			d := &canonical.Delta{}
			if tc.ID != "" {
				d.ToolCallID = tc.ID
				d.ToolName = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				d.Arguments = tc.Function.Arguments
			}
			results = append(results, canonical.Chunk{
				ID:    state.MessageID,
				Model: state.Model,
				Delta: d,
			})
		}

		// Finish reason
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			state.FinishReason = *choice.FinishReason
			results = append(results, canonical.Chunk{
				ID:           state.MessageID,
				Model:        state.Model,
				FinishReason: *choice.FinishReason,
			})
		}
	}

	// Usage
	if chunk.Usage != nil {
		usage := &canonical.Usage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
			TotalTokens:      chunk.Usage.TotalTokens,
		}
		if chunk.Usage.PromptTokensDetails != nil {
			usage.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		state.Usage = usage
		results = append(results, canonical.Chunk{
			ID:    state.MessageID,
			Model: state.Model,
			Usage: usage,
		})
	}

	return results, nil
}

func (t *Translator) CanonicalToStreamChunk(chunk canonical.Chunk, state *translate.StreamState) ([]byte, error) {
	sc := streamChunk{
		ID:     chunk.ID,
		Object: "chat.completion.chunk",
		Model:  chunk.Model,
	}

	choice := streamChoice{Index: 0}

	if chunk.Role != "" {
		choice.Delta.Role = chunk.Role
	}

	if chunk.Delta != nil {
		if chunk.Delta.Text != "" {
			choice.Delta.Content = &chunk.Delta.Text
		}
		if chunk.Delta.Thinking != "" {
			choice.Delta.ReasoningContent = &chunk.Delta.Thinking
		}
		if chunk.Delta.ToolCallID != "" || chunk.Delta.Arguments != "" {
			tc := streamToolCall{
				Index: state.BlockIndex,
				Function: streamFunctionCall{
					Arguments: chunk.Delta.Arguments,
				},
			}
			if chunk.Delta.ToolCallID != "" {
				tc.ID = chunk.Delta.ToolCallID
				tc.Type = "function"
				tc.Function.Name = chunk.Delta.ToolName
				state.BlockIndex++
			}
			choice.Delta.ToolCalls = []streamToolCall{tc}
		}
	}

	if chunk.FinishReason != "" {
		reason := chunk.FinishReason
		choice.FinishReason = &reason
	}

	sc.Choices = []streamChoice{choice}

	if chunk.Usage != nil {
		sc.Usage = &streamUsage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
			TotalTokens:      chunk.Usage.TotalTokens,
		}
	}

	return json.Marshal(sc)
}
