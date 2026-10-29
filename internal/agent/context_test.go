package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"rosetta/internal/llm"
	"rosetta/internal/protocol"
)

func TestHistoryTrimmedAsWholeTurnsAsToolLoopGrows(t *testing.T) {
	history := []llm.HistoryTurn{{Messages: []llm.Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "old", Name: "get_time", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "old", Content: "old observation"},
		{Role: "assistant", Content: strings.Repeat("h", 30<<10)},
	}}}
	calls, executions, recorded := 0, 0, 0
	p := providerFunc(func(ctx context.Context, input llm.Request, emit func(string) error) (llm.Response, error) {
		calls++
		encoded, _ := json.Marshal(input)
		if len(encoded) > llm.MaxInputBytes || input.Messages[0].Role != "system" {
			t.Fatal("invalid assembled input")
		}
		if calls == 1 {
			if len(input.Messages) != 6 || input.Messages[1].Content != "old question" {
				t.Fatal("initial history missing")
			}
			text := strings.Repeat("n", 40<<10)
			if err := emit(text); err != nil {
				return llm.Response{}, err
			}
			return llm.Response{Message: llm.Message{Role: "assistant", Content: text, ToolCalls: []llm.ToolCall{{ID: "new", Name: "get_time", Arguments: `{}`}}}}, nil
		}
		if len(input.Messages) != 4 || input.Messages[1].Content != "current question" || input.Messages[2].ToolCalls[0].ID != "new" || input.Messages[3].ToolCallID != "new" {
			t.Fatal("trim split a turn or removed current input")
		}
		return answer("done", emit)
	})
	err := New(p, timeRegistry(t)).RunWithHistory(context.Background(), protocol.ChatRequest{Message: "current question"}, history, func(e protocol.Event) error {
		if e.Type == protocol.ToolStarted {
			executions++
		}
		return nil
	}, func(llm.Message) error { recorded++; return nil })
	if err != nil || calls != 2 || executions != 1 || recorded != 3 {
		t.Fatalf("calls=%d tools=%d recorded=%d err=%v", calls, executions, recorded, err)
	}
	if len(history[0].Messages) != 4 || history[0].Messages[2].ToolCallID != "old" {
		t.Fatal("mutated source history")
	}
}

type sizedProvider struct{ providerFunc }

func (sizedProvider) InputSize(req llm.Request) (int, error) {
	if len(req.Messages) > 2 {
		return llm.MaxInputBytes + 1, nil
	}
	return 100, nil
}

func TestProviderWireBudgetCanEvictHistory(t *testing.T) {
	p := sizedProvider{providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		if len(req.Messages) != 2 || req.Messages[1].Content != "now" {
			t.Fatal("provider wire overhead ignored")
		}
		return answer("done", emit)
	})}
	err := New(p, nil).RunWithHistory(context.Background(), protocol.ChatRequest{Message: "now"}, []llm.HistoryTurn{{Messages: []llm.Message{{Role: "user", Content: "past"}, {Role: "assistant", Content: "answer"}}}}, func(protocol.Event) error { return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestInvalidHistoryNeverReachesModel(t *testing.T) {
	p := providerFunc(func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		t.Fatal("invalid history reached model")
		return llm.Response{}, nil
	})
	err := New(p, nil).RunWithHistory(context.Background(), protocol.ChatRequest{Message: "now"}, []llm.HistoryTurn{{Messages: []llm.Message{{Role: "user", Content: "past"}, {Role: "tool", ToolCallID: "orphan"}, {Role: "assistant", Content: "answer"}}}}, func(protocol.Event) error { return nil }, nil)
	if err == nil || err.Error() != "invalid_history" {
		t.Fatal(err)
	}
}
