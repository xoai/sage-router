package canonical

import (
	"errors"
	"fmt"
)

// Validate checks a canonical Request for structural correctness.
func Validate(req *Request) error {
	if req.Model == "" {
		return errors.New("model is required")
	}

	for i, msg := range req.Messages {
		switch msg.Role {
		case RoleSystem, RoleUser, RoleAssistant, RoleTool:
			// valid
		default:
			return fmt.Errorf("message[%d]: unknown role %q", i, msg.Role)
		}

		if msg.Content == nil {
			return fmt.Errorf("message[%d]: content must not be nil", i)
		}

		for j, c := range msg.Content {
			switch c.Type {
			case TypeText, TypeThinking:
				// Text may be empty
			case TypeImage:
				if c.ImageSource == nil {
					return fmt.Errorf("message[%d].content[%d]: image requires image_source", i, j)
				}
			case TypeToolCall:
				if c.ToolCallID == "" {
					return fmt.Errorf("message[%d].content[%d]: tool_call requires tool_call_id", i, j)
				}
				if c.ToolName == "" {
					return fmt.Errorf("message[%d].content[%d]: tool_call requires tool_name", i, j)
				}
			case TypeToolResult:
				if c.ToolCallID == "" {
					return fmt.Errorf("message[%d].content[%d]: tool_result requires tool_call_id", i, j)
				}
			default:
				return fmt.Errorf("message[%d].content[%d]: unknown type %q", i, j, c.Type)
			}
		}
	}

	// Validate tool call/result pairing
	toolCallIDs := map[string]bool{}
	toolResultIDs := map[string]bool{}
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			if c.Type == TypeToolCall {
				toolCallIDs[c.ToolCallID] = true
			}
			if c.Type == TypeToolResult {
				toolResultIDs[c.ToolCallID] = true
			}
		}
	}
	for id := range toolCallIDs {
		if !toolResultIDs[id] {
			return fmt.Errorf("tool_call %q has no matching tool_result", id)
		}
	}

	return nil
}
