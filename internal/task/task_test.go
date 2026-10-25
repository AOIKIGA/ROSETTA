package task

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStartStatusWaitIsolationAndLimit(t *testing.T) {
	entered, finish := make(chan struct{}, 2), make(chan struct{})
	m := New(func(ctx context.Context, req Request, report func(Progress)) (string, error) {
		report(Progress{Round: 2, Tool: "read_files"})
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-finish:
			return "answer: " + req.Prompt, nil
		}
	})
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, err := m.Start(ctx, Request{Prompt: "one"})
	if err != nil || first.Status != "running" {
		t.Fatalf("%+v %v", first, err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	status, err := m.Status(ctx, first.ID)
	if err != nil || status.Progress.Round != 2 || status.Progress.Tool != "read_files" {
		t.Fatalf("%+v %v", status, err)
	}
	other := New(nil)
	defer other.Close()
	if _, err := other.Status(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-turn lookup succeeded")
	}
	waitCtx, cancelWait := context.WithCancel(ctx)
	cancelWait()
	if _, err := m.Wait(waitCtx, first.ID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s, _ := m.Status(ctx, first.ID); s.Status != "running" {
		t.Fatal("canceling waiter canceled task")
	}
	if _, err := m.Start(ctx, Request{Prompt: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, Request{Prompt: "three"}); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	close(finish)
	result, err := m.Wait(ctx, first.ID)
	if err != nil || result.Status != "completed" || result.Output != "answer: one" {
		t.Fatalf("%+v %v", result, err)
	}
	// Returned snapshots are copies, including completion timestamps.
	*result.FinishedAt = time.Time{}
	if s, _ := m.Status(ctx, first.ID); s.FinishedAt.IsZero() {
		t.Fatal("snapshot aliases manager memory")
	}
	m.Close()
	if _, err := m.Start(ctx, Request{}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestCloseCancelsChildrenAndSanitizesFailures(t *testing.T) {
	m := New(func(ctx context.Context, _ Request, _ func(Progress)) (string, error) {
		<-ctx.Done()
		return "partial", ctx.Err()
	})
	h, err := m.Start(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	result, err := m.Status(context.Background(), h.ID)
	if err != nil || result.Status != "canceled" || result.Output != "" {
		t.Fatalf("%+v %v", result, err)
	}
	for _, tc := range []struct {
		output string
		err    error
		code   string
	}{{"", errors.New("secret credential"), "subagent_failed"}, {strings.Repeat("中", MaxResultBytes), nil, ""}} {
		m := New(func(context.Context, Request, func(Progress)) (string, error) { return tc.output, tc.err })
		h, err := m.Start(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}
		result, err := m.Wait(context.Background(), h.ID)
		m.Close()
		if err != nil || result.Code != tc.code || strings.Contains(result.Code, "secret") || len(result.Output) > MaxResultBytes {
			t.Fatalf("%+v %v", result, err)
		}
	}
}
