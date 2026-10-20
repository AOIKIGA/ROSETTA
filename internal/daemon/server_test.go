package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rosetta/internal/agent"
	"rosetta/internal/provider"
)

type blockingProvider struct {
	entered chan struct{}
}

func (blockingProvider) Name() string { return "test" }

func (p blockingProvider) Stream(ctx context.Context, _ string, _ func(string) error) error {
	p.entered <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestInvalidRequests(t *testing.T) {
	s := New(agent.New(provider.Mock{}), testLogger())
	for _, body := range []string{
		`{}`, `{"session_id":"../bad","message":"hi"}`,
		`{"session_id":"main","message":" "}`,
		`{"session_id":"main","message":"hi","unknown":true}`,
		`{"session_id":"main","message":"hi"} {}`,
		`{"session_id":"main","message":"` + strings.Repeat("x", 17<<10) + `"}`,
		`{"session_id":"main","message":"` + strings.Repeat("x", 65<<10) + `"}`,
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	}
}

func TestAdmissionAndCancellationRelease(t *testing.T) {
	p := blockingProvider{entered: make(chan struct{}, 4)}
	s := New(agent.New(p), testLogger())
	handler := s.Handler()
	var cancels []context.CancelFunc
	var done []chan struct{}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	for _, session := range []string{"a", "b", "c", "d"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cancels = append(cancels, cancel)
		finished := make(chan struct{})
		done = append(done, finished)
		req := httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"session_id":"`+session+`","message":"hi"}`)).WithContext(ctx)
		go func() {
			handler.ServeHTTP(httptest.NewRecorder(), req)
			close(finished)
		}()
		select {
		case <-p.entered:
		case <-ctx.Done():
			t.Fatal("provider did not start")
		}
	}
	for _, tc := range []struct {
		session string
		status  int
	}{{"a", 409}, {"e", 429}} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"session_id":"`+tc.session+`","message":"hi"}`)))
		if w.Code != tc.status {
			t.Fatalf("status = %d, want %d", w.Code, tc.status)
		}
	}
	for i, cancel := range cancels {
		cancel()
		select {
		case <-done[i]:
		case <-time.After(time.Second):
			t.Fatal("canceled handler did not exit")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.active) != 0 {
		t.Fatalf("canceled turns leaked permits: %v", s.active)
	}
}
