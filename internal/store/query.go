package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"rosetta/internal/identity"
)

func validLimit(limit int) bool { return limit >= 1 && limit <= 100 }

func (s *Store) Sessions(ctx context.Context, user, after string, limit int) (SessionPage, error) {
	page := SessionPage{Sessions: []Session{}}
	if !identity.ValidName(user) || !validLimit(limit) || len(after) > 64 {
		return page, fmt.Errorf("invalid session query")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.id,u.name,s.name,s.created_at,(SELECT COUNT(*) FROM turns t WHERE t.session_id=s.id) FROM sessions s JOIN users u ON u.id=s.user_id WHERE u.name=? AND s.id>? ORDER BY s.id LIMIT ?`, user, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(page.Sessions) == limit {
			page.NextAfter = page.Sessions[len(page.Sessions)-1].ID
			break
		}
		var session Session
		if err := rows.Scan(&session.ID, &session.User, &session.Name, &session.CreatedAt, &session.Turns); err != nil {
			return page, err
		}
		page.Sessions = append(page.Sessions, session)
	}
	return page, rows.Err()
}

func (s *Store) History(ctx context.Context, scope identity.Scope, after int64, limit int) (HistoryPage, error) {
	page := HistoryPage{Messages: []Message{}}
	if !identity.ValidName(scope.User) || !identity.ValidName(scope.Session) || !validLimit(limit) || after < 0 {
		return page, fmt.Errorf("invalid history query")
	}
	err := s.db.QueryRowContext(ctx, `SELECT s.id,u.name,s.name,s.created_at,(SELECT COUNT(*) FROM turns t WHERE t.session_id=s.id) FROM sessions s JOIN users u ON u.id=s.user_id WHERE u.name=? AND s.name=?`, scope.User, scope.Session).Scan(&page.Session.ID, &page.Session.User, &page.Session.Name, &page.Session.CreatedAt, &page.Session.Turns)
	if errors.Is(err, sql.ErrNoRows) {
		return page, ErrNotFound
	}
	if err != nil {
		return page, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.ordinal,m.role,m.content,m.tool_calls,m.tool_call_id,t.id,t.session_id,t.status,t.error,t.provider,t.started_at,t.finished_at FROM messages m JOIN turns t ON t.id=m.turn_id WHERE t.session_id=? AND m.id>? ORDER BY m.id LIMIT ?`, page.Session.ID, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	size := 1024 // Reserve room for the envelope, session metadata and next cursor.
	for rows.Next() {
		if len(page.Messages) == limit {
			page.NextAfter = page.Messages[len(page.Messages)-1].ID
			break
		}
		var message Message
		var calls string
		if err := rows.Scan(&message.ID, &message.Ordinal, &message.Role, &message.Content, &calls, &message.ToolCallID, &message.Turn.ID, &message.Turn.SessionID, &message.Turn.Status, &message.Turn.Error, &message.Turn.Provider, &message.Turn.StartedAt, &message.Turn.FinishedAt); err != nil {
			return page, err
		}
		if err := json.Unmarshal([]byte(calls), &message.ToolCalls); err != nil {
			return page, err
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			return page, err
		}
		if size+len(encoded)+1 > MaxPageBytes {
			if len(page.Messages) == 0 {
				return page, fmt.Errorf("stored message exceeds page budget")
			}
			page.NextAfter = page.Messages[len(page.Messages)-1].ID
			break
		}
		size += len(encoded) + 1
		page.Messages = append(page.Messages, message)
	}
	return page, rows.Err()
}
