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
	AgentID string `json:"agent_id,omitempty"`
	// Record and Recall are the two memory switches, kept independent on
	// purpose. Record decides whether this conversation is written to the
	// store at all; Recall decides whether prior context is retrieved into it.
	// Drawing on the full history while leaving no trace of the present
	// conversation is a valid combination, and so is the reverse.
	//
	// Neither is omitempty. Whether a conversation is being recorded is
	// exactly the kind of state that is bad to be wrong about in either
	// direction, so the field is always on the wire rather than absent when
	// false and inferred by whatever is reading it.
	Record bool `json:"record"`
	Recall bool `json:"recall"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// sessionColumns is the shared SELECT list, so the scan order below cannot
// drift from the query.
const sessionColumns = `id, client_id, title, backend, model, agent_id, record, recall, created_at, updated_at`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var sess Session
	err := row.Scan(&sess.ID, &sess.ClientID, &sess.Title, &sess.Backend, &sess.Model,
		&sess.AgentID, &sess.Record, &sess.Recall, &sess.CreatedAt, &sess.UpdatedAt)
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
	// A new conversation is on the record and recalling. Ephemeral is always
	// something the operator turns on, never a state a session arrives in.
	return &Session{
		ID: id, ClientID: clientID, Title: title,
		Backend: backend, Model: model, AgentID: agentID,
		Record: true, Recall: true, CreatedAt: now, UpdatedAt: now,
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

// DeleteSession removes a conversation and, through ON DELETE CASCADE, its
// messages and audit rows — the same reach as utilities.PruneSessions, which
// is the existing way conversations leave the database.
//
// The delete is scoped to the owning client and reports ErrNotFound when it
// matches nothing, so one client cannot discover, let alone delete, another's
// session. Scoping it in the statement rather than checking first also means
// there is no window between the check and the delete.
func (s *Store) DeleteSession(ctx context.Context, id string, clientID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE id = ? AND client_id = ?`, id, clientID)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSessionMemory flips a conversation's two memory switches.
//
// Turning `record` off mid-conversation deletes the turns already written for
// it, in the same transaction as the flag change. That is the whole point of
// the switch: a conversation the operator has just declared off the record
// must not keep a partial transcript of everything said before they reached
// for the toggle. Vectors derived from those messages go with them by
// ON DELETE CASCADE, and secure_delete (see Open) means the text is
// overwritten rather than merely unlinked.
//
// Turning it back on does not retroactively commit anything. Turns exchanged
// while off the record stay unrecorded — they were never written, so there is
// nothing to restore, and reconstructing them from the live session would
// record words the operator said in confidence.
//
// Scoped to the owning client, like every other session lookup here.
func (s *Store) SetSessionMemory(ctx context.Context, id string, clientID int64, record, recall bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set memory: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE sessions SET record = ?, recall = ?, updated_at = ? WHERE id = ? AND client_id = ?`,
		record, recall, time.Now().UTC(), id, clientID)
	if err != nil {
		return fmt.Errorf("set session memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set session memory: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}

	if !record {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, id); err != nil {
			return fmt.Errorf("purge transcript: %w", err)
		}
	}
	return tx.Commit()
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
//
// It returns the row ids of what it wrote, in the order given, which is what
// the retrieval indexer needs to queue them. They are returned rather than
// looked up afterwards because the caller has just committed them and a second
// query could race with anything else writing to the same session.
func (s *Store) AppendMessages(ctx context.Context, sessionID string, msgs ...llm.Message) ([]int64, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin append: %w", err)
	}
	defer tx.Rollback()

	var seq int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM messages WHERE session_id = ?`, sessionID).Scan(&seq)
	if err != nil {
		return nil, fmt.Errorf("next seq: %w", err)
	}

	ids := make([]int64, 0, len(msgs))
	now := time.Now().UTC()
	for _, m := range msgs {
		seq++
		var calls string
		if len(m.ToolCalls) > 0 {
			buf, err := json.Marshal(m.ToolCalls)
			if err != nil {
				return nil, fmt.Errorf("encode tool calls: %w", err)
			}
			calls = string(buf)
		}
		var thinking string
		if len(m.Thinking) > 0 {
			buf, err := json.Marshal(m.Thinking)
			if err != nil {
				return nil, fmt.Errorf("encode thinking: %w", err)
			}
			thinking = string(buf)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO messages (session_id, seq, role, content, tool_calls, tool_call_id, is_error,
			                       thinking, backend, model, token_count, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, seq, string(m.Role), m.Content, calls, m.ToolCallID, m.IsError, thinking,
			m.Backend, m.Model, m.TokenCount, now)
		if err != nil {
			return nil, fmt.Errorf("insert message: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("insert message: %w", err)
		}
		ids = append(ids, id)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		return nil, fmt.Errorf("touch session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// Messages returns a session's full transcript in order.
// TurnProgress summarises where a running turn has got to.
//
// It exists because the browser polls for progress every few seconds while a
// turn is in flight, and the obvious way to answer — read the transcript and
// look at the end of it — is the expensive way. Messages() returns every row
// with its content, its tool calls and its thinking blocks; on a long session
// against a reasoning model that is the largest object the server builds, and
// polling it re-reads, re-parses and re-serialises the whole conversation
// several times a minute to look at one row. The cost also grows with the
// conversation, so it is worst exactly when turns are longest.
//
// These four numbers are what the status line actually shows. They come from
// indexed counts and a single-row lookup that never touches content or
// thinking, so the answer stays the same size whether the session has ten
// messages or ten thousand.
type TurnProgress struct {
	// Count is the whole transcript's length, which the client uses to notice
	// that something changed since the last poll.
	Count int `json:"count"`
	// Steps is how many assistant messages belong to the turn in progress —
	// everything after the last user message.
	Steps int `json:"steps"`
	// LastRole is the role of the newest message: "assistant" mid-tool-call,
	// "tool" just after one returned, "user" before the model has replied.
	LastRole string `json:"last_role"`
	// Tools names the calls the newest assistant message asked for, when it
	// asked for any.
	Tools []string `json:"tools,omitempty"`
}

// TurnProgress reads the summary above for one session.
func (s *Store) TurnProgress(ctx context.Context, sessionID string) (TurnProgress, error) {
	var p TurnProgress
	// The count and the turn boundary come from one pass. MAX over a CASE is
	// null when the session has no user message yet, which is a new session
	// rather than an error.
	var lastUser sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(CASE WHEN role = 'user' THEN seq END)
		 FROM messages WHERE session_id = ?`, sessionID).Scan(&p.Count, &lastUser)
	if err != nil {
		return p, fmt.Errorf("turn progress: %w", err)
	}
	if p.Count == 0 {
		return p, nil
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		 WHERE session_id = ? AND role = 'assistant' AND seq > ?`,
		sessionID, lastUser.Int64).Scan(&p.Steps); err != nil {
		return p, fmt.Errorf("turn progress steps: %w", err)
	}

	// Only the newest row, and only the two columns the status needs — no
	// content and no thinking.
	var calls string
	if err := s.db.QueryRowContext(ctx,
		`SELECT role, tool_calls FROM messages
		 WHERE session_id = ? ORDER BY seq DESC LIMIT 1`,
		sessionID).Scan(&p.LastRole, &calls); err != nil {
		return p, fmt.Errorf("turn progress last message: %w", err)
	}
	if calls != "" {
		var parsed []tool.Call
		if err := json.Unmarshal([]byte(calls), &parsed); err != nil {
			// A malformed row should not take the status line down with it;
			// the roles and counts above are still worth returning.
			return p, nil
		}
		for _, c := range parsed {
			p.Tools = append(p.Tools, c.Name)
		}
	}
	return p, nil
}

func (s *Store) Messages(ctx context.Context, sessionID string) ([]llm.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, content, tool_calls, tool_call_id, is_error, thinking, backend, model, token_count
		 FROM messages WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var out []llm.Message
	for rows.Next() {
		var m llm.Message
		var role, calls, thinking string
		if err := rows.Scan(&role, &m.Content, &calls, &m.ToolCallID, &m.IsError, &thinking,
			&m.Backend, &m.Model, &m.TokenCount); err != nil {
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
