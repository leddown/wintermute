package recall

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wintermute/internal/tool"
)

// Episodic memory: what was *done*, as distinct from what was said.
//
// Muninn already holds it — every proposed tool call, the decision the
// approval policy took on it, and the outcome — across every agent and every
// conversation. It is the record that survives when a transcript does not,
// which is why off-the-record conversations still write to it: the content of
// a private conversation is nobody's business, but a rename that happened on
// the operator's NAS did happen.
//
// This is exposed as a tool rather than folded into the automatic injection in
// search.go, and the distinction is worth stating. Semantic recall answers
// "what do I know about X" and belongs in front of every turn. Episodic recall
// answers "what did you actually do last Tuesday" — a question that is asked
// occasionally and explicitly, whose answer is bulky and structured, and which
// would spend the whole context budget on rows nobody asked for if it were
// injected every time. So the model fetches it when the question calls for it.
//
// It is read-only and scoped to the authenticated client, like every other
// read here.

// Activity is one recorded action.
type Activity struct {
	SessionID    string    `json:"session_id"`
	SessionTitle string    `json:"conversation,omitempty"`
	AgentID      string    `json:"agent,omitempty"`
	ToolName     string    `json:"tool"`
	Side         string    `json:"side"`
	Risk         string    `json:"risk"`
	Decision     string    `json:"decision"`
	Outcome      string    `json:"outcome,omitempty"`
	IsError      bool      `json:"failed,omitempty"`
	Input        string    `json:"input,omitempty"`
	At           time.Time `json:"at"`
}

// ActivityQuery filters the episodic record.
type ActivityQuery struct {
	ClientID int64
	// AgentID narrows to one agent. Empty means every agent the client owns,
	// which is the unscoped assistant's view of all activity.
	AgentID string
	// Tool, Decision and Since narrow the search; all are optional.
	Tool     string
	Decision string
	Since    time.Time
	Limit    int
}

// Activity reads the episodic record.
func (s *Store) Activity(ctx context.Context, q ActivityQuery) ([]Activity, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}

	clauses := []string{"sess.client_id = ?"}
	args := []any{q.ClientID}

	if q.AgentID != "" {
		clauses = append(clauses, "sess.agent_id = ?")
		args = append(args, q.AgentID)
	}
	if q.Tool != "" {
		clauses = append(clauses, "m.tool_name = ?")
		args = append(args, q.Tool)
	}
	if q.Decision != "" {
		clauses = append(clauses, "m.decision = ?")
		args = append(args, q.Decision)
	}
	if !q.Since.IsZero() {
		clauses = append(clauses, "m.created_at >= ?")
		args = append(args, q.Since)
	}
	args = append(args, q.Limit)

	rows, err := s.db.QueryContext(ctx,
		`SELECT m.session_id, sess.title, sess.agent_id, m.tool_name, m.side, m.risk,
		        m.decision, m.outcome, m.is_error, m.input, m.created_at
		 FROM muninn m
		 JOIN sessions sess ON sess.id = m.session_id
		 WHERE `+strings.Join(clauses, " AND ")+`
		 ORDER BY m.created_at DESC, m.id DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("recall: read activity: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.SessionID, &a.SessionTitle, &a.AgentID, &a.ToolName,
			&a.Side, &a.Risk, &a.Decision, &a.Outcome, &a.IsError, &a.Input, &a.At); err != nil {
			return nil, fmt.Errorf("recall: scan activity: %w", err)
		}
		// The audit trail stores full tool input and a generous slice of the
		// outcome. Handing all of it to a model would flood the turn, so it is
		// trimmed here rather than at the source, where it is evidence.
		a.Input = clip(a.Input, 200)
		a.Outcome = clip(a.Outcome, 400)
		out = append(out, a)
	}
	return out, rows.Err()
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ActivityTool exposes the episodic record to the model, scoped to one client.
//
// The scope is bound here, at registration, rather than taken from the tool's
// arguments. A model that could name the client whose activity it reads would
// be one prompt injection away from reading somebody else's, and the same rule
// already governs the knowledge tools: what a session may reach is decided by
// the session, not by a field the model fills in.
func ActivityTool(store *Store, clientID int64, agentID string) (tool.Definition, tool.Handler) {
	def := tool.Definition{
		Name: "recall_activity",
		Description: "Look up what has actually been done in past conversations: which tools were " +
			"called, whether the user approved them, and what the outcome was. Use this for questions " +
			"about past actions and their results, rather than guessing from memory of the discussion.",
		Risk: tool.RiskRead,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"tool": {"type": "string", "description": "Only this tool name, e.g. rename_file."},
				"decision": {"type": "string", "enum": ["auto", "approved", "denied", "blocked"],
					"description": "Only calls with this approval outcome."},
				"since_days": {"type": "integer", "description": "Only the last N days."},
				"limit": {"type": "integer", "description": "How many records to return (default 50)."}
			}
		}`),
	}

	handler := func(ctx context.Context, input json.RawMessage) (string, error) {
		var args struct {
			Tool      string `json:"tool"`
			Decision  string `json:"decision"`
			SinceDays int    `json:"since_days"`
			Limit     int    `json:"limit"`
		}
		if len(input) > 0 {
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("could not read the arguments: %w", err)
			}
		}

		q := ActivityQuery{
			ClientID: clientID,
			AgentID:  agentID,
			Tool:     strings.TrimSpace(args.Tool),
			Decision: strings.TrimSpace(args.Decision),
			Limit:    args.Limit,
		}
		if args.SinceDays > 0 {
			q.Since = time.Now().UTC().AddDate(0, 0, -args.SinceDays)
		}

		records, err := store.Activity(ctx, q)
		if err != nil {
			return "", err
		}
		if len(records) == 0 {
			return "No recorded activity matches that.", nil
		}
		buf, err := json.Marshal(records)
		if err != nil {
			return "", fmt.Errorf("could not encode the activity: %w", err)
		}
		return string(buf), nil
	}

	return def, handler
}
