// Package protocol defines the local CLI/daemon wire format, independent of HTTP.
package protocol

const Version = 4

const (
	Started      = "turn.started"
	TextDelta    = "text.delta"
	Completed    = "turn.completed"
	Failed       = "turn.failed"
	RoundStarted = "model.round.started"
	ToolStarted  = "tool.started"
	ToolFinished = "tool.finished"
)

type ChatRequest struct {
	User    string `json:"user"`
	Session string `json:"session"`
	Message string `json:"message"`
}

// Event is one newline-delimited JSON object. Seq starts at 1 within a run.
type Event struct {
	User       string `json:"user"`
	Session    string `json:"session"`
	Version    int    `json:"version"`
	RunID      string `json:"run_id"`
	SessionID  string `json:"session_id"`
	Seq        int    `json:"seq"`
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	Error      string `json:"error,omitempty"`
	Round      int    `json:"round,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	Status     string `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type Health struct {
	Status     string `json:"status"`
	Protocol   int    `json:"protocol"`
	Provider   string `json:"provider,omitempty"`
	Agent      string `json:"agent"`
	InstanceID string `json:"instance_id,omitempty"`
	PID        int    `json:"pid,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ShutdownRequest struct {
	InstanceID string `json:"instance_id"`
}
