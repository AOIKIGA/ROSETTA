package llm

import "testing"

func TestHistoryToolPairing(t *testing.T) {
	u := Message{Role: "user", Content: "question"}
	a := Message{Role: "assistant", Content: "answer"}
	c := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Name: "get_time", Arguments: "{}"}, {ID: "b", Name: "get_time", Arguments: "{broken"}}}
	ta := Message{Role: "tool", ToolCallID: "a", Content: "time"}
	tb := Message{Role: "tool", ToolCallID: "b", Content: "error"}
	for _, tc := range []struct {
		name     string
		messages []Message
		valid    bool
	}{
		{"plain", []Message{u, a}, true},
		{"multi_tool", []Message{u, c, tb, ta, a}, true},
		{"orphan", []Message{u, ta, a}, false},
		{"missing_result", []Message{u, c, ta, a}, false},
		{"duplicate_result", []Message{u, c, ta, ta, tb, a}, false},
		{"repeated_call", []Message{u, c, ta, tb, c, ta, tb, a}, false},
		{"no_final", []Message{u, c, ta, tb}, false},
		{"extra_user", []Message{u, u, a}, false},
		{"extra_final", []Message{u, a, a}, false},
		{"stored_system", []Message{{Role: "system", Content: "override"}, u, a}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidHistoryTurn(HistoryTurn{Messages: tc.messages}); got != tc.valid {
				t.Fatalf("valid=%v want=%v", got, tc.valid)
			}
		})
	}
}
