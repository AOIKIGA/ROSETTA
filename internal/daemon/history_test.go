package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rosetta/internal/agent"
	"rosetta/internal/identity"
	"rosetta/internal/provider"
	"rosetta/internal/store"
)

func attachTestStore(t *testing.T, d *Daemon) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	d.store = db
}

func TestDifferentUsersMayRunSameSessionName(t *testing.T) {
	p := blockingProvider{entered: make(chan struct{}, 2)}
	d := New(agent.New(p, nil), testLogger())
	attachTestStore(t, d)
	handler := d.Handler()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{}, 2)
	for _, user := range []string{"alice", "bob"} {
		req := httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"user":"`+user+`","session":"main","message":"hello"}`)).WithContext(ctx)
		go func() { handler.ServeHTTP(httptest.NewRecorder(), req); done <- struct{}{} }()
		select {
		case <-p.entered:
		case <-ctx.Done():
			t.Fatal("same session name blocked another user")
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"user":"alice","session":"main","message":"again"}`)))
	if w.Code != 409 {
		t.Fatalf("same user's active session not blocked: %d", w.Code)
	}
	cancel()
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("cancel did not release turn")
		}
	}
	for _, user := range []string{"alice", "bob"} {
		page, err := d.store.History(context.Background(), identity.Scope{User: user, Session: "main"}, 0, 20)
		if err != nil || len(page.Messages) != 1 || page.Messages[0].Turn.Status != "canceled" {
			t.Fatalf("%+v %v", page, err)
		}
	}
}

func TestHistoryEndpointsAreScopedAndDoNotCreateMissingSessions(t *testing.T) {
	d := New(agent.New(provider.Mock{}, nil), testLogger())
	attachTestStore(t, d)
	handler := d.Handler()
	for _, user := range []string{"alice", "bob"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"user":"`+user+`","session":"main","message":"`+user+`-private"}`)))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "turn.completed") {
			t.Fatal(w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/v1/history?user=alice&session=main", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "alice-private") || strings.Contains(w.Body.String(), "bob-private") {
		t.Fatal(w.Body.String())
	}
	var page store.HistoryPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Session.User != "alice" || len(page.Messages) != 2 {
		t.Fatalf("%+v %v", page, err)
	}
	for _, tc := range []struct {
		path string
		code int
	}{{"/v1/history?user=missing&session=main", 404}, {"/v1/history?user=alice&session=missing", 404}, {"/v1/history?user=alice&after=-1", 400}, {"/v1/sessions?limit=101", 400}, {"/v1/sessions?user=../bad", 400}} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
}
