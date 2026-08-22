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
	// scope, when set, layers the session's agent profile over the base tool
	// set and system prompt. It is optional so the loop still runs — unscoped,
	// as it did before agents existed — in a deployment that has none.
	scope Scoper
	// ephemeral holds the transcripts of conversations that are not being
	// written down. See transcript.go.
	ephemeral *Ephemeral
}

// Scoper narrows a turn to the profile the session belongs to: which knowledge
// tools it may use, and what to add to the system prompt.
//
// It is an interface so the agent loop does not depend on the knowledge, grc
// and websearch packages, all of which would otherwise have to be imported
// here purely to be passed through. What the loop needs to know is only "given
// this session, what may it reach"; internal/app answers that.
type Scoper interface {
	// Scope registers the session's permitted knowledge tools onto registry and
	// returns any prompt text to append to the base system prompt.
	Scope(ctx context.Context, agentID string, registry *tool.Registry) (prompt string, err error)
}

// WithScope attaches the profile scoper.
func (a *Agent) WithScope(s Scoper) *Agent {
	a.scope = s
	return a
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
		ephemeral:     NewEphemeral(0, 0),
	}
}

// Router exposes the router so the API can report available backends.
func (a *Agent) Router() *llm.Router { return a.router }

// ErrTooManyIterations is returned when a turn exceeds the tool-call budget.
var ErrTooManyIterations = errors.New("tool iteration budget exhausted")

// ErrRepeatedToolFailure is returned when the model retried the same failing
// call unchanged until it was clearly not going to change its mind.
var ErrRepeatedToolFailure = errors.New("the same tool call failed repeatedly")

// repeatedToolFailureLimit is how many identical failures end the turn.
//
// A capable model reads a tool's error and fixes its arguments. A small local
// one often cannot: asked for a task "due 18/8/2026" it sends that string, is
// told the field wants YYYY-MM-DD, and sends it again — the same call, the
// same error, until the budget runs out. That is twelve model calls to reach a
// conclusion the second one already supported, which on local hardware is
// minutes of waiting for an error message that then names neither the tool nor
// the argument.
//
// Three is enough to distinguish a stuck model from one making progress: a
// genuine retry with different arguments resets the count, because the error
// text changes with it.
const repeatedToolFailureLimit = 3

