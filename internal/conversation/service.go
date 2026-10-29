// Package conversation records a turn without coupling the model loop to SQL.
// Completed history is selected within the current persisted session.
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
	LoadContext(context.Context, string) ([]llm.HistoryTurn, error)
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
	// Send started before the bounded read, including when storage fails. The
	// stream always starts with one started event and can then report failure.
	err := emit(protocol.Event{Type: protocol.Started})
	var history []llm.HistoryTurn
	if err == nil {
		loadCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		history, err = s.Store.LoadContext(loadCtx, turn.ID)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				err = &llm.Error{Code: "storage_error"}
			}
		}
	}
	if err == nil {
		err = s.Runtime.RunWithHistory(ctx, req, history, func(event protocol.Event) error {
			if event.Type == protocol.Started {
				return nil // Service already announced the turn before loading history.
			}
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
	}
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
