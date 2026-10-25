package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	_ "modernc.org/sqlite"
	"rosetta/internal/identity"
	"rosetta/internal/llm"
	"rosetta/internal/state"
)

type Store struct{ db *sql.DB }

// Open must be called while holding the agent's daemon lock. Only that owner
// may run migrations and mark unfinished turns interrupted.
func Open(ctx context.Context, path, agent string) (*Store, error) {
	f, err := state.OpenPrivateFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		f, err := state.OpenPrivateFile(path+suffix, os.O_RDWR)
		if err == nil {
			_ = f.Close()
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	// DSN pragmas apply on every connection, including a replacement connection.
	db, err := sql.Open("sqlite", uri+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(3000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.initialize(ctx, agent); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initialize(ctx context.Context, agent string) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != 1 {
		return fmt.Errorf("unsupported database schema version %d", version)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES('agent',?)", agent); err != nil {
			return err
		}
	}
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='agent'").Scan(&owner); err != nil {
		return err
	}
	if owner != agent {
		return fmt.Errorf("database belongs to a different agent")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE turns SET status='interrupted', error='daemon_interrupted', finished_at=? WHERE status='running'", now()); err != nil {
		return err
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE metadata(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE users(id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL);
CREATE TABLE sessions(id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), name TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(user_id,name));
CREATE TABLE turns(id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id), status TEXT NOT NULL CHECK(status IN ('running','completed','failed','canceled','interrupted')), error TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '');
CREATE INDEX turns_session ON turns(session_id);
CREATE TABLE messages(id INTEGER PRIMARY KEY AUTOINCREMENT, turn_id TEXT NOT NULL REFERENCES turns(id), ordinal INTEGER NOT NULL, role TEXT NOT NULL CHECK(role IN ('user','assistant','tool')), content TEXT NOT NULL, tool_calls TEXT NOT NULL DEFAULT '[]', tool_call_id TEXT NOT NULL DEFAULT '', UNIQUE(turn_id,ordinal));
PRAGMA user_version=1;
`

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Store) BeginTurn(ctx context.Context, scope identity.Scope, runID, message, provider string) (Turn, error) {
	if !identity.ValidName(scope.User) || !identity.ValidName(scope.Session) {
		return Turn{}, fmt.Errorf("invalid scope")
	}
	userID, err := newID()
	if err != nil {
		return Turn{}, err
	}
	sessionID, err := newID()
	if err != nil {
		return Turn{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Turn{}, err
	}
	defer tx.Rollback()
	stamp := now()
	if _, err = tx.ExecContext(ctx, "INSERT INTO users(id,name,created_at) VALUES(?,?,?) ON CONFLICT(name) DO NOTHING", userID, scope.User, stamp); err != nil {
		return Turn{}, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE name=?", scope.User).Scan(&userID); err != nil {
		return Turn{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO sessions(id,user_id,name,created_at) VALUES(?,?,?,?) ON CONFLICT(user_id,name) DO NOTHING", sessionID, userID, scope.Session, stamp); err != nil {
		return Turn{}, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT id FROM sessions WHERE user_id=? AND name=?", userID, scope.Session).Scan(&sessionID); err != nil {
		return Turn{}, err
	}
	turn := Turn{ID: runID, SessionID: sessionID, Status: "running", Provider: provider, StartedAt: stamp}
	if _, err = tx.ExecContext(ctx, "INSERT INTO turns(id,session_id,status,provider,started_at) VALUES(?,?,'running',?,?)", runID, sessionID, provider, stamp); err != nil {
		return Turn{}, err
	}
	if err = appendMessage(ctx, tx, runID, llm.Message{Role: "user", Content: message}); err != nil {
		return Turn{}, err
	}
	return turn, tx.Commit()
}

func appendMessage(ctx context.Context, tx *sql.Tx, runID string, message llm.Message) error {
	calls, err := json.Marshal(message.ToolCalls)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(turn_id,ordinal,role,content,tool_calls,tool_call_id) SELECT ?,COALESCE(MAX(ordinal),0)+1,?,?,?,? FROM messages WHERE turn_id=?`, runID, message.Role, message.Content, string(calls), message.ToolCallID, runID)
	return err
}

func running(ctx context.Context, tx *sql.Tx, id string) error {
	var status string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM turns WHERE id=?", id).Scan(&status); err != nil {
		return err
	}
	if status != "running" {
		return ErrTurnClosed
	}
	return nil
}

func (s *Store) AppendMessage(ctx context.Context, runID string, message llm.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = running(ctx, tx, runID); err != nil {
		return err
	}
	if err = appendMessage(ctx, tx, runID, message); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishTurn commits the final assistant message and completed status together.
func (s *Store) FinishTurn(ctx context.Context, runID, status, code string, final *llm.Message) error {
	if status != "completed" && status != "failed" && status != "canceled" {
		return fmt.Errorf("invalid final status")
	}
	if status == "completed" && (final == nil || final.Role != "assistant" || len(final.ToolCalls) > 0) {
		return fmt.Errorf("completed turn requires a final answer")
	}
	if status != "completed" && final != nil {
		return fmt.Errorf("failed turn cannot have a final answer")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = running(ctx, tx, runID); err != nil {
		return err
	}
	if final != nil {
		if err = appendMessage(ctx, tx, runID, *final); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE turns SET status=?,error=?,finished_at=? WHERE id=?", status, code, now(), runID); err != nil {
		return err
	}
	return tx.Commit()
}
