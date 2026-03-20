package claude

import (
	"encoding/json"
	"fmt"
	"sage-router/internal/translate"
	"sage-router/pkg/canonical"
)

// Claude SSE event types
type claudeEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"-"`
}

type messageStartData struct {
	Type    string `json:"type"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Role  string `json:"role"`
		Usage *struct {
			InputTokens int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
		} `json:"usage,omitempty"`
	} `json:"message"`
}

type contentBlockStartData struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
		Text string `json:"text,omitempty"`
	} `json:"content_block"`
}

type contentBlockDeltaData struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		Thinking    string `json:"thinking,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
	} `json:"delta"`
}

type contentBlockStopData struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type messageDeltaData struct {
	Type  string `json:"type"`
	Delta struct {
		StopReason string `json:"stop_reason,omitempty"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

func (t *Translator) StreamChunkToCanonical(data []byte, state *translate.StreamState) ([]canonical.Chunk, error) {
	// Parse the event type
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, fmt.Errorf("parse claude event type: %w", err)
	}

	switch event.Type {
	case "message_start":
		return t.handleMessageStart(data, state)
	case "content_block_start":
		return t.handleContentBlockStart(data, state)
	case "content_block_delta":
		return t.handleContentBlockDelta(data, state)
	case "content_block_stop":
		return t.handleContentBlockStop(data, state)
	case "message_delta":
		return t.handleMessageDelta(data, state)
	case "message_stop":
		return t.handleMessageStop(state)
	case "ping":
		return nil, nil // keepalive, skip
	case "error":
		return nil, fmt.Errorf("claude stream error: %s", string(data))
	default:
		return nil, nil // unknown event, skip
	}
}

func (t *Translator) handleMessageStart(data []byte, state *translate.StreamState) ([]canonical.Chunk, error) {
	var msg messageStartData
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}

	state.MessageID = msg.Message.ID
	state.Model = msg.Message.Model

	// Store initial usage if present
	if msg.Message.Usage != nil {
		if state.Usage == nil {
			state.Usage = &canonical.Usage{}
		}
		state.Usage.InputTokens = msg.Message.Usage.InputTokens
		state.Usage.PromptTokens = msg.Message.Usage.InputTokens
		state.Usage.CacheReadTokens = msg.Message.Usage.CacheReadInputTokens
		state.Usage.CacheCreationTokens = msg.Message.Usage.CacheCreationInputTokens
	}

	return []canonical.Chunk{{
		ID:    state.MessageID,
		Model: state.Model,
		Role:  "assistant",
	}}, nil
}

func (t *Translator) handleContentBlockStart(data []byte, state *translate.StreamState) ([]canonical.Chunk, error) {
	var block contentBlockStartData
	if err := json.Unmarshal(data, &block); err != nil {
		return nil, err
	}

	state.BlockIndex = block.Index

	switch block.ContentBlock.Type {
	case "text":
		state.InThinking = false
		state.InToolCall = false
		state.Custom["activeBlockType"] = "text"

	case "thinking":
		state.InThinking = true
		state.InToolCall = false
		state.Custom["activeBlockType"] = "thinking"

	case "tool_use":
		state.InThinking = false
		state.InToolCall = true
		state.Custom["activeBlockType"] = "tool_use"
		// Emit initial tool call chunk with ID and name
		return []canonical.Chunk{{
			ID:    state.MessageID,
			Model: state.Model,
			Delta: &canonical.Delta{
				ToolCallID: block.ContentBlock.ID,
				ToolName:   block.ContentBlock.Name,
			},
		}}, nil

	case "server_tool_use":
		state.Custom["activeBlockType"] = "server_tool"
		// Skip server tool blocks
	}

	return nil, nil
}

