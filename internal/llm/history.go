package llm

import "strings"

// HistoryTurn is one completed user request, including all tool exchanges.
// Keeping turn boundaries lets the runtime trim history without orphaning tools.
type HistoryTurn struct {
	Messages []Message
}

const MaxHistoryTurns = 20
const MaxHistoryBytes = 32 << 10

// ValidHistoryTurn checks structure, not the truth or authority of stored text.
// Malformed tool arguments may be legitimate history followed by an error result.
func ValidHistoryTurn(turn HistoryTurn) bool {
	messages := turn.Messages
	if len(messages) < 2 || messages[0].Role != "user" || strings.TrimSpace(messages[0].Content) == "" {
		return false
	}
	pending, seen := map[string]bool{}, map[string]bool{}
	for i, m := range messages {
		switch m.Role {
		case "user":
			if i != 0 || len(m.ToolCalls) != 0 || m.ToolCallID != "" {
				return false
			}
		case "assistant":
			if i == 0 || len(pending) != 0 || m.ToolCallID != "" || len(m.ToolCalls) > MaxToolCalls {
				return false
			}
			if len(m.ToolCalls) == 0 {
				return i == len(messages)-1 && strings.TrimSpace(m.Content) != ""
			}
			for _, call := range m.ToolCalls {
				if strings.TrimSpace(call.ID) == "" || len(call.ID) > 128 || strings.TrimSpace(call.Name) == "" || len(call.Name) > 128 || len(call.Arguments) > 16<<10 || seen[call.ID] {
					return false
				}
				pending[call.ID], seen[call.ID] = true, true
			}
		case "tool":
			if !pending[m.ToolCallID] || len(m.ToolCalls) != 0 {
				return false
			}
			delete(pending, m.ToolCallID)
		default:
			return false // Never accept stored system/developer messages as history.
		}
	}
	return false // A completed turn must end with a final assistant answer.
}
