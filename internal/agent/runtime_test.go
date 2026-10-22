package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"rosetta/internal/llm"
	"rosetta/internal/protocol"
	"rosetta/internal/tool"
)

type providerFunc func(context.Context, llm.Request, func(string) error) (llm.Response, error)

func (providerFunc) Name() string { return "test" }

func (f providerFunc) Stream(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
	return f(ctx, req, emit)
}

func TestPartialProviderFailureNeverCompletes(t *testing.T) {
	want := errors.New("upstream disconnected")
	r := New(providerFunc(func(ctx context.Context, _ llm.Request, emit func(string) error) (llm.Response, error) {
		if err := emit("partial"); err != nil {
			return llm.Response{}, err
		}
		return llm.Response{}, want
	}), nil)
	var events []protocol.Event
	err := r.Run(context.Background(), protocol.ChatRequest{Message: "hello"}, func(e protocol.Event) error {
		events = append(events, e)
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if len(events) != 3 || events[0].Type != protocol.Started || events[2].Text != "partial" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestConsumerErrorStopsProvider(t *testing.T) {
	want := errors.New("output closed")
	r := New(providerFunc(func(ctx context.Context, _ llm.Request, emit func(string) error) (llm.Response, error) {
		return llm.Response{}, emit("hello")
	}), nil)
	err := r.Run(context.Background(), protocol.ChatRequest{}, func(e protocol.Event) error {
		if e.Type == protocol.TextDelta {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}

func timeRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	r, err := tool.NewRegistry(tool.Time{Now: func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func answer(text string, emit func(string) error) (llm.Response, error) {
	if err := emit(text); err != nil {
		return llm.Response{}, err
	}
	return llm.Response{Message: llm.Message{Role: "assistant", Content: text}}, nil
}

func TestToolLoopPairsMultipleCallsAndObservations(t *testing.T) {
	calls := 0
	p := providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		calls++
		if len(req.Tools) != 1 || req.Tools[0].Name != "get_time" {
			t.Fatal("missing tool definition")
		}
		if calls == 1 {
			if len(req.Messages) != 2 || req.Messages[1].Content != "compare times" {
				t.Fatal("bad initial messages")
			}
			return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
				{ID: "a", Name: "get_time", Arguments: `{}`},
				{ID: "b", Name: "get_time", Arguments: `{"timezone":"UTC"}`},
			}}}, nil
		}
		if len(req.Messages) != 5 || len(req.Messages[2].ToolCalls) != 2 {
			t.Fatalf("missing exchanges: %+v", req.Messages)
		}
		for i, id := range []string{"a", "b"} {
			message := req.Messages[3+i]
			var result tool.Result
			if message.Role != "tool" || message.ToolCallID != id || json.Unmarshal([]byte(message.Content), &result) != nil || result.IsError {
				t.Fatalf("bad result: %+v", message)
			}
			if !strings.Contains(result.Content, "2026-10-22") {
				t.Fatal("tool did not use injected clock")
			}
		}
		return answer("times compared", emit)
	})
	var events []protocol.Event
	err := New(p, timeRegistry(t)).Run(context.Background(), protocol.ChatRequest{Message: "compare times"}, func(e protocol.Event) error { events = append(events, e); return nil })
	if err != nil || calls != 2 || events[len(events)-1].Type != protocol.Completed {
		t.Fatalf("calls=%d events=%v err=%v", calls, events, err)
	}
	starts, finishes := 0, 0
	for _, e := range events {
		if e.Type == protocol.ToolStarted {
			starts++
		}
		if e.Type == protocol.ToolFinished {
			finishes++
			if e.Status != "ok" {
				t.Fatal(e)
			}
		}
	}
	if starts != 2 || finishes != 2 {
		t.Fatalf("tool events: %v", events)
	}
}

func TestToolErrorsCanBeCorrectedByNextRound(t *testing.T) {
	for _, first := range []llm.ToolCall{
		{ID: "bad", Name: "missing", Arguments: `{}`},
		{ID: "bad", Name: "get_time", Arguments: `{"timezone":42}`},
		{ID: "bad", Name: "get_time", Arguments: `{broken`},
	} {
		t.Run(first.Name+first.Arguments, func(t *testing.T) {
			step := 0
			p := providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
				step++
				switch step {
				case 1:
					return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{first}}}, nil
				case 2:
					var result tool.Result
					if json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &result) != nil || !result.IsError {
						t.Fatal("error observation not passed back")
					}
					return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "fixed", Name: "get_time", Arguments: `{}`}}}}, nil
				default:
					return answer("corrected", emit)
				}
			})
			if err := New(p, timeRegistry(t)).Run(context.Background(), protocol.ChatRequest{}, func(protocol.Event) error { return nil }); err != nil || step != 3 {
				t.Fatalf("step=%d err=%v", step, err)
			}
		})
	}
}