func (t *Translator) handleContentBlockDelta(data []byte, state *translate.StreamState) ([]canonical.Chunk, error) {
	var delta contentBlockDeltaData
	if err := json.Unmarshal(data, &delta); err != nil {
		return nil, err
	}

	blockType, _ := state.Custom["activeBlockType"].(string)
	if blockType == "server_tool" {
		return nil, nil // skip server tool deltas
	}

	switch delta.Delta.Type {
	case "text_delta":
		if delta.Delta.Text == "" {
			return nil, nil
		}
		return []canonical.Chunk{{
			ID:    state.MessageID,
			Model: state.Model,
			Delta: &canonical.Delta{Text: delta.Delta.Text},
		}}, nil

	case "thinking_delta":
		if delta.Delta.Thinking == "" {
			return nil, nil
		}
		return []canonical.Chunk{{
			ID:    state.MessageID,
			Model: state.Model,
			Delta: &canonical.Delta{Thinking: delta.Delta.Thinking},
		}}, nil

	case "input_json_delta":
		if delta.Delta.PartialJSON == "" {
			return nil, nil
		}
		return []canonical.Chunk{{
			ID:    state.MessageID,
			Model: state.Model,
			Delta: &canonical.Delta{Arguments: delta.Delta.PartialJSON},
		}}, nil

	case "signature_delta":
		// Skip signature deltas - provider-specific
		return nil, nil
	}

	return nil, nil
}

func (t *Translator) handleContentBlockStop(data []byte, state *translate.StreamState) ([]canonical.Chunk, error) {
	state.Custom["activeBlockType"] = ""
	state.InThinking = false
	state.InToolCall = false
	return nil, nil
}

func (t *Translator) handleMessageDelta(data []byte, state *translate.StreamState) ([]canonical.Chunk, error) {
	var delta messageDeltaData
	if err := json.Unmarshal(data, &delta); err != nil {
		return nil, err
	}

	// Map stop_reason
	if delta.Delta.StopReason != "" {
		switch delta.Delta.StopReason {
		case "end_turn":
			state.FinishReason = "stop"
		case "max_tokens":
			state.FinishReason = "length"
		case "tool_use":
			state.FinishReason = "tool_calls"
		default:
			state.FinishReason = delta.Delta.StopReason
		}
	}

	// Extract output usage
	if delta.Usage != nil {
		if state.Usage == nil {
			state.Usage = &canonical.Usage{}
		}
		state.Usage.OutputTokens = delta.Usage.OutputTokens
		state.Usage.CompletionTokens = delta.Usage.OutputTokens
		state.Usage.TotalTokens = state.Usage.PromptTokens + state.Usage.CompletionTokens
	}

	return nil, nil
}

func (t *Translator) handleMessageStop(state *translate.StreamState) ([]canonical.Chunk, error) {
	chunk := canonical.Chunk{
		ID:           state.MessageID,
		Model:        state.Model,
		FinishReason: state.FinishReason,
	}
	if state.Usage != nil {
		chunk.Usage = state.Usage
	}
	return []canonical.Chunk{chunk}, nil
}

