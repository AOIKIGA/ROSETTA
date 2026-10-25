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
	"strings"
	"sync"
	"time"

	"rosetta/internal/agent"
	"rosetta/internal/conversation"
	"rosetta/internal/identity"
	"rosetta/internal/protocol"
	"rosetta/internal/state"
	"rosetta/internal/store"
)

type Daemon struct {
	runtime  *agent.Runtime
	logger   *slog.Logger
	mu       sync.Mutex
	active   map[identity.Scope]bool
	store    *store.Store
	health   protocol.Health
	stopping bool
	stop     chan struct{}
}

func New(runtime *agent.Runtime, logger *slog.Logger) *Daemon {
	return &Daemon{runtime: runtime, logger: logger, active: make(map[identity.Scope]bool), stop: make(chan struct{})}
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
	mux.HandleFunc("GET /v1/sessions", d.sessions)
	mux.HandleFunc("GET /v1/history", d.history)
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
	scope, err := identity.Resolve(req.User, req.Session)
	if err != nil || strings.TrimSpace(req.Message) == "" || len(req.Message) > 16<<10 {
		writeError(w, http.StatusBadRequest, "invalid_request", "user/session: 1-64 ASCII letters/digits/_/-; message: nonblank, maximum 16 KiB")
		return
	}
	req.User, req.Session = scope.User, scope.Session
	// Hold the mutex only while changing bookkeeping, never during model calls.
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "daemon_stopping", "daemon is stopping")
		return
	}
	if d.active[scope] {
		d.mu.Unlock()
		writeError(w, http.StatusConflict, "session_busy", "this session already has an active turn")
		return
	}
	if len(d.active) >= 4 {
		d.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "capacity_exceeded", "maximum 4 active turns")
		return
	}
	d.active[scope] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.active, scope)
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
	if d.store == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "storage is not ready")
		return
	}
	turn, err := d.store.BeginTurn(ctx, scope, runID, req.Message, d.runtime.ProviderName())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error", "cannot save this request")
		return
	}
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
		event.Version, event.RunID, event.SessionID, event.Seq = protocol.Version, runID, turn.SessionID, seq
		event.User, event.Session = scope.User, scope.Session
		if err := encoder.Encode(event); err != nil {
			return err
		}
		return controller.Flush()
	}
	err = (conversation.Service{Runtime: d.runtime, Store: d.store}).Run(ctx, turn, req, emit)
	if err != nil {
		reason := conversation.ErrorCode(err)
		if errors.Is(err, conversation.ErrDelivery) {
			d.logger.Info("completed turn not delivered", "run_id", runID)
			return
		}
		_ = emit(protocol.Event{Type: protocol.Failed, Error: reason})
		d.logger.Info("turn ended", "run_id", runID, "user", scope.User, "session", scope.Session, "reason", reason)
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
	initCtx, initCancel := context.WithTimeout(ctx, 5*time.Second)
	db, err := store.Open(initCtx, paths.Database, paths.Agent)
	initCancel()
	if err != nil {
		return fmt.Errorf("open agent database: %w", err)
	}
	defer db.Close()
	d.store = db
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