func TestLoopLimitsAndInvalidCalls(t *testing.T) {
	for _, tc := range []struct {
		name     string
		batch    int
		repeatID bool
		want     string
		rounds   int
		executed int
	}{
		{"round_limit", 1, false, "round_limit", 6, 5},
		{"total_tool_limit", 4, false, "tool_call_limit", 3, 8},
		{"batch_limit", 5, false, "tool_call_limit", 1, 0},
		{"duplicate_id", 2, true, "provider_invalid_tool_call", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rounds, executed := 0, 0
			p := providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
				rounds++
				var batch []llm.ToolCall
				for i := 0; i < tc.batch; i++ {
					id := fmt.Sprintf("%d_%d", rounds, i)
					if tc.repeatID {
						id = "same"
					}
					batch = append(batch, llm.ToolCall{ID: id, Name: "get_time", Arguments: `{}`})
				}
				return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: batch}}, nil
			})
			err := New(p, timeRegistry(t)).Run(context.Background(), protocol.ChatRequest{}, func(e protocol.Event) error {
				if e.Type == protocol.ToolStarted {
					executed++
				}
				if e.Type == protocol.Completed {
					t.Fatal("completed after limit")
				}
				return nil
			})
			var public *llm.Error
			if !errors.As(err, &public) || public.Code != tc.want || rounds != tc.rounds || executed != tc.executed {
				t.Fatalf("rounds=%d tools=%d err=%v", rounds, executed, err)
			}
		})
	}
}

func TestInputLimitStopsBeforeProvider(t *testing.T) {
	p := providerFunc(func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		t.Fatal("provider called")
		return llm.Response{}, nil
	})
	err := New(p, nil).Run(context.Background(), protocol.ChatRequest{Message: strings.Repeat("x", MaxInputBytes)}, func(protocol.Event) error { return nil })
	if err == nil || err.Error() != "input_limit" {
		t.Fatal(err)
	}
}

func TestCancelOrOutputFailurePreventsTools(t *testing.T) {
	for _, cancelTurn := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		want := errors.New("output closed")
		p := providerFunc(func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
			return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "a", Name: "get_time", Arguments: `{}`}}}}, nil
		})
		err := New(p, timeRegistry(t)).Run(ctx, protocol.ChatRequest{}, func(e protocol.Event) error {
			if e.Type == protocol.ToolStarted {
				if cancelTurn {
					cancel()
					return nil
				}
				return want
			}
			if e.Type == protocol.ToolFinished || e.Type == protocol.Completed {
				t.Fatal("continued after output failure/cancel")
			}
			return nil
		})
		cancel()
		if cancelTurn {
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		} else if !errors.Is(err, want) {
			t.Fatal(err)
		}
	}
}

func TestConcurrentTurnsHaveIndependentMessages(t *testing.T) {
	p := providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		user := req.Messages[1].Content
		if len(req.Messages) == 2 {
			return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: user, Name: "get_time", Arguments: `{}`}}}}, nil
		}
		if len(req.Messages) != 4 || req.Messages[3].ToolCallID != user {
			return llm.Response{}, errors.New("crossed turns")
		}
		return answer(user, emit)
	})
	r := New(p, timeRegistry(t))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprint(i)
			if err := r.Run(context.Background(), protocol.ChatRequest{Message: id}, func(protocol.Event) error { return nil }); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}

type waitingTool struct{}

func (waitingTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "wait", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (waitingTool) Execute(ctx context.Context, _ string) (tool.Result, error) {
	<-ctx.Done()
	return tool.Result{}, ctx.Err()
}

func TestToolDeadlineStopsTheTurn(t *testing.T) {
	tools, _ := tool.NewRegistry(waitingTool{})
	calls := 0
	p := providerFunc(func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		calls++
		return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "wait1", Name: "wait", Arguments: `{}`}}}}, nil
	})
	r := New(p, tools)
	r.toolTimeout = 10 * time.Millisecond
	err := r.Run(context.Background(), protocol.ChatRequest{}, func(e protocol.Event) error {
		if e.Type == protocol.Completed {
			t.Fatal("completed after timeout")
		}
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestDuplicateCallIDAcrossRoundsIsRejected(t *testing.T) {
	calls, executions := 0, 0
	p := providerFunc(func(context.Context, llm.Request, func(string) error) (llm.Response, error) {
		calls++
		return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "reused", Name: "get_time", Arguments: `{}`}}}}, nil
	})
	err := New(p, timeRegistry(t)).Run(context.Background(), protocol.ChatRequest{}, func(e protocol.Event) error {
		if e.Type == protocol.ToolStarted {
			executions++
		}
		return nil
	})
	if err == nil || err.Error() != "provider_invalid_tool_call" || calls != 2 || executions != 1 {
		t.Fatalf("calls=%d executions=%d err=%v", calls, executions, err)
	}
}
