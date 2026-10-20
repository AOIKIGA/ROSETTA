// Package agent executes turns without knowing about sockets or HTTP.
package agent

import (
	"context"
	"fmt"

	"rosetta/internal/protocol"
)

// TextProvider is deliberately small for M1. M3 will add structured messages
// and tool calls. Implementations must honor ctx and call emit synchronously.
type TextProvider interface {
	Name() string
	Stream(ctx context.Context, message string, emit func(string) error) error
}

type Runtime struct {
	provider TextProvider
}

func New(provider TextProvider) *Runtime {
	return &Runtime{provider: provider}
}

func (r *Runtime) ProviderName() string { return r.provider.Name() }

// Run emits started/deltas/completed. The transport reports returned errors as
// failed events when the connection is still available.
func (r *Runtime) Run(ctx context.Context, req protocol.ChatRequest, emit func(protocol.Event) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := emit(protocol.Event{Type: protocol.Started}); err != nil {
		return err
	}
	err := r.provider.Stream(ctx, req.Message, func(text string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(protocol.Event{Type: protocol.TextDelta, Text: text})
	})
	if err != nil {
		return fmt.Errorf("provider stream: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return emit(protocol.Event{Type: protocol.Completed})
}
