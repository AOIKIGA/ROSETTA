// Package protocol defines the local CLI/daemon wire format, independent of HTTP.
package protocol

const Version = 1

const (
	Started   = "turn.started"
	TextDelta = "text.delta"
	Completed = "turn.completed"
	Failed    = "turn.failed"
)

type ChatRequest struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// Event is one newline-delimited JSON object. Seq starts at 1 within a run.
type Event struct {
	Version   int    `json:"version"`
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	Seq       int    `json:"seq"`
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Health struct {
	Status   string `json:"status"`
	Protocol int    `json:"protocol"`
	Provider string `json:"provider"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
