package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"rosetta/internal/llm"
	"rosetta/internal/protocol"
	"rosetta/internal/task"
	"rosetta/internal/tool"
)

func TestSubagentHasSeparateMessagesAndReturnsOnlyFinalAnswer(t *testing.T) {
	parentCalls, childCalls := 0, 0
	p := providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		if req.Messages[1].Content == "parent private context" {
			parentCalls++
			if parentCalls == 1 {
				return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "delegate", Name: "subagent", Arguments: `{"prompt":"child task"}`}}}}, nil
			}
			var result tool.Result
			var snapshot task.Snapshot
			if json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &result) != nil || json.Unmarshal([]byte(result.Content), &snapshot) != nil {
				t.Fatal("missing child observation")
			}
			if snapshot.Status != "completed" || snapshot.Output != "child final" || snapshot.ID == "" {
				t.Fatalf("%+v", snapshot)
			}
			return answer("parent final", emit)
		}
		childCalls++
		if req.Messages[1].Content != "child task" {
			t.Fatalf("child inherited parent: %+v", req.Messages)
		}
		for _, spec := range req.Tools {
			if spec.Name == "subagent" {
				t.Fatal("recursive delegation exposed")
			}
		}
		if childCalls == 1 {
			if len(req.Messages) != 2 {
				t.Fatal("child inherited history")
			}
			if err := emit("child intermediate"); err != nil {
				return llm.Response{}, err
			}
			return llm.Response{Message: llm.Message{Role: "assistant", Content: "child intermediate", ToolCalls: []llm.ToolCall{{ID: "time", Name: "get_time", Arguments: `{}`}}}}, nil
		}
		return answer("child final", emit)
	})
	r, err := NewWithSubagents(p, tool.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var shown strings.Builder
	starts, completed, toolStarts := 0, 0, 0
	err = r.Run(context.Background(), protocol.ChatRequest{Message: "parent private context"}, func(e protocol.Event) error {
		switch e.Type {
		case protocol.TextDelta:
			shown.WriteString(e.Text)
		case protocol.Started:
			starts++
		case protocol.Completed:
			completed++
		case protocol.ToolStarted:
			toolStarts++
		}
		return nil
	})
	if err != nil || shown.String() != "parent final" || parentCalls != 2 || childCalls != 2 || starts != 1 || completed != 1 || toolStarts != 1 {
		t.Fatalf("text=%q calls=%d/%d events=%d/%d/%d err=%v", shown.String(), parentCalls, childCalls, starts, completed, toolStarts, err)
	}
}

func TestParentCancellationStopsChild(t *testing.T) {
	entered := make(chan struct{})
	p := providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		if req.Messages[1].Content == "parent" {
			return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "child", Name: "subagent", Arguments: `{"prompt":"wait"}`}}}}, nil
		}
		close(entered)
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	})
	r, err := NewWithSubagents(p, tool.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, protocol.ChatRequest{Message: "parent"}, func(protocol.Event) error { return nil })
	}()
	select {
	case <-entered:
		cancel()
	case <-ctx.Done():
		t.Fatal("child did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("child outlived canceled parent")
	}
}

func TestChildTaskLimitAndToolTimeoutPolicy(t *testing.T) {
	parents, children := 0, 0
	p := providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		if req.Messages[1].Content == "child" {
			children++
			return answer("done", emit)
		}
		parents++
		if parents <= 3 {
			return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: string(rune('a' + parents)), Name: "subagent", Arguments: `{"prompt":"child"}`}}}}, nil
		}
		var result tool.Result
		if json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &result) != nil || result.Code != "subagent_limit" {
			t.Fatal("third child not rejected")
		}
		return answer("finished", emit)
	})
	r, err := NewWithSubagents(p, tool.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if r.tools.Timeout("subagent", r.toolTimeout) != time.Minute || r.tools.Timeout("get_time", r.toolTimeout) != 3*time.Second {
		t.Fatal("wrong timeout policy")
	}
	if err := r.Run(context.Background(), protocol.ChatRequest{Message: "parent"}, func(protocol.Event) error { return nil }); err != nil || children != 2 {
		t.Fatalf("children=%d err=%v", children, err)
	}
}

func TestChildRoundLimitIsAnObservationForParent(t *testing.T) {
	childCalls := 0
	p := providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		if req.Messages[1].Content == "child" {
			childCalls++
			return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: string(rune('a' + childCalls)), Name: "get_time", Arguments: `{}`}}}}, nil
		}
		if len(req.Messages) == 2 {
			return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "delegate", Name: "subagent", Arguments: `{"prompt":"child"}`}}}}, nil
		}
		var observation tool.Result
		if json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &observation) != nil || observation.Code != "round_limit" || !observation.IsError {
			t.Fatalf("bad failure observation: %+v", observation)
		}
		return answer("child reached its limit", emit)
	})
	r, err := NewWithSubagents(p, tool.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), protocol.ChatRequest{Message: "parent"}, func(protocol.Event) error { return nil }); err != nil || childCalls != 3 {
		t.Fatalf("calls=%d err=%v", childCalls, err)
	}
}

func TestConcurrentParentTurnsKeepChildResultsSeparate(t *testing.T) {
	p := providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		prompt := req.Messages[1].Content
		if strings.HasPrefix(prompt, "child:") {
			return answer(prompt, emit)
		}
		if len(req.Messages) == 2 {
			args, _ := json.Marshal(map[string]string{"prompt": "child:" + prompt})
			return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "delegate", Name: "subagent", Arguments: string(args)}}}}, nil
		}
		var observation tool.Result
		var snapshot task.Snapshot
		if json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &observation) != nil || json.Unmarshal([]byte(observation.Content), &snapshot) != nil {
			t.Error("invalid result")
		}
		if snapshot.Output != "child:"+prompt {
			t.Errorf("cross-turn result: %q for %q", snapshot.Output, prompt)
		}
		return answer(snapshot.Output, emit)
	})
	r, err := NewWithSubagents(p, tool.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, message := range []string{"alice", "bob", "carol", "dave"} {
		wg.Go(func() {
			if err := r.Run(context.Background(), protocol.ChatRequest{Message: message}, func(protocol.Event) error { return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
