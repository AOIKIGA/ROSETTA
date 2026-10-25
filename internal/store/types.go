// Package store persists agent-local users, sessions and complete messages.
package store

import (
	"errors"

	"rosetta/internal/llm"
)

var ErrNotFound = errors.New("session not found")
var ErrTurnClosed = errors.New("turn is not running")

const MaxPageBytes = 2 << 20

type Session struct {
	ID        string `json:"id"`
	User      string `json:"user"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Turns     int    `json:"turns"`
}

type Turn struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	Provider   string `json:"provider"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type Message struct {
	ID      int64 `json:"id"`
	Ordinal int   `json:"ordinal"`
	Turn    Turn  `json:"turn"`
	llm.Message
}

type SessionPage struct {
	Sessions  []Session `json:"sessions"`
	NextAfter string    `json:"next_after,omitempty"`
}

type HistoryPage struct {
	Session   Session   `json:"session"`
	Messages  []Message `json:"messages"`
	NextAfter int64     `json:"next_after,omitempty"`
}
