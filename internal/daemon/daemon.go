// Package daemon handles admission, local connections and process lifetime.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"rosetta/internal/agent"
	"rosetta/internal/protocol"
	"rosetta/internal/state"
)

var sessionPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type Daemon struct {
	runtime  *agent.Runtime
	logger   *slog.Logger
	mu       sync.Mutex
	active   map[string]bool
	health   protocol.Health
	stopping bool
	stop     chan struct{}
}

func New(runtime *agent.Runtime, logger *slog.Logger) *Daemon {
	return &Daemon{runtime: runtime, logger: logger, active: make(map[string]bool), stop: make(chan struct{})}
}

func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		d.mu.Lock()
		health := d.health
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(health)
	})
	mux.HandleFunc("POST /v1/chat", d.chat)
	mux.HandleFunc("POST /v1/shutdown", d.shutdown)
	return mux
}

func (d *Daemon) chat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req protocol.ChatRequest
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "expected a JSON chat request (maximum 64 KiB)")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "expected exactly one JSON object")
		return
	}
	if !sessionPattern.MatchString(req.SessionID) || strings.TrimSpace(req.Message) == "" || len(req.Message) > 16<<10 {
		writeError(w, http.StatusBadRequest, "invalid_request", "session_id: 1-64 letters/digits/_/-; message: nonblank, maximum 16 KiB")
		return
	}
	// Hold the mutex only while changing bookkeeping, never during model calls.
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "daemon_stopping", "daemon is stopping")
		return
	}
	if d.active[req.SessionID] {
		d.mu.Unlock()
		writeError(w, http.StatusConflict, "session_busy", "this session already has an active turn")
		return
	}
	if len(d.active) >= 4 {
		d.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "capacity_exceeded", "maximum 4 active turns")
		return
	}
	d.active[req.SessionID] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.active, req.SessionID)
		d.mu.Unlock()
	}()

	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot allocate run ID")
		return
	}
	runID := hex.EncodeToString(id[:])
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	controller := http.NewResponseController(w)
	encoder := json.NewEncoder(w)
	seq := 0
	emit := func(event protocol.Event) error {
		// Bound slow-reader writes instead of buffering an unbounded event queue.
		if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		seq++
		event.Version, event.RunID, event.SessionID, event.Seq = protocol.Version, runID, req.SessionID, seq
		if err := encoder.Encode(event); err != nil {
			return err
		}
		return controller.Flush()
	}
	err := d.runtime.Run(ctx, req, emit)
	if err != nil {
		// Do not expose raw provider errors (which may contain credentials).
		reason := "provider_error"
		var publicError interface{ PublicCode() string }
		if errors.As(err, &publicError) {
			reason = publicError.PublicCode()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "deadline_exceeded"
		} else if errors.Is(err, context.Canceled) {
			reason = "canceled"
		}
		_ = emit(protocol.Event{Type: protocol.Failed, Error: reason})
		d.logger.Info("turn ended", "run_id", runID, "session_id", req.SessionID, "reason", reason)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(protocol.Error{Code: code, Message: message})
}

// shutdown only signals Serve; waiting for HTTP shutdown inside a handler deadlocks.
func (d *Daemon) shutdown(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req protocol.ShutdownRequest
	if err := decoder.Decode(&req); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "expected one shutdown request")
		return
	}
	d.mu.Lock()
	if req.InstanceID == "" || req.InstanceID != d.health.InstanceID {
		d.mu.Unlock()
		writeError(w, http.StatusConflict, "instance_mismatch", "daemon instance has changed; check status again")
		return
	}
	d.markStoppingLocked()
	health := d.health
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(health)
}

func (d *Daemon) markStoppingLocked() {
	if !d.stopping {
		d.stopping = true
		d.health.Status = "stopping"
		close(d.stop)
	}
}

// Serve is called once per Daemon. It owns the instance metadata and listener lock
// until shutdown finishes. The optional ready callback reports startup to the parent.
func (d *Daemon) Serve(parent context.Context, paths state.Paths, ready func(protocol.Health) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	listener, cleanup, err := listenLocal(paths.Run)
	if err != nil {
		return err
	}
	defer cleanup()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	health := protocol.Health{
		Status: "ready", Protocol: protocol.Version, Provider: d.runtime.ProviderName(),
		Agent: paths.Agent, InstanceID: hex.EncodeToString(id[:]), PID: os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := state.WriteJSON(paths.Instance, health); err != nil {
		return err
	}
	defer os.Remove(paths.Instance)
	d.mu.Lock()
	d.health = health
	d.mu.Unlock()
	server := &http.Server{
		Handler: d.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10,
		BaseContext:    func(net.Listener) context.Context { return ctx },
	}
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	var startupErr error
	if ready != nil {
		startupErr = ready(health)
	}
	if startupErr != nil {
		cancel()
	}
	d.logger.Info("daemon ready", "agent", paths.Agent, "instance_id", health.InstanceID, "socket", paths.Socket, "provider", health.Provider)
	select {
	case err := <-stopped:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	case <-d.stop:
	}
	d.mu.Lock()
	d.markStoppingLocked()
	d.mu.Unlock()
	cancel() // Cancel active model/tool calls before waiting for handlers.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		<-stopped
		return fmt.Errorf("daemon shutdown: %w", err)
	}
	<-stopped
	d.logger.Info("daemon stopped", "agent", paths.Agent, "instance_id", health.InstanceID)
	return startupErr
}
