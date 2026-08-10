// Package agent runs the assistant's turn loop.
//
// The loop is deliberately split across two processes. Tools the server owns
// (metadata lookups, anything network-bound) run here. Tools the client owns
// (reading and renaming files on a NAS share) are handed back to the caller as
// pending calls; the client applies its approval policy, executes them, and
// posts the results, which resumes the same loop. The server therefore never
// touches the user's filesystem and never decides that an action was approved.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"wintermute/internal/llm"
	"wintermute/internal/store"
	"wintermute/internal/tool"
)

// Status describes where a turn stopped.
type Status string

const (
	// StatusComplete means the model produced a final answer.
	StatusComplete Status = "complete"
	// StatusAwaitingClient means the model asked for client-side tools; the
	// caller must execute them and post the results back.
	StatusAwaitingClient Status = "awaiting_client"
)

// PendingCall is a client-side tool call awaiting approval and execution.
type PendingCall struct {
	tool.Call
	Risk        tool.Risk `json:"risk"`
	Description string    `json:"description"`
}

// Turn is the outcome of advancing a session.
type Turn struct {
	SessionID string        `json:"session_id"`
	Status    Status        `json:"status"`
	Reply     string        `json:"reply,omitempty"`
	Pending   []PendingCall `json:"pending_calls,omitempty"`
	Usage     llm.Usage     `json:"usage"`

	// Backend and Model record what actually served the turn. They are worth
	// reporting because they are not always what the session asked for: a
	// local backend that fails is retried against the configured fallback.
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	// FellBackFrom is set when the turn left the intended backend, so the UI
	// can say so rather than quietly answering from somewhere else.
	FellBackFrom   string `json:"fell_back_from,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`
}

// Agent wires the model, the transcript store and the server-side tools.
type Agent struct {
	router        *llm.Router
	pool          *llm.Pool
	store         *store.Store
	serverTools   *tool.Registry
	log           *slog.Logger
	maxIterations int
	system        string
}

// New builds an Agent. serverTools holds only tools the server executes;
// client-declared tools are layered on per session.
//
// pool may be nil, in which case no batch tool is offered — the model is never
// shown a way to fan work out that the deployment has not configured.
func New(router *llm.Router, pool *llm.Pool, s *store.Store, serverTools *tool.Registry, log *slog.Logger, maxIterations int) *Agent {
	return &Agent{
		router:        router,
		pool:          pool,
		store:         s,
		serverTools:   serverTools,
		log:           log,
		maxIterations: maxIterations,
		system:        SystemPrompt,
	}
}

// Router exposes the router so the API can report available backends.
func (a *Agent) Router() *llm.Router { return a.router }

// ErrTooManyIterations is returned when a turn exceeds the tool-call budget.
var ErrTooManyIterations = errors.New("tool iteration budget exhausted")

// Advance appends the given messages to the session transcript and runs the
// loop until the model answers or asks for a client-side tool.
//
// clientTools are the tools the calling client declared it can execute. A
// browser passes none, and the model simply won't see filesystem tools.
func (a *Agent) Advance(ctx context.Context, sess *store.Session, clientTools []tool.Definition, incoming ...llm.Message) (*Turn, error) {
	if err := a.store.AppendMessages(ctx, sess.ID, incoming...); err != nil {
		return nil, err
	}

	registry, err := a.registryFor(sess, clientTools)
	if err != nil {
		return nil, err
	}
	defs := registry.Definitions()

	turn := &Turn{SessionID: sess.ID}
	for i := 0; i < a.maxIterations; i++ {
		history, err := a.store.Messages(ctx, sess.ID)
		if err != nil {
			return nil, err
		}

		res, err := a.router.Complete(ctx, sess.Backend, llm.Request{
			System:   a.system,
			Messages: history,
			Tools:    defs,
			Model:    sess.Model,
		})
		if err != nil {
			return nil, fmt.Errorf("completion: %w", err)
		}
		resp := res.Response

		turn.Backend = res.Backend
		turn.Model = res.Model
		if res.FellBackFrom != "" {
			turn.FellBackFrom = res.FellBackFrom
			turn.FallbackReason = res.FallbackReason
		}
		turn.Usage.PromptTokens += resp.Usage.PromptTokens
		turn.Usage.CompletionTokens += resp.Usage.CompletionTokens
		turn.Usage.TotalTokens += resp.Usage.TotalTokens

		if err := a.store.AppendMessages(ctx, sess.ID, resp.Message); err != nil {
			return nil, err
		}

		if len(resp.Message.ToolCalls) == 0 {
			turn.Status = StatusComplete
			turn.Reply = resp.Message.Content
			return turn, nil
		}

		serverCalls, clientCalls, unknown := partition(registry, resp.Message.ToolCalls)

		// Unknown names happen when a small model hallucinates a tool. Feed
		// the error back rather than failing the turn — it usually recovers.
		for _, call := range unknown {
			res := tool.Errorf(call.ID, "unknown tool %q; use one of the tools provided", call.Name)
			if err := a.store.AppendMessages(ctx, sess.ID, llm.ToolMessage(res)); err != nil {
				return nil, err
			}
		}

		for _, call := range serverCalls {
			res := a.runServerTool(ctx, registry, sess, call, call.ID)
			if err := a.store.AppendMessages(ctx, sess.ID, llm.ToolMessage(res)); err != nil {
				return nil, err
			}
		}

		if len(clientCalls) > 0 {
			turn.Status = StatusAwaitingClient
			turn.Reply = resp.Message.Content
			turn.Pending = a.describe(registry, clientCalls)
			return turn, nil
		}
	}

	return nil, ErrTooManyIterations
}

