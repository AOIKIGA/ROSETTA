package agent

import (
	"context"
	"errors"
	"testing"

	"rosetta/internal/protocol"
)

type providerFunc func(context.Context, string, func(string) error) error

func (providerFunc) Name() string { return "test" }

func (f providerFunc) Stream(ctx context.Context, message string, emit func(string) error) error {
	return f(ctx, message, emit)
}

func TestPartialProviderFailureNeverCompletes(t *testing.T) {
	want := errors.New("upstream disconnected")
	r := New(providerFunc(func(ctx context.Context, message string, emit func(string) error) error {
		if err := emit("partial"); err != nil {
			return err
		}
		return want
	}))
	var events []protocol.Event
	err := r.Run(context.Background(), protocol.ChatRequest{Message: "hello"}, func(e protocol.Event) error {
		events = append(events, e)
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if len(events) != 2 || events[0].Type != protocol.Started || events[1].Text != "partial" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestConsumerErrorStopsProvider(t *testing.T) {
	want := errors.New("output closed")
	r := New(providerFunc(func(ctx context.Context, message string, emit func(string) error) error {
		return emit("hello")
	}))
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
