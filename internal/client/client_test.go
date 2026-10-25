package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rosetta/internal/protocol"
)

// Test the decoder with real HTTP chunks without depending on a live provider.
func TestStreamValidation(t *testing.T) {
	frame := func(seq int, kind string) string {
		return fmt.Sprintf(`{"version":%d,"run_id":"run1","session_id":"stored-id","user":"defaultuser","session":"main","seq":%d,"type":%q}`+"\n", protocol.Version, seq, kind)
	}
	for _, tc := range []struct {
		name, body, want string
	}{
		{"complete", frame(1, protocol.Started) + frame(2, protocol.Completed), ""},
		{"tool_events", frame(1, protocol.Started) + frame(2, protocol.RoundStarted) + frame(3, protocol.ToolStarted) + frame(4, protocol.ToolFinished) + frame(5, protocol.RoundStarted) + frame(6, protocol.TextDelta) + frame(7, protocol.Completed), ""},
		{"old_protocol", strings.Replace(frame(1, protocol.Started), fmt.Sprintf(`"version":%d`, protocol.Version), `"version":1`, 1), "sequence mismatch"},
		{"truncated", frame(1, protocol.Started), "incomplete"},
		{"sequence_gap", frame(1, protocol.Started) + frame(3, protocol.Completed), "sequence mismatch"},
		{"no_start", frame(1, protocol.Completed), "did not start"},
		{"failed", frame(1, protocol.Started) + frame(2, protocol.Failed), "turn failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			// Redirect the test-only HTTP client to the httptest address.
			c := &Client{http: &http.Client{Transport: rewriteTransport{base: srv.Client().Transport, url: srv.URL}}}
			defer c.Close()
			err := c.Chat(context.Background(), protocol.ChatRequest{Session: "main", Message: "hello"}, func(protocol.Event) error { return nil })
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v; want substring %q", err, tc.want)
			}
		})
	}
}

type rewriteTransport struct {
	base http.RoundTripper
	url  string
}

func TestPreviousProtocolCanOnlyBeUsedForShutdown(t *testing.T) {
	for _, version := range []int{2, 3, protocol.Version, 99} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status := "ready"
				if r.URL.Path == "/v1/shutdown" {
					status = "stopping"
				}
				fmt.Fprintf(w, `{"status":%q,"protocol":%d,"instance_id":"observed"}`, status, version)
			}))
			defer srv.Close()
			c := &Client{http: &http.Client{Transport: rewriteTransport{base: srv.Client().Transport, url: srv.URL}}}
			defer c.Close()
			ctx := context.Background()
			if _, err := c.Health(ctx); (err == nil) != (version == protocol.Version) {
				t.Fatalf("strict health: %v", err)
			}
			compatible := version == 3 || version == protocol.Version
			if _, err := c.LifecycleHealth(ctx); (err == nil) != compatible {
				t.Fatalf("lifecycle health: %v", err)
			}
			if err := c.Shutdown(ctx, "observed"); (err == nil) != compatible {
				t.Fatalf("shutdown: %v", err)
			}
			if err := c.Shutdown(ctx, "another-instance"); err == nil {
				t.Fatal("accepted acknowledgment from another instance")
			}
		})
	}
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(t.url, "http://")
	return t.base.RoundTrip(clone)
}
