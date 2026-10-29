package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"rosetta/internal/llm"
)

// LoadContext uses the persisted current turn as the ownership and time boundary.
// It reads a recent suffix of completed turns, never the current input, failures
// or another session. Callers hold the daemon's per-session admission slot.
func (s *Store) LoadContext(ctx context.Context, currentID string) ([]llm.HistoryTurn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var sessionID string
	var before int64
	if err := tx.QueryRowContext(ctx, `SELECT t.session_id,m.id FROM turns t JOIN messages m ON m.turn_id=t.id AND m.ordinal=1 WHERE t.id=? AND t.status='running'`, currentID).Scan(&sessionID, &before); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.id FROM turns t JOIN messages m ON m.turn_id=t.id AND m.ordinal=1 WHERE t.session_id=? AND t.status='completed' AND m.id<? ORDER BY m.id DESC LIMIT ?`, sessionID, before, llm.MaxHistoryTurns)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var history []llm.HistoryTurn
	budget := llm.MaxHistoryBytes
	for _, id := range ids {
		// Bound allocations before retrieving text. JSON encoding can be larger;
		// the exact encoded size is checked below too.
		var rawBytes, count int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(CAST(content AS BLOB))+length(CAST(tool_calls AS BLOB))+length(CAST(tool_call_id AS BLOB))),0),COUNT(*) FROM messages WHERE turn_id=?`, id).Scan(&rawBytes, &count); err != nil {
			return nil, err
		}
		if rawBytes > budget {
			break // Keep a suffix; do not jump past an oversized recent turn.
		}
		if count < 2 || count > 32 {
			return nil, fmt.Errorf("invalid stored turn structure")
		}
		rows, err := tx.QueryContext(ctx, `SELECT ordinal,role,content,tool_calls,tool_call_id FROM messages WHERE turn_id=? ORDER BY ordinal`, id)
		if err != nil {
			return nil, err
		}
		turn := llm.HistoryTurn{}
		for rows.Next() {
			var m llm.Message
			var calls string
			var ordinal int
			if err := rows.Scan(&ordinal, &m.Role, &m.Content, &calls, &m.ToolCallID); err != nil {
				rows.Close()
				return nil, err
			}
			if ordinal != len(turn.Messages)+1 || json.Unmarshal([]byte(calls), &m.ToolCalls) != nil {
				rows.Close()
				return nil, fmt.Errorf("invalid stored message")
			}
			turn.Messages = append(turn.Messages, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if !llm.ValidHistoryTurn(turn) {
			return nil, fmt.Errorf("invalid stored tool pairing")
		}
		encoded, err := json.Marshal(turn.Messages)
		if err != nil {
			return nil, err
		}
		if len(encoded) > budget {
			break
		}
		budget -= len(encoded)
		history = append(history, turn)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	slices.Reverse(history) // Model sees chronological order, not reverse lookup order.
	return history, nil
}
