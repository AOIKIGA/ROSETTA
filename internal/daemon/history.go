package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"rosetta/internal/identity"
	"rosetta/internal/store"
)

func queryScope(r *http.Request) (identity.Scope, int, error) {
	scope, err := identity.Resolve(r.URL.Query().Get("user"), r.URL.Query().Get("session"))
	if err != nil {
		return scope, 0, err
	}
	limit := 20
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
	}
	if err != nil || limit < 1 || limit > 100 {
		return scope, 0, errors.New("limit must be 1-100")
	}
	return scope, limit, nil
}

func (d *Daemon) sessions(w http.ResponseWriter, r *http.Request) {
	scope, limit, err := queryScope(r)
	after := r.URL.Query().Get("after")
	if err != nil || len(after) > 64 {
		writeError(w, 400, "invalid_request", "invalid user, session, cursor or limit (1-100)")
		return
	}
	if d.store == nil {
		writeError(w, 503, "storage_unavailable", "storage is not ready")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	page, err := d.store.Sessions(ctx, scope.User, after, limit)
	if err != nil {
		writeError(w, 500, "storage_error", "cannot read sessions")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = json.NewEncoder(w).Encode(page)
}

func (d *Daemon) history(w http.ResponseWriter, r *http.Request) {
	scope, limit, err := queryScope(r)
	if err != nil {
		writeError(w, 400, "invalid_request", "invalid user, session or limit (1-100)")
		return
	}
	after := int64(0)
	if value := r.URL.Query().Get("after"); value != "" {
		after, err = strconv.ParseInt(value, 10, 64)
	}
	if err != nil || after < 0 {
		writeError(w, 400, "invalid_request", "history cursor must be a nonnegative integer")
		return
	}
	if d.store == nil {
		writeError(w, 503, "storage_unavailable", "storage is not ready")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	page, err := d.store.History(ctx, scope, after, limit)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "session_not_found", "this user has no session with that name")
		return
	}
	if err != nil {
		writeError(w, 500, "storage_error", "cannot read session history")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = json.NewEncoder(w).Encode(page)
}
