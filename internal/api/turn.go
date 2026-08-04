package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"wintermute/internal/agent"
	"wintermute/internal/llm"
	"wintermute/internal/store"
	"wintermute/internal/tool"
)

// toolNamePattern matches what the Messages API accepts for a tool name. It
// also keeps a client from declaring a name that would collide confusingly
// with a server tool.
var toolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// clientToolInput is a tool the caller declares it can execute locally.
//
// The field set must stay compatible with tool.Definition's JSON form, since
// clients send exactly that; decoding rejects unknown fields.
type clientToolInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Risk        string          `json:"risk"`
	// Side is accepted but ignored: anything arriving here is client-side by
	// definition, and trusting the caller's claim would let a client register
	// a tool the server would then try to execute itself.
	Side string `json:"side"`
}

type postMessageRequest struct {
	Text        string            `json:"text"`
	ClientTools []clientToolInput `json:"client_tools"`
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var req postMessageRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	tools, err := validateClientTools(req.ClientTools)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if sess.Title == "" {
		if err := s.store.SetTitle(r.Context(), sess.ID, agent.Title(req.Text)); err != nil {
			s.log.Warn("set title failed", "session", sess.ID, "error", err)
		}
	}

	turn, err := s.agent.Advance(r.Context(), sess, tools, llm.UserMessage(req.Text))
	s.writeTurn(w, turn, err)
}

// toolResultInput is one executed (or refused) client-side tool call. The
// client reports what its approval policy decided; the server records that
// verdict in the audit trail alongside the outcome.
type toolResultInput struct {
	CallID   string          `json:"call_id"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
	Content  string          `json:"content"`
	IsError  bool            `json:"is_error"`
	Decision string          `json:"decision"`
	Risk     string          `json:"risk"`
}

type toolResultsRequest struct {
	Results     []toolResultInput `json:"results"`
	ClientTools []clientToolInput `json:"client_tools"`
}

func (s *Server) handleToolResults(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var req toolResultsRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Results) == 0 {
		writeError(w, http.StatusBadRequest, "results must not be empty")
		return
	}

	tools, err := validateClientTools(req.ClientTools)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	msgs := make([]llm.Message, 0, len(req.Results))
	for _, res := range req.Results {
		if res.CallID == "" {
			writeError(w, http.StatusBadRequest, "each result needs a call_id")
			return
		}
		decision, err := validateDecision(res.Decision)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		entry := store.AuditEntry{
			SessionID: sess.ID,
			CallID:    res.CallID,
			ToolName:  res.ToolName,
			Side:      tool.SideClient,
			Risk:      tool.Risk(res.Risk),
			Input:     string(res.Input),
			Decision:  decision,
			Outcome:   res.Content,
			IsError:   res.IsError,
		}
		if err := s.store.RecordTool(r.Context(), entry); err != nil {
			s.fail(w, "record audit", err)
			return
		}

		msgs = append(msgs, llm.ToolMessage(tool.Result{
			CallID:  res.CallID,
			Content: res.Content,
			IsError: res.IsError,
		}))
	}

	turn, err := s.agent.Advance(r.Context(), sess, tools, msgs...)
	s.writeTurn(w, turn, err)
}

func (s *Server) writeTurn(w http.ResponseWriter, turn *agent.Turn, err error) {
	switch {
	case errors.Is(err, agent.ErrTooManyIterations):
		writeError(w, http.StatusUnprocessableEntity,
			"the model exceeded its tool-call budget for this turn; rephrase or narrow the request")
	case err != nil:
		s.fail(w, "advance turn", err)
	default:
		writeJSON(w, http.StatusOK, turn)
	}
}

func validateClientTools(in []clientToolInput) ([]tool.Definition, error) {
	const maxClientTools = 64
	if len(in) > maxClientTools {
		return nil, fmt.Errorf("too many client tools (max %d)", maxClientTools)
	}

	seen := make(map[string]bool, len(in))
	out := make([]tool.Definition, 0, len(in))
	for _, t := range in {
		if !toolNamePattern.MatchString(t.Name) {
			return nil, fmt.Errorf("invalid tool name %q", t.Name)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("duplicate tool name %q", t.Name)
		}
		seen[t.Name] = true

		risk := tool.Risk(t.Risk)
		if !risk.Valid() {
			return nil, fmt.Errorf("tool %q: invalid risk %q", t.Name, t.Risk)
		}
		if len(t.Parameters) > 0 && !json.Valid(t.Parameters) {
			return nil, fmt.Errorf("tool %q: parameters is not valid JSON", t.Name)
		}

		out = append(out, tool.Definition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
			Risk:        risk,
			Side:        tool.SideClient,
		})
	}
	return out, nil
}

func validateDecision(d string) (string, error) {
	switch d {
	case store.DecisionAuto, store.DecisionApproved, store.DecisionDenied, store.DecisionBlocked:
		return d, nil
	case "":
		return "", errors.New("each result needs a decision")
	default:
		return "", fmt.Errorf("unknown decision %q", d)
	}
}
