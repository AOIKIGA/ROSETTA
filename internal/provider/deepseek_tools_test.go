package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rosetta/internal/agent"
	"rosetta/internal/llm"
	"rosetta/internal/protocol"
	"rosetta/internal/tool"
)

const toolStopFrame = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"

func toolFrame(index int, id, name, args string) string {
	data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}}}})
	return "data: " + string(data) + "\n\n"
}

func TestInterleavedToolArgumentsAndToolOnlyRound(t *testing.T) {
	input := toolFrame(1, "b", "get_time", `{"time`) + toolFrame(0, "a", "get_time", `{`) + toolFrame(1, "", "", `zone":"UTC"}`) + toolFrame(0, "", "", `}`) + toolStopFrame + doneFrame
	response, err := readDeepSeekStream(context.Background(), strings.NewReader(input), func(string) error { t.Fatal("unexpected text"); return nil })
	if err != nil || len(response.Message.ToolCalls) != 2 {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	a, b := response.Message.ToolCalls[0], response.Message.ToolCalls[1]
	if a.ID != "a" || a.Arguments != `{}` || b.ID != "b" || b.Arguments != `{"timezone":"UTC"}` {
		t.Fatalf("calls=%+v", response.Message.ToolCalls)
	}
}

func TestInvalidToolStreamsNeverReturnExecutableCalls(t *testing.T) {
	valid := toolFrame(0, "a", "get_time", `{}`)
	for _, tc := range []struct{ name, input, code string }{
		{"truncated", valid, "provider_incomplete_response"},
		{"no_done", valid + toolStopFrame, "provider_incomplete_response"},
		{"wrong_finish", valid + stopFrame + doneFrame, "provider_incomplete_response"},
		{"missing_id", toolFrame(0, "", "get_time", `{}`) + toolStopFrame + doneFrame, "provider_invalid_event"},
		{"missing_index", strings.Replace(valid, `"index":0,`, "", 1) + toolStopFrame + doneFrame, "provider_invalid_event"},
		{"index_gap", toolFrame(1, "b", "get_time", `{}`) + toolStopFrame + doneFrame, "provider_invalid_event"},
		{"duplicate_id", valid + toolFrame(1, "a", "get_time", `{}`) + toolStopFrame + doneFrame, "provider_invalid_event"},
		{"changed_name", valid + toolFrame(0, "", "different", "") + toolStopFrame + doneFrame, "provider_invalid_event"},
		{"too_many", toolFrame(4, "e", "get_time", `{}`) + toolStopFrame + doneFrame, "provider_invalid_event"},
		{"large_arguments", toolFrame(0, "a", "get_time", strings.Repeat("x", 17<<10)) + toolStopFrame + doneFrame, "provider_response_limit"},
		{"after_finish", valid + toolStopFrame + textFrame + doneFrame, "provider_invalid_event"},
		{"output_limit", valid + strings.ReplaceAll(stopFrame, "stop", "length"), "provider_output_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := readDeepSeekStream(context.Background(), strings.NewReader(tc.input), func(string) error { return nil })
			var public *PublicError
			if !errors.As(err, &public) || public.Code != tc.code || len(response.Message.ToolCalls) != 0 {
				t.Fatalf("response=%+v err=%v", response, err)
			}
		})
	}
}

func TestDeepSeekRuntimeToolRoundTrip(t *testing.T) {
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rounds++
		var req struct {
			Messages []apiMessage  `json:"messages"`
			Tools    []apiToolSpec `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(req.Tools) != 1 || req.Tools[0].Type != "function" || req.Tools[0].Function.Name != "get_time" {
			t.Error("tool schema not sent")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if rounds == 1 {
			if len(req.Messages) != 2 || req.Messages[1].Role != "user" {
				t.Error("bad initial messages")
			}
			_, _ = io.WriteString(w, toolFrame(0, "call1", "get_time", `{"time`)+toolFrame(0, "", "", `zone":"UTC"}`)+toolStopFrame+doneFrame)
			return
		}
		if len(req.Messages) != 4 {
			t.Errorf("messages=%+v", req.Messages)
			w.WriteHeader(400)
			return
		}
		assistant, observation := req.Messages[2], req.Messages[3]
		if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Type != "function" || assistant.ToolCalls[0].ID != "call1" || assistant.ToolCalls[0].Function.Arguments != `{"timezone":"UTC"}` || observation.Role != "tool" || observation.ToolCallID != "call1" {
			t.Error("incorrect wire pairing")
		}
		if !strings.Contains(observation.Content, "2026-10-22T00:00:00Z") {
			t.Errorf("tool result missing: %s", observation.Content)
		}
		_, _ = fmt.Fprint(w, textFrame+stopFrame+doneFrame)
	}))
	defer srv.Close()
	p, _ := NewDeepSeek("test-credential", DefaultDeepSeekModel, 64)
	p.endpoint = srv.URL
	tools, _ := tool.NewRegistry(tool.Time{Now: func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) }})
	var events []protocol.Event
	err := agent.New(p, tools).Run(context.Background(), protocol.ChatRequest{Message: "query time"}, func(e protocol.Event) error { events = append(events, e); return nil })
	if err != nil || rounds != 2 || events[len(events)-1].Type != protocol.Completed {
		t.Fatalf("rounds=%d events=%+v err=%v", rounds, events, err)
	}
}

func TestDeepSeekInputBudgetBeforeNetwork(t *testing.T) {
	p, _ := NewDeepSeek("test-credential", DefaultDeepSeekModel, 64)
	_, err := p.Stream(context.Background(), llm.Request{Messages: []llm.Message{{Role: "user", Content: strings.Repeat("x", llm.MaxInputBytes)}}}, func(string) error { return nil })
	if err == nil || err.Error() != "input_limit" {
		t.Fatal(err)
	}
}
