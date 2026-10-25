package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"rosetta/internal/identity"
	"rosetta/internal/llm"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(context.Background(), path, "personal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestIdentityIsolationMessagesAndRestart(t *testing.T) {
	s, path := testStore(t)
	ctx := context.Background()
	seen := map[string]bool{}
	for i, scope := range []identity.Scope{{User: "alice", Session: "main"}, {User: "bob", Session: "main"}, {User: "alice", Session: "learning"}} {
		turn, err := s.BeginTurn(ctx, scope, fmt.Sprintf("run%d", i), scope.User+" question", "mock")
		if err != nil {
			t.Fatal(err)
		}
		if seen[turn.SessionID] {
			t.Fatal("sessions share identity")
		}
		seen[turn.SessionID] = true
		if err := s.AppendMessage(ctx, turn.ID, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call", Name: "get_time", Arguments: `{}`}}}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendMessage(ctx, turn.ID, llm.Message{Role: "tool", ToolCallID: "call", Content: "time result"}); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishTurn(ctx, turn.ID, "completed", "", &llm.Message{Role: "assistant", Content: scope.User + " answer"}); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishTurn(ctx, turn.ID, "canceled", "canceled", nil); !errors.Is(err, ErrTurnClosed) {
			t.Fatal("completed status overwritten", err)
		}
	}
	if _, err := s.History(ctx, identity.Scope{User: "bob", Session: "learning"}, 0, 20); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-user session found", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path, "personal")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	history, err := reopened.History(ctx, identity.Scope{User: "alice", Session: "main"}, 0, 20)
	if err != nil || len(history.Messages) != 4 {
		t.Fatalf("%+v %v", history, err)
	}
	for i, role := range []string{"user", "assistant", "tool", "assistant"} {
		if history.Messages[i].Role != role || history.Messages[i].Ordinal != i+1 || history.Messages[i].Turn.Status != "completed" {
			t.Fatalf("%+v", history.Messages)
		}
	}
	if history.Messages[1].ToolCalls[0].ID != history.Messages[2].ToolCallID {
		t.Fatal("lost assistant/tool pairing")
	}
	if strings.Contains(history.Messages[0].Content, "bob") {
		t.Fatal("cross-user messages")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("database permissions: %v %v", info, err)
	}
	var foreignKeys int
	var journal string
	if err := reopened.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatal("foreign keys disabled", err)
	}
	if err := reopened.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatal("WAL not active", err)
	}
}

func TestInterruptedRecoveryKeepsUnfinishedMessages(t *testing.T) {
	s, path := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{User: "defaultuser", Session: "main"}
	turn, err := s.BeginTurn(ctx, scope, "unfinished", "pending", "mock")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(ctx, turn.ID, llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "not-run", Name: "get_time", Arguments: `{}`}}}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(ctx, path, "personal")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.History(ctx, scope, 0, 20)
	if err != nil || len(page.Messages) != 2 {
		t.Fatalf("%+v %v", page, err)
	}
	for _, message := range page.Messages {
		if message.Turn.Status != "interrupted" || message.Turn.Error != "daemon_interrupted" {
			t.Fatalf("%+v", message)
		}
	}
}

func TestHistoryByteBudgetPreservesWholeMessages(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{User: "alice", Session: "main"}
	turn, err := s.BeginTurn(ctx, scope, "large", "question", "mock")
	if err != nil {
		t.Fatal(err)
	}
	// JSON escapes '<' to six bytes. Budget the encoded wire size, not content length.
	content := strings.Repeat("<", 128<<10)
	for range 4 {
		if err := s.AppendMessage(ctx, turn.ID, llm.Message{Role: "assistant", Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	count, after := 0, int64(0)
	for {
		page, err := s.History(ctx, scope, after, 100)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(page)
		if err != nil || len(encoded) > MaxPageBytes {
			t.Fatalf("response bytes=%d, error=%v", len(encoded), err)
		}
		for _, message := range page.Messages {
			if message.Role == "assistant" && message.Content != content {
				t.Fatal("message was truncated")
			}
			count++
		}
		if page.NextAfter == 0 {
			break
		}
		if page.NextAfter <= after {
			t.Fatal("cursor did not advance")
		}
		after = page.NextAfter
	}
	if count != 5 {
		t.Fatalf("lost or duplicated messages: %d", count)
	}
}

func TestPaginationReadOnlyQueriesAndAtomicBegin(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if page, err := s.Sessions(ctx, "missing", "", 20); err != nil || len(page.Sessions) != 0 {
		t.Fatalf("%+v %v", page, err)
	}
	if _, err := s.History(ctx, identity.Scope{User: "missing", Session: "main"}, 0, 20); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil || count != 0 {
		t.Fatal("query created a user", err)
	}
	scope := identity.Scope{User: "alice", Session: "main"}
	turn, err := s.BeginTurn(ctx, scope, "one", "question", "mock")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(ctx, turn.ID, "completed", "", &llm.Message{Role: "assistant", Content: "answer"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.History(ctx, scope, 0, 1)
	if err != nil || len(first.Messages) != 1 || first.NextAfter == 0 {
		t.Fatalf("%+v %v", first, err)
	}
	second, err := s.History(ctx, scope, first.NextAfter, 1)
	if err != nil || len(second.Messages) != 1 || second.NextAfter != 0 || second.Messages[0].Content != "answer" {
		t.Fatalf("%+v %v", second, err)
	}
	if _, err := s.BeginTurn(ctx, identity.Scope{User: "bob", Session: "new"}, "one", "duplicate", "mock"); err == nil {
		t.Fatal("duplicate run accepted")
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users WHERE name='bob'").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed transaction created identity", err)
	}
}

func TestConcurrentIdentityCreation(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{User: "alice", Session: "main"}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if _, err := s.BeginTurn(ctx, scope, fmt.Sprintf("run%d", i), "hello", "mock"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	page, err := s.Sessions(ctx, "alice", "", 20)
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].Turns != 8 {
		t.Fatalf("%+v %v", page, err)
	}
}

func TestSchemaAndAgentGuards(t *testing.T) {
	s, path := testStore(t)
	_ = s.Close()
	if other, err := Open(context.Background(), path, "work"); err == nil {
		other.Close()
		t.Fatal("accepted another agent's database")
	}
	s, err := Open(context.Background(), path, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if future, err := Open(context.Background(), path, "personal"); err == nil {
		future.Close()
		t.Fatal("opened future schema")
	}
	link := filepath.Join(t.TempDir(), "linked.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if linked, err := Open(context.Background(), link, "personal"); err == nil {
		linked.Close()
		t.Fatal("followed database symlink")
	}
}
