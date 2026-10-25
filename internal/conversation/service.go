// Package conversation records a turn without coupling the model loop to SQL.
// Stage B records history but deliberately does not load it into the next prompt.
package conversation

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"rosetta/internal/agent"
	"rosetta/internal/llm"
	"rosetta/internal/protocol"
	"rosetta/internal/store"
)

var ErrDelivery = errors.New("completed turn could not be delivered")
var codePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

type Recorder interface {
	AppendMessage(context.Context, string, llm.Message) error
	FinishTurn(context.Context, string, string, string, *llm.Message) error
}

type Service struct {
	Runtime *agent.Runtime
	Store   Recorder
}

func (s Service) Run(ctx context.Context, turn store.Turn, req protocol.ChatRequest, emit func(protocol.Event) error) error {
	var final *llm.Message
	committed := false
	err := s.Runtime.RunRecorded(ctx, req, func(event protocol.Event) error {
		if event.Type == protocol.Completed {
			if err := s.Store.FinishTurn(ctx, turn.ID, "completed", "", final); err != nil {
				return &llm.Error{Code: "storage_error"}
			}
			committed = true
		}
		return emit(event)
	}, func(message llm.Message) error {
		if message.Role == "assistant" && len(message.ToolCalls) == 0 {
			final = &message
			return nil
		}
		if err := s.Store.AppendMessage(ctx, turn.ID, message); err != nil {
			return &llm.Error{Code: "storage_error"}
		}
		return nil
	})
	if committed {
		if err != nil {
			return fmt.Errorf("%w: %v", ErrDelivery, err)
		}
		return nil
	}
	if err == nil {
		err = &llm.Error{Code: "incomplete_turn"}
	}
	status := "failed"
	if errors.Is(err, context.Canceled) {
		status = "canceled"
	}
	// The request may already be canceled; cleanup still gets a bounded write.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if saveErr := s.Store.FinishTurn(cleanupCtx, turn.ID, status, ErrorCode(err), nil); saveErr != nil {
		return &llm.Error{Code: "storage_error"}
	}
	return err
}

func ErrorCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var public interface{ PublicCode() string }
	if errors.As(err, &public) && codePattern.MatchString(public.PublicCode()) {
		return public.PublicCode()
	}
	return "provider_error"
}
