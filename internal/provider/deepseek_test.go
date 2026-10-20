package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const textFrame = "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"},\"finish_reason\":null}]}\n\n"
const stopFrame = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
const doneFrame = "data: [DONE]\n\n"

func TestDeepSeekRequestAndStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Error("incorrect method or authorization")
		}
		var req struct {
			Model     string `json:"model"`
			Stream    bool   `json:"stream"`
			MaxTokens int    `json:"max_tokens"`
			Thinking  struct {
				Type string `json:"type"`
			} `json:"thinking"`
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != DefaultDeepSeekModel || !req.Stream || req.MaxTokens != 64 || req.Thinking.Type != "disabled" || len(req.Messages) != 1 || req.Messages[0].Content != "hello" || req.Messages[0].Role != "user" {
			t.Errorf("unexpected request: %+v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": keepalive\n\n"+textFrame+stopFrame+doneFrame)
	}))
	defer srv.Close()
	p, err := NewDeepSeek("test-credential", DefaultDeepSeekModel, 64)
	if err != nil {
		t.Fatal(err)
	}
	p.endpoint = srv.URL
	var text strings.Builder
	err = p.Stream(context.Background(), "hello", func(delta string) error { text.WriteString(delta); return nil })
	if err != nil || text.String() != "你好" {
		t.Fatalf("text=%q err=%v", text.String(), err)
	}
}

func TestDeepSeekIncompleteAndFailedStreams(t *testing.T) {
	for _, tc := range []struct{ name, input, code string }{
		{"truncated", textFrame, "provider_incomplete_response"},
		{"no_done", textFrame + stopFrame, "provider_incomplete_response"},
		{"no_stop", textFrame + doneFrame, "provider_incomplete_response"},
		{"empty", stopFrame + doneFrame, "provider_incomplete_response"},
		{"limit", textFrame + strings.ReplaceAll(stopFrame, "stop", "length"), "provider_output_limit"},
		{"tool", textFrame + strings.ReplaceAll(stopFrame, "stop", "tool_calls"), "provider_unsupported_finish"},
		{"bad_json", "data: {bad}\n\n", "provider_invalid_event"},
		{"upstream_error", "data: {\"error\":{\"message\":\"sensitive text\"}}\n\n", "provider_stream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := readDeepSeekStream(context.Background(), strings.NewReader(tc.input), func(string) error { return nil })
			var public *PublicError
			if !errors.As(err, &public) || public.Code != tc.code {
				t.Fatalf("err=%v; want %s", err, tc.code)
			}
		})
	}
}

func TestDeepSeekMultilineSSEAndConsumerError(t *testing.T) {
	input := "data: {\"choices\":\r\ndata: [{\"delta\":{\"content\":\"hello\"}}]}\r\n\r\n" + stopFrame + doneFrame
	want := errors.New("consumer closed")
	err := readDeepSeekStream(context.Background(), strings.NewReader(input), func(string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
}

func TestDeepSeekHTTPErrorDoesNotExposeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, "sensitive test response")
	}))
	defer srv.Close()
	p, _ := NewDeepSeek("test-credential", DefaultDeepSeekModel, 64)
	p.endpoint = srv.URL
	err := p.Stream(context.Background(), "hello", func(string) error { return nil })
	if err == nil || err.Error() != "provider_http_401" {
		t.Fatalf("err=%v", err)
	}
}

func TestDeepSeekCancellationClosesUpstream(t *testing.T) {
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, textFrame)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer srv.Close()
	p, _ := NewDeepSeek("test-credential", DefaultDeepSeekModel, 64)
	p.endpoint = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := p.Stream(ctx, "hello", func(string) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not close")
	}
}

func TestDeepSeekRejectsRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/unexpected", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	p, _ := NewDeepSeek("test-credential", DefaultDeepSeekModel, 64)
	p.endpoint = srv.URL
	err := p.Stream(context.Background(), "hello", func(string) error { return nil })
	if err == nil || err.Error() != "provider_http_307" {
		t.Fatalf("err=%v", err)
	}
}
