package openai

import (
	"encoding/json"
	"fmt"
	"sage-router/pkg/canonical"
)

// Non-streaming response types
type openAIResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []responseChoice `json:"choices"`
	Usage   *streamUsage     `json:"usage,omitempty"`
}

type responseChoice struct {
	Index        int            `json:"index"`
	Message      openAIMessage  `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

// ParseResponse converts a non-streaming OpenAI response into canonical chunks.
func ParseResponse(data []byte) (*canonical.Request, *canonical.Usage, error) {
	var resp openAIResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, nil, fmt.Errorf("parse openai response: %w", err)
	}

	var usage *canonical.Usage
	if resp.Usage != nil {
		usage = &canonical.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
	}

	// For non-streaming, we extract the response content
	// This is used for usage tracking, not for proxying
	return nil, usage, nil
}
