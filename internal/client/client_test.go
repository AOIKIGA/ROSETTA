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
		return fmt.Sprintf(`{"version":1,"run_id":"run1","session_id":"main","seq":%d,"type":%q}`+"\n", seq, kind)
	}
	for _, tc := range []struct {
		name, body, want string
	}{
		{"complete", frame(1, protocol.Started) + frame(2, protocol.Completed), ""},
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
			err := c.Chat(context.Background(), protocol.ChatRequest{SessionID: "main", Message: "hello"}, func(protocol.Event) error { return nil })
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

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(t.url, "http://")
	return t.base.RoundTrip(clone)
}