// CanonicalToStreamChunk converts canonical chunks to Claude SSE format.
func (t *Translator) CanonicalToStreamChunk(chunk canonical.Chunk, state *translate.StreamState) ([]byte, error) {
	// Role announcement → message_start
	if chunk.Role != "" {
		state.MessageID = chunk.ID
		state.Model = chunk.Model
		event := map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":    chunk.ID,
				"type":  "message",
				"role":  chunk.Role,
				"model": chunk.Model,
				"content": []any{},
				"usage": map[string]any{
					"input_tokens":  0,
					"output_tokens": 0,
				},
			},
		}
		return json.Marshal(event)
	}

	if chunk.Delta != nil {
		// Thinking content
		if chunk.Delta.Thinking != "" {
			if !state.InThinking {
				state.InThinking = true
				// Emit content_block_start for thinking
				start := map[string]any{
					"type":  "content_block_start",
					"index": state.BlockIndex,
					"content_block": map[string]any{
						"type":     "thinking",
						"thinking": "",
					},
				}
				startData, _ := json.Marshal(start)

				delta := map[string]any{
					"type":  "content_block_delta",
					"index": state.BlockIndex,
					"delta": map[string]any{
						"type":     "thinking_delta",
						"thinking": chunk.Delta.Thinking,
					},
				}
				deltaData, _ := json.Marshal(delta)
				return combineEvents(startData, deltaData), nil
			}
			delta := map[string]any{
				"type":  "content_block_delta",
				"index": state.BlockIndex,
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": chunk.Delta.Thinking,
				},
			}
			return json.Marshal(delta)
		}

		// Text content
		if chunk.Delta.Text != "" {
			if state.InThinking {
				// Close thinking block, open text block
				state.InThinking = false
				stop := map[string]any{"type": "content_block_stop", "index": state.BlockIndex}
				state.BlockIndex++
				start := map[string]any{
					"type":  "content_block_start",
					"index": state.BlockIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				}
				delta := map[string]any{
					"type":  "content_block_delta",
					"index": state.BlockIndex,
					"delta": map[string]any{"type": "text_delta", "text": chunk.Delta.Text},
				}
				stopData, _ := json.Marshal(stop)
				startData, _ := json.Marshal(start)
				deltaData, _ := json.Marshal(delta)
				return combineEvents(stopData, startData, deltaData), nil
			}

			blockType, _ := state.Custom["activeBlockType"].(string)
			if blockType != "text" {
				state.Custom["activeBlockType"] = "text"
				start := map[string]any{
					"type":  "content_block_start",
					"index": state.BlockIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				}
				delta := map[string]any{
					"type":  "content_block_delta",
					"index": state.BlockIndex,
					"delta": map[string]any{"type": "text_delta", "text": chunk.Delta.Text},
				}
				startData, _ := json.Marshal(start)
				deltaData, _ := json.Marshal(delta)
				return combineEvents(startData, deltaData), nil
			}

			delta := map[string]any{
				"type":  "content_block_delta",
				"index": state.BlockIndex,
				"delta": map[string]any{"type": "text_delta", "text": chunk.Delta.Text},
			}
			return json.Marshal(delta)
		}

		// Tool call start
		if chunk.Delta.ToolCallID != "" {
			// Close any open block
			var events [][]byte
			blockType, _ := state.Custom["activeBlockType"].(string)
			if blockType != "" {
				stop := map[string]any{"type": "content_block_stop", "index": state.BlockIndex}
				stopData, _ := json.Marshal(stop)
				events = append(events, stopData)
				state.BlockIndex++
			}

			state.Custom["activeBlockType"] = "tool_use"
			state.InToolCall = true
			start := map[string]any{
				"type":  "content_block_start",
				"index": state.BlockIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    chunk.Delta.ToolCallID,
					"name":  chunk.Delta.ToolName,
					"input": map[string]any{},
				},
			}
			startData, _ := json.Marshal(start)
			events = append(events, startData)
			return combineEvents(events...), nil
		}

		// Tool call arguments continuation
		if chunk.Delta.Arguments != "" {
			delta := map[string]any{
				"type":  "content_block_delta",
				"index": state.BlockIndex,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": chunk.Delta.Arguments,
				},
			}
			return json.Marshal(delta)
		}
	}

	// Finish reason
	if chunk.FinishReason != "" {
		var events [][]byte

		// Close any open block
		blockType, _ := state.Custom["activeBlockType"].(string)
		if blockType != "" {
			stop := map[string]any{"type": "content_block_stop", "index": state.BlockIndex}
			stopData, _ := json.Marshal(stop)
			events = append(events, stopData)
		}

		// Map finish reason
		stopReason := "end_turn"
		switch chunk.FinishReason {
		case "stop":
			stopReason = "end_turn"
		case "length":
			stopReason = "max_tokens"
		case "tool_calls":
			stopReason = "tool_use"
		}

		// message_delta with usage
		msgDelta := map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason": stopReason,
			},
		}
		if chunk.Usage != nil {
			msgDelta["usage"] = map[string]any{
				"output_tokens": chunk.Usage.CompletionTokens,
			}
		}
		msgDeltaData, _ := json.Marshal(msgDelta)
		events = append(events, msgDeltaData)

		// message_stop
		msgStop := map[string]any{"type": "message_stop"}
		msgStopData, _ := json.Marshal(msgStop)
		events = append(events, msgStopData)

		return combineEvents(events...), nil
	}

	return nil, nil
}

// combineEvents joins multiple SSE event data payloads with newlines.
func combineEvents(events ...[]byte) []byte {
	if len(events) == 0 {
		return nil
	}
	if len(events) == 1 {
		return events[0]
	}
	total := 0
	for _, e := range events {
		total += len(e) + 1
	}
	result := make([]byte, 0, total)
	for i, e := range events {
		result = append(result, e...)
		if i < len(events)-1 {
			result = append(result, '\n')
		}
	}
	return result
}
