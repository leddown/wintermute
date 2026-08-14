package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"wintermute/internal/llm"
	"wintermute/internal/tool"
)

// Session is one conversation, owned by exactly one client.
type Session struct {
	ID       string `json:"id"`
	ClientID int64  `json:"-"`
	Title    string `json:"title"`
	// Backend and Model pin which model serves this conversation. Empty means
	// the server's configured default. They are per session rather than global
	// so a user can keep a local model for routine work and open a separate
	// conversation against a cloud model when they want one.
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	// AgentID names the agent profile this conversation belongs to, which
	// decides the documents and external sources it may reach. Empty is the
	// unscoped assistant — what every session was before agents existed, and
	// what a session keeps working as if its agent is later deleted.
	AgentID   string    `json:"agent_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// sessionColumns is the shared SELECT list, so the scan order below cannot
// drift from the query.
const sessionColumns = `id, client_id, title, backend, model, agent_id, created_at, updated_at`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var sess Session
	err := row.Scan(&sess.ID, &sess.ClientID, &sess.Title, &sess.Backend, &sess.Model,
		&sess.AgentID, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// CreateSession starts a new conversation for a client. backend, model and
// agentID may be empty, meaning the server default and the unscoped assistant.
func (s *Store) CreateSession(ctx context.Context, clientID int64, title, backend, model, agentID string) (*Session, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, client_id, title, backend, model, agent_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, clientID, title, backend, model, agentID, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return &Session{
		ID: id, ClientID: clientID, Title: title,
		Backend: backend, Model: model, AgentID: agentID, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// SetSessionModel repoints an existing conversation at another backend/model.
// The transcript is kept: switching models mid-conversation is a supported and
// occasionally very useful thing to do, e.g. escalating a stuck local turn.
func (s *Store) SetSessionModel(ctx context.Context, id, backend, model string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET backend = ?, model = ?, updated_at = ? WHERE id = ?`,
		backend, model, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("set session model: %w", err)
	}
	return nil
}

// Session fetches a session scoped to its owning client. Callers pass the
// authenticated client ID; a session belonging to another client reports
// ErrNotFound rather than a permission error, so the API leaks nothing about
// sessions the caller cannot see.
func (s *Store) Session(ctx context.Context, id string, clientID int64) (*Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ? AND client_id = ?`,
		id, clientID)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup session: %w", err)
	}
	return sess, nil
}

// ListSessions returns a client's sessions, most recently updated first.
func (s *Store) ListSessions(ctx context.Context, clientID int64, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE client_id = ? ORDER BY updated_at DESC LIMIT ?`, clientID, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// SetTitle names a session. The first user message is used when it is empty.
func (s *Store) SetTitle(ctx context.Context, id, title string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`,
		title, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("set title: %w", err)
	}
	return nil
}

// AppendMessages adds messages to a transcript in order, within one
// transaction so a partially written turn can never be replayed to the model.
func (s *Store) AppendMessages(ctx context.Context, sessionID string, msgs ...llm.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append: %w", err)
	}
	defer tx.Rollback()

	var seq int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM messages WHERE session_id = ?`, sessionID).Scan(&seq)
	if err != nil {
		return fmt.Errorf("next seq: %w", err)
	}

	now := time.Now().UTC()
	for _, m := range msgs {
		seq++
		var calls string
		if len(m.ToolCalls) > 0 {
			buf, err := json.Marshal(m.ToolCalls)
			if err != nil {
				return fmt.Errorf("encode tool calls: %w", err)
			}
			calls = string(buf)
		}
		var thinking string
		if len(m.Thinking) > 0 {
			buf, err := json.Marshal(m.Thinking)
			if err != nil {
				return fmt.Errorf("encode thinking: %w", err)
			}
			thinking = string(buf)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO messages (session_id, seq, role, content, tool_calls, tool_call_id, is_error, thinking, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, seq, string(m.Role), m.Content, calls, m.ToolCallID, m.IsError, thinking, now)
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return tx.Commit()
}

// Messages returns a session's full transcript in order.
func (s *Store) Messages(ctx context.Context, sessionID string) ([]llm.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, content, tool_calls, tool_call_id, is_error, thinking FROM messages
		 WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var out []llm.Message
	for rows.Next() {
		var m llm.Message
		var role, calls, thinking string
		if err := rows.Scan(&role, &m.Content, &calls, &m.ToolCallID, &m.IsError, &thinking); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Role = llm.Role(role)
		if calls != "" {
			var parsed []tool.Call
			if err := json.Unmarshal([]byte(calls), &parsed); err != nil {
				return nil, fmt.Errorf("decode tool calls: %w", err)
			}
			m.ToolCalls = parsed
		}
		if thinking != "" {
			var parsed []json.RawMessage
			if err := json.Unmarshal([]byte(thinking), &parsed); err != nil {
				return nil, fmt.Errorf("decode thinking: %w", err)
			}
			m.Thinking = parsed
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
