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
	"regexp"
	"strings"
	"sync"
	"time"

	"rosetta/internal/agent"
	"rosetta/internal/protocol"
)

var sessionPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type Server struct {
	runtime *agent.Runtime
	logger  *slog.Logger
	mu      sync.Mutex
	active  map[string]bool
}

func New(runtime *agent.Runtime, logger *slog.Logger) *Server {
	return &Server{runtime: runtime, logger: logger, active: make(map[string]bool)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.Health{Status: "ready", Protocol: protocol.Version, Provider: s.runtime.ProviderName()})
	})
	mux.HandleFunc("POST /v1/chat", s.chat)
	return mux
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
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
	s.mu.Lock()
	if s.active[req.SessionID] {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "session_busy", "this session already has an active turn")
		return
	}
	if len(s.active) >= 4 {
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "capacity_exceeded", "maximum 4 active turns")
		return
	}
	s.active[req.SessionID] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, req.SessionID)
		s.mu.Unlock()
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
	err := s.runtime.Run(ctx, req, emit)
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
		s.logger.Info("turn ended", "run_id", runID, "session_id", req.SessionID, "reason", reason)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(protocol.Error{Code: code, Message: message})
}

// Serve owns the listener until all handlers exit. Canceling ctx propagates to
// every request before Shutdown waits for the handlers to release their permits.
func (s *Server) Serve(ctx context.Context, stateDir string) error {
	listener, cleanup, err := listenLocal(stateDir)
	if err != nil {
		return err
	}
	defer cleanup()
	server := &http.Server{
		Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10,
		BaseContext:    func(net.Listener) context.Context { return ctx },
	}
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	s.logger.Info("daemon ready", "socket", SocketPath(stateDir), "provider", s.runtime.ProviderName())
	select {
	case err := <-stopped:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			<-stopped
			return fmt.Errorf("daemon shutdown: %w", err)
		}
		<-stopped
		return nil
	}
}
