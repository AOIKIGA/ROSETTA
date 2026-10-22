package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"rosetta/internal/llm"
)

func TestTimeValidatesArgumentsAndTimezone(t *testing.T) {
	clock := Time{Now: func() time.Time { return time.Date(2026, 10, 22, 0, 1, 2, 0, time.UTC) }}
	for _, tc := range []struct{ args, want, code string }{
		{`{}`, "2026-10-22T08:01:02+08:00", ""},
		{`{"timezone":"UTC"}`, "2026-10-22T00:01:02Z", ""},
		{`{"timezone":"Mars/Base"}`, "", "invalid_timezone"},
		{`{"timezone":42}`, "", "invalid_arguments"},
		{`{"timezone":null}`, "", "invalid_arguments"},
		{`{"timezone":""}`, "", "invalid_arguments"},
		{`{"timezone":"Local"}`, "", "invalid_arguments"},
		{`{"timezone":"/etc/passwd"}`, "", "invalid_arguments"},
		{`{"timezone":"../UTC"}`, "", "invalid_arguments"},
		{`{"unexpected":true}`, "", "invalid_arguments"},
		{`{} {}`, "", "invalid_arguments"},
		{`{broken`, "", "invalid_arguments"},
		{`null`, "", "invalid_arguments"},
		{`[]`, "", "invalid_arguments"},
	} {
		t.Run(tc.args, func(t *testing.T) {
			result, err := clock.Execute(context.Background(), tc.args)
			if err != nil || result.Code != tc.code || result.IsError != (tc.code != "") || !strings.Contains(result.Content, tc.want) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

type failedTool struct{}

func (failedTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "broken", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (failedTool) Execute(context.Context, string) (Result, error) {
	return Result{}, errors.New("private internal detail")
}

func TestRegistryAndResultBounds(t *testing.T) {
	if _, err := NewRegistry(Time{}, Time{}); err == nil {
		t.Fatal("duplicate tool accepted")
	}
	r, err := NewRegistry(Time{}, failedTool{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Execute(context.Background(), llm.ToolCall{Name: "missing"})
	if err != nil || result.Code != "unknown_tool" || !result.IsError {
		t.Fatalf("%+v %v", result, err)
	}
	result, err = r.Execute(context.Background(), llm.ToolCall{Name: "broken"})
	if err != nil || result.Code != "tool_error" || strings.Contains(result.Content, "private") {
		t.Fatalf("%+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = r.Execute(ctx, llm.ToolCall{Name: "get_time", Arguments: `{}`}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, text := range []string{strings.Repeat("中", MaxOutputBytes), strings.Repeat("\x00", MaxOutputBytes)} {
		wire := WireContent(Result{Content: text})
		var result Result
		if len(wire) > MaxOutputBytes || json.Unmarshal([]byte(wire), &result) != nil || !result.Truncated || !utf8.ValidString(result.Content) {
			t.Fatalf("invalid bounded output: bytes=%d", len(wire))
		}
	}
	// Mutating a returned schema must not mutate the shared registry.
	specs := r.Specs()
	specs[0].Parameters[0] = '!'
	if !json.Valid(r.Specs()[0].Parameters) {
		t.Fatal("shared schema mutated")
	}
}
