package conversation

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"rosetta/internal/agent"
	"rosetta/internal/identity"
	"rosetta/internal/llm"
	"rosetta/internal/protocol"
	"rosetta/internal/store"
)

type providerFunc func(context.Context, llm.Request, func(string) error) (llm.Response, error)

func (providerFunc) Name() string { return "test" }
func (f providerFunc) Stream(ctx context.Context, r llm.Request, emit func(string) error) (llm.Response, error) {
	return f(ctx, r, emit)
}

func setup(t *testing.T) (*store.Store, identity.Scope) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), "personal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, identity.Scope{User: "alice", Session: "main"}
}

func TestCompletionIsCommittedBeforeNotificationWithoutLoadingHistory(t *testing.T) {
	db, scope := setup(t)
	ctx := context.Background()
	provider := providerFunc(func(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
		if len(req.Messages) != 2 {
			t.Fatal("stage B unexpectedly loaded history")
		}
		text := "answer to " + req.Messages[1].Content
		if err := emit(text); err != nil {
			return llm.Response{}, err
		}
		return llm.Response{Message: llm.Message{Role: "assistant", Content: text}}, nil
	})
	service := Service{Runtime: agent.New(provider, nil), Store: db}
	for _, id := range []string{"first", "second"} {
		turn, err := db.BeginTurn(ctx, scope, id, id, "test")
		if err != nil {
			t.Fatal(err)
		}
		err = service.Run(ctx, turn, protocol.ChatRequest{Message: id}, func(e protocol.Event) error {
			if e.Type == protocol.Completed {
				page, err := db.History(ctx, scope, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				last := page.Messages[len(page.Messages)-1]
				if last.Content != "answer to "+id || last.Turn.Status != "completed" {
					t.Fatal("completion sent before commit")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCanceledRequestAndCompletionDeliveryFailure(t *testing.T) {
	for _, kind := range []string{"cancel", "delivery"} {
		t.Run(kind, func(t *testing.T) {
			db, scope := setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			turn, err := db.BeginTurn(ctx, scope, kind, "hello", "test")
			if err != nil {
				t.Fatal(err)
			}
			p := providerFunc(func(ctx context.Context, _ llm.Request, emit func(string) error) (llm.Response, error) {
				if kind == "cancel" {
					cancel()
					return llm.Response{}, ctx.Err()
				}
				if err := emit("answer"); err != nil {
					return llm.Response{}, err
				}
				return llm.Response{Message: llm.Message{Role: "assistant", Content: "answer"}}, nil
			})
			err = (Service{Runtime: agent.New(p, nil), Store: db}).Run(ctx, turn, protocol.ChatRequest{Message: "hello"}, func(e protocol.Event) error {
				if e.Type == protocol.Completed {
					return errors.New("connection lost")
				}
				return nil
			})
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if kind == "delivery" && !errors.Is(err, ErrDelivery) {
				t.Fatal(err)
			}
			page, err := db.History(context.Background(), scope, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			want := "canceled"
			count := 1
			if kind == "delivery" {
				want = "completed"
				count = 2
			}
			if len(page.Messages) != count || page.Messages[0].Turn.Status != want {
				t.Fatalf("%+v", page)
			}
		})
	}
}

type failingCommit struct{ *store.Store }

func (f failingCommit) FinishTurn(ctx context.Context, id, status, code string, final *llm.Message) error {
	if status == "completed" {
		return errors.New("disk full")
	}
	return f.Store.FinishTurn(ctx, id, status, code, final)
}
func TestStorageFailureNeverEmitsCompleted(t *testing.T) {
	db, scope := setup(t)
	ctx := context.Background()
	turn, err := db.BeginTurn(ctx, scope, "one", "hello", "test")
	if err != nil {
		t.Fatal(err)
	}
	p := providerFunc(func(ctx context.Context, _ llm.Request, emit func(string) error) (llm.Response, error) {
		if err := emit("answer"); err != nil {
			return llm.Response{}, err
		}
		return llm.Response{Message: llm.Message{Role: "assistant", Content: "answer"}}, nil
	})
	err = (Service{Runtime: agent.New(p, nil), Store: failingCommit{db}}).Run(ctx, turn, protocol.ChatRequest{Message: "hello"}, func(e protocol.Event) error {
		if e.Type == protocol.Completed {
			t.Fatal("false success")
		}
		return nil
	})
	if ErrorCode(err) != "storage_error" {
		t.Fatal(err)
	}
	page, err := db.History(ctx, scope, 0, 20)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Turn.Status != "failed" {
		t.Fatalf("%+v %v", page, err)
	}
}