// Advance appends the given messages to the session transcript and runs the
// loop until the model answers or asks for a client-side tool.
//
// clientTools are the tools the calling client declared it can execute. A
// browser passes none, and the model simply won't see filesystem tools.
func (a *Agent) Advance(ctx context.Context, sess *store.Session, clientTools []tool.Definition, incoming ...llm.Message) (*Turn, error) {
	// Stamp the incoming messages with the model that is about to see them.
	// The session's pin can be empty, meaning "the server default", which is
	// not a name anyone reading the transcript in a year could resolve — so it
	// is resolved to a concrete backend and model here, at the moment it is
	// true, rather than left for a later reader to guess at.
	inBackend, inModel := a.resolve(sess)
	// Where this conversation's history lives. For a recorded session this is
	// the store, exactly as before; for one that is off the record it is
	// memory, and nothing below can tell the difference.
	transcript := a.transcriptFor(sess)
	if err := transcript.Append(ctx, sess.ID, stamp(incoming, inBackend, inModel, 0)...); err != nil {
		return nil, err
	}

	registry, extraPrompt, err := a.registryFor(ctx, sess, clientTools)
	if err != nil {
		return nil, err
	}
	defs := registry.Definitions()

	system := a.system
	if extraPrompt != "" {
		system += "\n\n" + extraPrompt
	}

	turn := &Turn{SessionID: sess.ID}
	// The last tool failure, and how many times running it has repeated
	// unchanged. Both are reported if the turn ends badly: "budget exhausted"
	// on its own does not say which call was stuck, which is the only thing
	// the operator needs to know.
	var lastFailure string
	var repeats int
	for i := 0; i < a.maxIterations; i++ {
		history, err := transcript.Messages(ctx, sess.ID)
		if err != nil {
			return nil, err
		}

		res, err := a.router.Complete(ctx, sess.Backend, llm.Request{
			System:   system,
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

		// The assistant message records what actually served it, which is not
		// always what was asked for: a failed local backend is retried against
		// the fallback, and the transcript should say so.
		resp.Message.Backend = res.Backend
		resp.Message.Model = res.Model
		resp.Message.TokenCount = resp.Usage.CompletionTokens
		if err := transcript.Append(ctx, sess.ID, resp.Message); err != nil {
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
			if err := transcript.Append(ctx, sess.ID,
				stamp([]llm.Message{llm.ToolMessage(res)}, turn.Backend, turn.Model, 0)...); err != nil {
				return nil, err
			}
		}

		for _, call := range serverCalls {
			res := a.runServerTool(ctx, registry, sess, call, call.ID)
			if err := transcript.Append(ctx, sess.ID,
				stamp([]llm.Message{llm.ToolMessage(res)}, turn.Backend, turn.Model, 0)...); err != nil {
				return nil, err
			}
			// A result is still written for a repeated failure before the turn
			// ends, so the transcript records what was tried and why it was
			// refused rather than stopping mid-exchange.
			if res.IsError {
				if key := call.Name + ": " + res.Content; key == lastFailure {
					repeats++
					if repeats >= repeatedToolFailureLimit {
						a.log.Warn("stopping turn: repeated identical tool failure",
							"session", sess.ID, "tool", call.Name, "attempts", repeats,
							"error", res.Content)
						return nil, fmt.Errorf("%w: %s (tried %d times, unchanged)",
							ErrRepeatedToolFailure, key, repeats)
					}
				} else {
					lastFailure, repeats = key, 1
				}
			} else {
				lastFailure, repeats = "", 0
			}
		}

		if len(clientCalls) > 0 {
			turn.Status = StatusAwaitingClient
			turn.Reply = resp.Message.Content
			turn.Pending = a.describe(registry, clientCalls)
			return turn, nil
		}
	}

	if lastFailure != "" {
		return nil, fmt.Errorf("%w; the last tool error was %s", ErrTooManyIterations, lastFailure)
	}
	return nil, ErrTooManyIterations
}

// registryFor layers a client's declared tools over the server's own, then
// narrows the result to what this session's agent profile may reach.
//
// The batch tool is added here rather than at startup because it has to be
// bound to a session: that is what puts each worker's tool calls in the right
// audit trail. The knowledge tools are added here for a stronger reason — they
// are bound to the session's agent, so the library a model can search is
// decided by the session rather than named in a tool argument it could change.
func (a *Agent) registryFor(ctx context.Context, sess *store.Session, clientTools []tool.Definition) (*tool.Registry, string, error) {
	registry := a.serverTools.Clone()
	if a.pool != nil {
		if err := registry.Register(batchDefinition(a.pool), a.batchHandler(sess)); err != nil {
			return nil, "", fmt.Errorf("batch tool: %w", err)
		}
	}

	var extraPrompt string
	if a.scope != nil {
		prompt, err := a.scope.Scope(ctx, sess.AgentID, registry)
		if err != nil {
			return nil, "", err
		}
		extraPrompt = prompt
	}

	for _, def := range clientTools {
		if err := registry.RegisterClient(def); err != nil {
			return nil, "", fmt.Errorf("client tool %q: %w", def.Name, err)
		}
	}
	return registry, extraPrompt, nil
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
	// Deliberately unconditional, including for off-the-record conversations.
	// Muninn records what was *done* — a call proposed against the operator's
	// filesystem, and whether it was allowed — not what was said. A rename is
	// no less real for having been discussed privately, and an audit trail
	// with holes in it wherever the conversation was confidential is not an
	// audit trail. What the transcript held is gone; what it caused remains.
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

// resolve turns a session's backend/model pin into concrete names, filling in
// the router's default where the session names none.
func (a *Agent) resolve(sess *store.Session) (backend, model string) {
	backend, model = sess.Backend, sess.Model
	b, ok := a.router.Backend(backend)
	if !ok {
		return backend, model
	}
	if backend == "" {
		backend = b.Name
	}
	if model == "" {
		model = b.Model
	}
	return backend, model
}

// stamp records which model a batch of messages passed through. It is the one
// piece of provenance the transcript cannot reconstruct later: a session can be
// repointed at another backend at any time, so "which model wrote this line"
// has to be answered when the line is written.
func stamp(msgs []llm.Message, backend, model string, tokens int) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		m.Backend, m.Model, m.TokenCount = backend, model, tokens
		out[i] = m
	}
	return out
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
