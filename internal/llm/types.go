// Package llm defines model data independently of HTTP and local UI events.
package llm

import (
	"context"
	"encoding/json"
)

// Arguments stays a string so malformed model JSON can be returned as a tool
// error without losing the original assistant message.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Request struct {
	Messages []Message  `json:"messages"`
	Tools    []ToolSpec `json:"tools,omitempty"`
}

type Response struct {
	Message Message
}

// Stream emits text synchronously, then returns the complete assistant message.
// Tool calls are available only after a fully completed response. Implementations
// must honor ctx and stop when onText returns an error.
type Provider interface {
	Name() string
	Stream(ctx context.Context, req Request, onText func(string) error) (Response, error)
}

// InputSizer optionally measures an adapter's exact encoded request body.
// It must be deterministic and must not perform network or model calls.
type InputSizer interface {
	InputSize(Request) (int, error)
}

// Error exposes only a controlled category to clients, never raw API bodies.
type Error struct{ Code string }

func (e *Error) Error() string      { return e.Code }
func (e *Error) PublicCode() string { return e.Code }

const (
	MaxInputBytes    = 64 << 10
	MaxResponseBytes = 64 << 10
	MaxToolCalls     = 4
)
