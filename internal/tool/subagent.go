package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"rosetta/internal/llm"
	"rosetta/internal/task"
)

// Subagent is a blocking adapter over Start/Status/Wait, not a second task engine.
type Subagent struct{}

func (Subagent) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "subagent", Description: "Delegate one self-contained read-only task to a child assistant and wait for its final answer. It has time and workspace file tools, but no parent conversation and cannot delegate again. At most two children per parent turn. Supply all necessary context in prompt.", Parameters: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"A complete task and required context; maximum 8192 UTF-8 bytes"}},"required":["prompt"],"additionalProperties":false}`)}
}

func (Subagent) Timeout() time.Duration { return time.Minute }

func (Subagent) Execute(ctx context.Context, arguments string) (Result, error) {
	var args struct {
		Prompt string `json:"prompt"`
	}
	if !decodeObject(arguments, &args) || strings.TrimSpace(args.Prompt) == "" || len(args.Prompt) > 8192 {
		return failure("invalid_arguments", "Expected one nonblank prompt of at most 8192 bytes."), nil
	}
	service := task.FromContext(ctx)
	if service == nil {
		return failure("subagent_unavailable", "Subagents are not available in this execution."), nil
	}
	snapshot, err := service.Start(ctx, task.Request{Prompt: args.Prompt})
	if errors.Is(err, task.ErrLimit) {
		return failure("subagent_limit", "At most two child tasks are allowed per parent turn."), nil
	}
	if err != nil {
		return Result{}, err
	}
	snapshot, err = service.Wait(ctx, snapshot.ID)
	if err != nil {
		return Result{}, err
	}
	// Preserve structured JSON even if escaping makes a long answer exceed the wire budget.
	for {
		data, err := json.Marshal(snapshot)
		if err != nil {
			return Result{}, err
		}
		result := Result{Content: string(data), IsError: snapshot.Status != "completed", Code: snapshot.Code, Truncated: snapshot.Truncated}
		if fitsWire(result) {
			return result, nil
		}
		snapshot.Output = truncateUTF8(snapshot.Output, len(snapshot.Output)/2)
		snapshot.Truncated = true
	}
}
