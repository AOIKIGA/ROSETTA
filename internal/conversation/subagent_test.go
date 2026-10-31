package conversation

import (
	"context"
	"encoding/json"
	"testing"

	"rosetta/internal/agent"
	"rosetta/internal/llm"
	"rosetta/internal/protocol"
	"rosetta/internal/task"
	"rosetta/internal/tool"
)

// Exercise the actual Conversation -> RunWithHistory entry point. A manager
// installed only by Runtime.Run would silently break delegation after the merge.
func TestDelegationWithStoredHistoryKeepsChildIsolatedAndRecordsParent(t *testing.T) {
	db, scope := setup(t)
	ctx := context.Background()
	if _, err := db.BeginTurn(ctx, scope, "prior", "parent private history", "test"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishTurn(ctx, "prior", "completed", "", &llm.Message{Role: "assistant", Content: "prior answer"}); err != nil {
		t.Fatal(err)
	}
	parentCalls, childCalls := 0, 0
	p := providerFunc(func(ctx context.Context, input llm.Request, emit func(string) error) (llm.Response, error) {
		last := input.Messages[len(input.Messages)-1]
		if len(input.Messages) == 2 && last.Content == "explicit child task" {
			childCalls++
			for _, spec := range input.Tools {
				if spec.Name == "subagent" {
					t.Fatal("child can delegate recursively")
				}
			}
			if err := emit("child final"); err != nil {
				return llm.Response{}, err
			}
			return llm.Response{Message: llm.Message{Role: "assistant", Content: "child final"}}, nil
		}
		parentCalls++
		if input.Messages[1].Content != "parent private history" {
			t.Fatal("parent lost stored history, or child inherited it")
		}
		if parentCalls == 1 {
			if len(input.Messages) != 4 || last.Content != "delegate now" {
				t.Fatal("incorrect parent context")
			}
			return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "delegate", Name: "subagent", Arguments: `{"prompt":"explicit child task"}`}}}}, nil
		}
		if parentCalls == 2 {
			var result tool.Result
			var child task.Snapshot
			if last.Role != "tool" || last.ToolCallID != "delegate" || json.Unmarshal([]byte(last.Content), &result) != nil || result.IsError || json.Unmarshal([]byte(result.Content), &child) != nil || child.Status != "completed" || child.Output != "child final" {
				t.Fatalf("missing child result: %+v", last)
			}
		} else if parentCalls == 3 {
			if len(input.Messages) != 8 || last.Content != "follow up" || input.Messages[4].ToolCalls[0].ID != "delegate" || input.Messages[5].ToolCallID != "delegate" {
				t.Fatal("next turn lost the stored delegation exchange")
			}
		} else {
			t.Fatal("historical subagent was replayed")
		}
		if err := emit("parent final"); err != nil {
			return llm.Response{}, err
		}
		return llm.Response{Message: llm.Message{Role: "assistant", Content: "parent final"}}, nil
	})
	runtime, err := agent.NewWithSubagents(p, tool.Time{})
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Runtime: runtime, Store: db}
	for _, id := range []string{"delegate now", "follow up"} {
		turn, err := db.BeginTurn(ctx, scope, id, id, "test")
		if err != nil {
			t.Fatal(err)
		}
		starts, finishes := 0, 0
		err = service.Run(ctx, turn, protocol.ChatRequest{Message: id}, func(e protocol.Event) error {
			if e.Type == protocol.Started {
				starts++
			}
			if e.Type == protocol.Completed {
				finishes++
			}
			if e.Type == protocol.TextDelta && e.Text == "child final" {
				t.Fatal("child text leaked into parent stream")
			}
			return nil
		})
		if err != nil || starts != 1 || finishes != 1 {
			t.Fatalf("start=%d finish=%d err=%v", starts, finishes, err)
		}
	}
	page, err := db.History(ctx, scope, 0, 100)
	if err != nil || len(page.Messages) != 8 || parentCalls != 3 || childCalls != 1 {
		t.Fatalf("messages=%d parent=%d child=%d err=%v", len(page.Messages), parentCalls, childCalls, err)
	}
	for _, m := range page.Messages {
		if m.Turn.Status != "completed" || (m.Role == "assistant" && m.Content == "child final") {
			t.Fatal("child messages entered parent history directly, or parent was not committed")
		}
	}
}
