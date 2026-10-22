// Package tool contains local capabilities. Tools cooperate with cancellation;
// deadlines are not a mechanism for killing arbitrary Go code.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"rosetta/internal/llm"
)

const MaxOutputBytes = 8 << 10

type Result struct {
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
	Code      string `json:"code,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type Tool interface {
	Spec() llm.ToolSpec
	Execute(context.Context, string) (Result, error)
}

// Registry is immutable after construction and safe to share between turns.
// Registered implementations must also support concurrent use.
type Registry struct {
	tools map[string]Tool
	specs []llm.ToolSpec
}

func NewRegistry(tools ...Tool) (*Registry, error) {
	r := &Registry{tools: make(map[string]Tool)}
	for _, t := range tools {
		spec := t.Spec()
		if strings.TrimSpace(spec.Name) == "" || !json.Valid(spec.Parameters) {
			return nil, fmt.Errorf("invalid tool specification")
		}
		if _, exists := r.tools[spec.Name]; exists {
			return nil, fmt.Errorf("duplicate tool name %q", spec.Name)
		}
		spec.Parameters = append(json.RawMessage(nil), spec.Parameters...)
		r.tools[spec.Name] = t
		r.specs = append(r.specs, spec)
	}
	return r, nil
}

func (r *Registry) Specs() []llm.ToolSpec {
	if r == nil {
		return nil
	}
	specs := append([]llm.ToolSpec(nil), r.specs...)
	for i := range specs {
		specs[i].Parameters = append(json.RawMessage(nil), specs[i].Parameters...)
	}
	return specs
}

func (r *Registry) Execute(ctx context.Context, call llm.ToolCall) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if r == nil || r.tools[call.Name] == nil {
		return Result{IsError: true, Code: "unknown_tool", Content: "Choose a tool from the provided tool list."}, nil
	}
	result, err := r.tools[call.Name].Execute(ctx, call.Arguments)
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if err != nil {
		// Tool implementations return safe business errors as Result; unexpected
		// errors are sanitized here instead of being exposed to the model.
		return Result{IsError: true, Code: "tool_error", Content: "The tool could not complete."}, nil
	}
	return limitResult(result), nil
}

// WireContent caps the entire JSON tool result, including encoding overhead.
func WireContent(result Result) string {
	result = limitResult(result)
	for {
		data, _ := json.Marshal(result)
		if len(data) <= MaxOutputBytes {
			return string(data)
		}
		result.Truncated = true
		result.Content = truncateUTF8(result.Content, len(result.Content)/2)
	}
}

func limitResult(result Result) Result {
	if len(result.Content) > MaxOutputBytes {
		result.Content = truncateUTF8(result.Content, MaxOutputBytes)
		result.Truncated = true
	}
	if len(result.Code) > 64 {
		result.Code = "tool_error"
	}
	return result
}

func truncateUTF8(s string, size int) string {
	if len(s) <= size {
		return s
	}
	for size > 0 && !utf8.RuneStart(s[size]) {
		size--
	}
	return s[:size]
}
