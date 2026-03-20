package gemini

import (
	"encoding/json"
	"fmt"
	"sage-router/internal/translate"
	"sage-router/pkg/canonical"
)

// Gemini streaming response types.
// Gemini streams newline-delimited JSON objects (or a JSON array of candidates).
// Each chunk has the shape:
//
//	{
//	  "candidates": [{"content": {"parts": [...]}, "finishReason": "..."}],
//	  "usageMetadata": {"promptTokenCount": N, "candidatesTokenCount": N, "totalTokenCount": N}
//	}
type geminiStreamChunk struct {
	Candidates    []geminiCandidate  `json:"candidates,omitempty"`
	UsageMetadata *geminiUsageMeta   `json:"usageMetadata,omitempty"`
}

type geminiCandidate struct {
	Content      *geminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        int            `json:"index"`
}

type geminiUsageMeta struct {
	PromptTokenCount     int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount      int `json:"totalTokenCount,omitempty"`
}

// StreamChunkToCanonical converts a Gemini streaming chunk to canonical chunks.
func (t *Translator) StreamChunkToCanonical(data []byte, state *translate.StreamState) ([]canonical.Chunk, error) {
	// Gemini may wrap the stream in a JSON array — strip leading '[' or ',' and trailing ']'
	trimmed := trimArrayWrapper(data)

	var chunk geminiStreamChunk
	if err := json.Unmarshal(trimmed, &chunk); err != nil {
		return nil, fmt.Errorf("parse gemini stream chunk: %w", err)
	}

	if state.MessageID == "" {
		state.MessageID = "gemini-msg"
	}
	if state.Model == "" && state.Custom != nil {
		if m, ok := state.Custom["model"].(string); ok {
			state.Model = m
		}
	}

	var results []canonical.Chunk

	for _, candidate := range chunk.Candidates {
		if candidate.Content != nil {
			// Emit role on first content if not yet announced
			if _, announced := state.Custom["role_announced"]; !announced {
				role := candidate.Content.Role
				if role == "" {
					role = "model"
				}
				results = append(results, canonical.Chunk{
					ID:    state.MessageID,
					Model: state.Model,
					Role:  geminiRoleToCanonical(role),
				})
				state.Custom["role_announced"] = true
			}

			for _, part := range candidate.Content.Parts {
				switch {
				case part.FunctionCall != nil:
					argsBytes, _ := json.Marshal(part.FunctionCall.Args)
					callID := "call_" + part.FunctionCall.Name
					// Emit tool call start + full arguments in one chunk
					results = append(results, canonical.Chunk{
						ID:    state.MessageID,
						Model: state.Model,
						Delta: &canonical.Delta{
							ToolCallID: callID,
							ToolName:   part.FunctionCall.Name,
							Arguments:  string(argsBytes),
						},
					})

				case part.Text != "":
					results = append(results, canonical.Chunk{
						ID:    state.MessageID,
						Model: state.Model,
						Delta: &canonical.Delta{Text: part.Text},
					})

				default:
					// Empty text part — skip
				}
			}
		}

		// Finish reason
		if candidate.FinishReason != "" {
			reason := geminiFinishReasonToCanonical(candidate.FinishReason)
			state.FinishReason = reason
			results = append(results, canonical.Chunk{
				ID:           state.MessageID,
				Model:        state.Model,
				FinishReason: reason,
			})
		}
	}

	// Usage metadata
	if chunk.UsageMetadata != nil {
		usage := &canonical.Usage{
			PromptTokens:     chunk.UsageMetadata.PromptTokenCount,
			CompletionTokens: chunk.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      chunk.UsageMetadata.TotalTokenCount,
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

// CanonicalToStreamChunk converts a canonical chunk to Gemini streaming format.
func (t *Translator) CanonicalToStreamChunk(chunk canonical.Chunk, state *translate.StreamState) ([]byte, error) {
	sc := geminiStreamChunk{}

	// Build a candidate from the chunk
	candidate := geminiCandidate{Index: 0}
	hasCandidateContent := false

	// Role announcement
	if chunk.Role != "" {
		state.MessageID = chunk.ID
		state.Model = chunk.Model
		candidate.Content = &geminiContent{
			Role:  canonicalRoleToGemini(chunk.Role),
			Parts: []geminiPart{},
		}
		hasCandidateContent = true
	}

	// Delta content
	if chunk.Delta != nil {
		if candidate.Content == nil {
			candidate.Content = &geminiContent{
				Role:  "model",
				Parts: []geminiPart{},
			}
		}
		hasCandidateContent = true

		if chunk.Delta.Text != "" {
			candidate.Content.Parts = append(candidate.Content.Parts, geminiPart{
				Text: chunk.Delta.Text,
			})
		}

		if chunk.Delta.ToolCallID != "" || chunk.Delta.Arguments != "" {
			// Tool call — Gemini expects the full functionCall in one piece.
			// In streaming canonical, the first chunk has ToolCallID+ToolName,
			// subsequent chunks have Arguments fragments. We accumulate.
			if chunk.Delta.ToolCallID != "" {
				// Start of a new tool call — store in state for accumulation
				state.ToolCalls[chunk.Delta.ToolCallID] = &translate.ToolCallAccumulator{
					ID:        chunk.Delta.ToolCallID,
					Name:      chunk.Delta.ToolName,
					Arguments: chunk.Delta.Arguments,
				}
				state.Custom["activeToolCallID"] = chunk.Delta.ToolCallID
			} else if chunk.Delta.Arguments != "" {
				// Continuation of existing tool call arguments
				activeID, _ := state.Custom["activeToolCallID"].(string)
				if acc, ok := state.ToolCalls[activeID]; ok {
					acc.Arguments += chunk.Delta.Arguments
				}
			}

			// Don't emit the functionCall part until we have complete args.
			// We'll emit on finish or when a new tool call starts.
			// For now, emit a text placeholder to keep the stream alive.
			// Actually, in practice Gemini sends complete functionCalls in one chunk,
			// so for outbound we should also. We emit nothing until finish.
			hasCandidateContent = false
			candidate.Content = nil
		}

		if chunk.Delta.Thinking != "" {
			// Gemini doesn't support thinking — skip
			hasCandidateContent = false
			candidate.Content = nil
		}
	}

	// Finish reason
	if chunk.FinishReason != "" {
		reason := canonicalFinishReasonToGemini(chunk.FinishReason)
		candidate.FinishReason = reason
		hasCandidateContent = true

		// Flush any accumulated tool calls
		if len(state.ToolCalls) > 0 {
			if candidate.Content == nil {
				candidate.Content = &geminiContent{
					Role:  "model",
					Parts: []geminiPart{},
				}
			}
			for _, acc := range state.ToolCalls {
				var args map[string]any
				if err := json.Unmarshal([]byte(acc.Arguments), &args); err != nil {
					args = map[string]any{}
				}
				candidate.Content.Parts = append(candidate.Content.Parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: acc.Name,
						Args: args,
					},
				})
			}
			// Clear accumulated tool calls
			state.ToolCalls = make(map[string]*translate.ToolCallAccumulator)
		}
	}

	if hasCandidateContent {
		sc.Candidates = []geminiCandidate{candidate}
	}

	// Usage
	if chunk.Usage != nil {
		sc.UsageMetadata = &geminiUsageMeta{
			PromptTokenCount:     chunk.Usage.PromptTokens,
			CandidatesTokenCount: chunk.Usage.CompletionTokens,
			TotalTokenCount:      chunk.Usage.TotalTokens,
		}
	}

	// If nothing to emit, return nil
	if len(sc.Candidates) == 0 && sc.UsageMetadata == nil {
		return nil, nil
	}

	return json.Marshal(sc)
}

// --- Helpers ---

// trimArrayWrapper strips JSON array delimiters that Gemini may wrap streaming chunks in.
// Gemini sometimes sends: [ {chunk1}, \n{chunk2}, \n{chunk3} ]
// We strip leading '[', ',', whitespace and trailing ']'.
func trimArrayWrapper(data []byte) []byte {
	start := 0
	end := len(data)

	// Skip leading whitespace, '[', ','
	for start < end {
		b := data[start]
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '[' || b == ',' {
			start++
		} else {
			break
		}
	}

	// Skip trailing whitespace, ']', ','
	for end > start {
		b := data[end-1]
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == ']' || b == ',' {
			end--
		} else {
			break
		}
	}

	if start >= end {
		return data
	}
	return data[start:end]
}

func geminiFinishReasonToCanonical(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "stop"
	case "RECITATION":
		return "stop"
	default:
		return "stop"
	}
}

func canonicalFinishReasonToGemini(reason string) string {
	switch reason {
	case "stop":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "tool_calls":
		return "STOP"
	default:
		return "STOP"
	}
}