// registryFor layers a client's declared tools over the server's own.
//
// The batch tool is added here rather than at startup because it has to be
// bound to a session: that is what puts each worker's tool calls in the right
// audit trail.
func (a *Agent) registryFor(sess *store.Session, clientTools []tool.Definition) (*tool.Registry, error) {
	registry := a.serverTools.Clone()
	if a.pool != nil {
		if err := registry.Register(batchDefinition(a.pool), a.batchHandler(sess)); err != nil {
			return nil, fmt.Errorf("batch tool: %w", err)
		}
	}
	for _, def := range clientTools {
		if err := registry.RegisterClient(def); err != nil {
			return nil, fmt.Errorf("client tool %q: %w", def.Name, err)
		}
	}
	return registry, nil
}

// runServerTool executes one server-side tool and records it in the audit
// trail. Server tools are read-only by construction, so they are auto-approved.
//
// auditID is the identifier written to the audit trail, which is the call's own
// id in the main loop but is namespaced per item inside a batch, where many
// concurrent workers hand back the same stock id.
func (a *Agent) runServerTool(ctx context.Context, registry *tool.Registry, sess *store.Session, call tool.Call, auditID string) tool.Result {
	def, _ := registry.Definition(call.Name)
	handler, ok := registry.Handler(call.Name)
	if !ok {
		return tool.Errorf(call.ID, "tool %q has no server-side handler", call.Name)
	}

	out, err := handler(ctx, call.Input)
	res := tool.Result{CallID: call.ID, Content: out}
	if err != nil {
		a.log.Warn("server tool failed", "tool", call.Name, "session", sess.ID, "error", err)
		res = tool.Errorf(call.ID, "tool %q failed: %v", call.Name, err)
	}

	entry := store.AuditEntry{
		SessionID: sess.ID,
		CallID:    auditID,
		ToolName:  call.Name,
		Side:      tool.SideServer,
		Risk:      def.Risk,
		Input:     string(call.Input),
		Decision:  store.DecisionAuto,
		Outcome:   res.Content,
		IsError:   res.IsError,
	}
	if err := a.store.RecordTool(ctx, entry); err != nil {
		a.log.Error("audit write failed", "tool", call.Name, "session", sess.ID, "error", err)
	}
	return res
}

func (a *Agent) describe(registry *tool.Registry, calls []tool.Call) []PendingCall {
	out := make([]PendingCall, 0, len(calls))
	for _, c := range calls {
		def, _ := registry.Definition(c.Name)
		out = append(out, PendingCall{Call: c, Risk: def.Risk, Description: def.Description})
	}
	return out
}

// partition splits calls by which side owns them.
func partition(registry *tool.Registry, calls []tool.Call) (server, client, unknown []tool.Call) {
	for _, c := range calls {
		def, ok := registry.Definition(c.Name)
		switch {
		case !ok:
			unknown = append(unknown, c)
		case def.Side == tool.SideClient:
			client = append(client, c)
		default:
			server = append(server, c)
		}
	}
	return server, client, unknown
}

// Title derives a short session title from the first user message.
func Title(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	const max = 60
	if len(text) <= max {
		return text
	}
	return text[:max] + "..."
}
