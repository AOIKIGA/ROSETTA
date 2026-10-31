// Package task manages child work independently of how it is exposed as a tool.
// A Manager belongs to one parent turn; task IDs never grant access to another turn.
package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
	"unicode/utf8"
)

const MaxTasks = 2
const MaxResultBytes = 4096

var (
	ErrLimit    = errors.New("subagent_limit")
	ErrNotFound = errors.New("subagent_not_found")
	ErrClosed   = errors.New("subagent_manager_closed")
)

type Request struct{ Prompt string }

type Progress struct {
	Round int    `json:"round,omitempty"`
	Tool  string `json:"tool,omitempty"`
}

type Snapshot struct {
	ID         string     `json:"task_id"`
	Status     string     `json:"status"`
	Progress   Progress   `json:"progress"`
	Output     string     `json:"output,omitempty"`
	Code       string     `json:"error,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Runner must honor cancellation; report must be called synchronously by Runner.
type Runner func(context.Context, Request, func(Progress)) (string, error)

// Service is the stable boundary for a future status/wait command or async tool.
// Canceling Wait stops waiting; it does not cancel the underlying task.
type Service interface {
	Start(context.Context, Request) (Snapshot, error)
	Status(context.Context, string) (Snapshot, error)
	Wait(context.Context, string) (Snapshot, error)
}

type record struct {
	snapshot Snapshot
	done     chan struct{}
	cancel   context.CancelFunc
}

type Manager struct {
	mu      sync.Mutex
	runner  Runner
	records map[string]*record
	closed  bool
	wg      sync.WaitGroup
}

func New(runner Runner) *Manager { return &Manager{runner: runner, records: make(map[string]*record)} }

func (m *Manager) Start(ctx context.Context, req Request) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Snapshot{}, ErrClosed
	}
	if len(m.records) >= MaxTasks {
		return Snapshot{}, ErrLimit
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Snapshot{}, err
	}
	childCtx, cancel := context.WithCancel(ctx)
	r := &record{snapshot: Snapshot{ID: hex.EncodeToString(id[:]), Status: "running", StartedAt: time.Now().UTC()}, done: make(chan struct{}), cancel: cancel}
	m.records[r.snapshot.ID] = r
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		output, err := m.runner(childCtx, req, func(p Progress) {
			m.mu.Lock()
			r.snapshot.Progress = p
			m.mu.Unlock()
		})
		if childCtx.Err() != nil {
			err = childCtx.Err()
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		now := time.Now().UTC()
		r.snapshot.FinishedAt = &now
		r.snapshot.Status = "completed"
		if err != nil {
			r.snapshot.Status, r.snapshot.Code = "failed", "subagent_failed"
			var public interface{ PublicCode() string }
			if errors.As(err, &public) {
				r.snapshot.Code = public.PublicCode()
			}
			if errors.Is(err, context.Canceled) {
				r.snapshot.Status, r.snapshot.Code = "canceled", "canceled"
			}
			if errors.Is(err, context.DeadlineExceeded) {
				r.snapshot.Code = "deadline_exceeded"
			}
		} else {
			if len(output) > MaxResultBytes {
				n := MaxResultBytes
				for n > 0 && !utf8.RuneStart(output[n]) {
					n--
				}
				output = output[:n]
				r.snapshot.Truncated = true
			}
			r.snapshot.Output = output
		}
		close(r.done)
	}()
	return r.snapshot, nil
}

func (m *Manager) Status(ctx context.Context, id string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	snapshot := r.snapshot
	if snapshot.FinishedAt != nil {
		finished := *snapshot.FinishedAt
		snapshot.FinishedAt = &finished
	}
	return snapshot, nil
}

func (m *Manager) Wait(ctx context.Context, id string) (Snapshot, error) {
	m.mu.Lock()
	r, ok := m.records[id]
	m.mu.Unlock()
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-r.done:
		return m.Status(ctx, id)
	}
}

// Close cancels and joins children before the parent releases its turn permit.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for _, r := range m.records {
		r.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

type contextKey struct{}

func WithService(ctx context.Context, service Service) context.Context {
	return context.WithValue(ctx, contextKey{}, service)
}
func FromContext(ctx context.Context) Service {
	service, _ := ctx.Value(contextKey{}).(Service)
	return service
}
