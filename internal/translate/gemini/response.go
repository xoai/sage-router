package gemini

import (
	"encoding/json"
	"fmt"
	"sage-router/pkg/canonical"
)

// geminiResponse is the non-streaming Gemini response envelope.
// It reuses geminiCandidate and geminiUsageMeta defined in stream.go.
type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates,omitempty"`
	UsageMetadata *geminiUsageMeta  `json:"usageMetadata,omitempty"`
	ModelVersion  string            `json:"modelVersion,omitempty"`
}

// ParseResponse extracts usage metadata and message content from a non-streaming Gemini response.
// This mirrors the pattern in openai/response.go — it returns usage info for tracking.
func ParseResponse(data []byte) (*canonical.Message, *canonical.Usage, error) {
	var resp geminiResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, nil, fmt.Errorf("parse gemini response: %w", err)
	}

	// Extract usage
	var usage *canonical.Usage
	if resp.UsageMetadata != nil {
		usage = &canonical.Usage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		}
	}

	// Extract the first candidate's content into a canonical Message
	if len(resp.Candidates) == 0 {
		return nil, usage, nil
	}

	candidate := resp.Candidates[0]
	if candidate.Content == nil {
		return nil, usage, nil
	}

	msg := &canonical.Message{
		Role:    geminiRoleToCanonical(candidate.Content.Role),
		Content: []canonical.Content{},
	}

	for _, part := range candidate.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			argsBytes, _ := json.Marshal(part.FunctionCall.Args)
			callID := "call_" + part.FunctionCall.Name
			msg.Content = append(msg.Content, canonical.ToolCallContent(
				callID,
				part.FunctionCall.Name,
				string(argsBytes),
			))

		case part.InlineData != nil:
			msg.Content = append(msg.Content, canonical.ImageContent(
				part.InlineData.MimeType,
				part.InlineData.Data,
			))

		case part.Text != "":
			msg.Content = append(msg.Content, canonical.TextContent(part.Text))

		default:
			msg.Content = append(msg.Content, canonical.TextContent(""))
		}
	}

	if len(msg.Content) == 0 {
		msg.Content = append(msg.Content, canonical.TextContent(""))
	}

	return msg, usage, nil
}

// ParseFinishReason extracts the finish reason from a non-streaming Gemini response.
func ParseFinishReason(data []byte) string {
	var resp geminiResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return ""
	}
	if len(resp.Candidates) == 0 {
		return ""
	}
	return geminiFinishReasonToCanonical(resp.Candidates[0].FinishReason)
}
