package store

import (
	"context"
	"fmt"
	"time"

	"wintermute/internal/tool"
)

// Approval decisions recorded in the audit trail.
const (
	// DecisionAuto means the policy allowed the call without asking a human.
	DecisionAuto = "auto"
	// DecisionApproved means a human approved this specific call.
	DecisionApproved = "approved"
	// DecisionDenied means a human refused it.
	DecisionDenied = "denied"
	// DecisionBlocked means policy refused it before a human saw it.
	DecisionBlocked = "blocked"
)

// AuditEntry is one durable record of a proposed tool call and its fate.
type AuditEntry struct {
	ID        int64         `json:"id"`
	SessionID string        `json:"session_id"`
	CallID    string        `json:"call_id"`
	ToolName  string        `json:"tool_name"`
	Side      tool.Side     `json:"side"`
	Risk      tool.Risk     `json:"risk"`
	Input     string        `json:"input"`
	Decision  string        `json:"decision"`
	Outcome   string        `json:"outcome"`
	IsError   bool          `json:"is_error"`
	CreatedAt time.Time     `json:"created_at"`
	Duration  time.Duration `json:"-"`
}

// RecordTool appends an audit entry. Outcomes are truncated: the audit trail
// answers "what was done", not "what did it return in full".
func (s *Store) RecordTool(ctx context.Context, e AuditEntry) error {
	const maxOutcome = 4096
	if len(e.Outcome) > maxOutcome {
		e.Outcome = e.Outcome[:maxOutcome] + "... (truncated)"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO muninn (session_id, call_id, tool_name, side, risk, input, decision, outcome, is_error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.SessionID, e.CallID, e.ToolName, string(e.Side), string(e.Risk),
		e.Input, e.Decision, e.Outcome, e.IsError, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("record tool audit: %w", err)
	}
	return nil
}

// AuditForSession returns a session's audit trail, newest first.
func (s *Store) AuditForSession(ctx context.Context, sessionID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, call_id, tool_name, side, risk, input, decision, outcome, is_error, created_at
		 FROM muninn WHERE session_id = ? ORDER BY id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var side, risk string
		if err := rows.Scan(&e.ID, &e.SessionID, &e.CallID, &e.ToolName, &side, &risk,
			&e.Input, &e.Decision, &e.Outcome, &e.IsError, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan audit: %w", err)
		}
		e.Side, e.Risk = tool.Side(side), tool.Risk(risk)
		out = append(out, e)
	}
	return out, rows.Err()
}
