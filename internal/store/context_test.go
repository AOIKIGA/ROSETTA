package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"rosetta/internal/identity"
	"rosetta/internal/llm"
)

func completeContextTurn(t *testing.T, s *Store, scope identity.Scope, id, answer string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.BeginTurn(ctx, scope, id, id, "mock"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(ctx, id, "completed", "", &llm.Message{Role: "assistant", Content: answer}); err != nil {
		t.Fatal(err)
	}
}

func TestContextUsesPersistedSessionAndCompletedPastTurnsOnly(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{User: "alice", Session: "main"}
	completeContextTurn(t, s, scope, "first", "first answer")
	completeContextTurn(t, s, identity.Scope{User: "bob", Session: "main"}, "other-user", "private")
	completeContextTurn(t, s, identity.Scope{User: "alice", Session: "work"}, "other-session", "private")
	for _, status := range []string{"failed", "canceled", "running", "interrupted"} {
		if _, err := s.BeginTurn(ctx, scope, status, "exclude me", "mock"); err != nil {
			t.Fatal(err)
		}
		if status == "failed" || status == "canceled" {
			if err := s.FinishTurn(ctx, status, status, status, nil); err != nil {
				t.Fatal(err)
			}
		}
		if status == "interrupted" {
			if _, err := s.db.Exec("UPDATE turns SET status='interrupted' WHERE id=?", status); err != nil {
				t.Fatal(err)
			}
		}
	}
	completeContextTurn(t, s, scope, "second", "second answer")
	if _, err := s.BeginTurn(ctx, scope, "current", "current input", "mock"); err != nil {
		t.Fatal(err)
	}
	// The current request is a stable boundary even if later data exists.
	completeContextTurn(t, s, scope, "future", "future answer")
	history, err := s.LoadContext(ctx, "current")
	if err != nil || len(history) != 2 {
		t.Fatalf("%+v %v", history, err)
	}
	if history[0].Messages[0].Content != "first" || history[1].Messages[0].Content != "second" {
		t.Fatalf("incorrect order/ownership: %+v", history)
	}
	if _, err := s.LoadContext(ctx, "nonexistent"); err == nil {
		t.Fatal("missing anchor accepted")
	}
}

func TestContextKeepsToolExchangesAndRejectsBrokenCompletedTurn(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			s, _ := testStore(t)
			ctx := context.Background()
			scope := identity.Scope{User: "alice", Session: "main"}
			_, err := s.BeginTurn(ctx, scope, "tools", "time?", "mock")
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range []llm.Message{
				{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call", Name: "get_time", Arguments: "{broken"}}},
				{Role: "tool", ToolCallID: "call", Content: "invalid arguments"},
			} {
				if broken && m.Role == "tool" {
					continue
				}
				if err := s.AppendMessage(ctx, "tools", m); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.FinishTurn(ctx, "tools", "completed", "", &llm.Message{Role: "assistant", Content: "could not get time"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.BeginTurn(ctx, scope, "current", "continue", "mock"); err != nil {
				t.Fatal(err)
			}
			history, err := s.LoadContext(ctx, "current")
			if broken {
				if err == nil {
					t.Fatal("orphaned tool call accepted")
				}
				return
			}
			if err != nil || len(history) != 1 || len(history[0].Messages) != 4 || history[0].Messages[2].ToolCallID != "call" {
				t.Fatalf("%+v %v", history, err)
			}
		})
	}
}

func TestContextBoundsNewestWholeTurnsWithoutDeletingHistory(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{User: "alice", Session: "main"}
	for i := 0; i < 23; i++ {
		completeContextTurn(t, s, scope, fmt.Sprintf("turn%02d", i), "answer")
	}
	if _, err := s.BeginTurn(ctx, scope, "count-bound", "question", "mock"); err != nil {
		t.Fatal(err)
	}
	history, err := s.LoadContext(ctx, "count-bound")
	if err != nil || len(history) != 20 || history[0].Messages[0].Content != "turn03" || history[19].Messages[0].Content != "turn22" {
		t.Fatalf("%+v %v", history, err)
	}
	// The raw text fits but its escaped JSON does not. Do not jump back to
	// older turns after this oversized completed turn.
	completeContextTurn(t, s, scope, "oversized", strings.Repeat("<", 6<<10))
	completeContextTurn(t, s, scope, "latest", "small answer")
	if _, err := s.BeginTurn(ctx, scope, "byte-bound", "question", "mock"); err != nil {
		t.Fatal(err)
	}
	history, err = s.LoadContext(ctx, "byte-bound")
	if err != nil || len(history) != 1 || history[0].Messages[0].Content != "latest" {
		t.Fatalf("%+v %v", history, err)
	}
	page, err := s.History(ctx, scope, 0, 100)
	if err != nil || len(page.Messages) != 52 {
		t.Fatalf("context selection deleted stored data: messages=%d err=%v", len(page.Messages), err)
	}
}
