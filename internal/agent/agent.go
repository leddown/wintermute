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
	"time"

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
	// indexer, when set, is told about messages that were durably written so
	// they can be embedded for retrieval. Optional: without an embedder
	// configured the loop behaves exactly as it did before memory existed.
	indexer Indexer
	// recaller, when set, supplies prior context for a new turn.
	recaller Recaller
}

// Recaller retrieves prior context for a turn and renders it as a block to put
// in front of the user's message.
//
// It returns a string rather than an error on purpose. Retrieval is an
// enhancement to a conversation that works without it, so a failed or empty
// recall must be indistinguishable from having found nothing: the turn
// proceeds on its own transcript. An interface here keeps the loop from
// importing the recall package.
type Recaller interface {
	Recall(ctx context.Context, sess *store.Session, query string) string
	// Framing is the static instruction describing how to read a prior-context
	// block. It lives with the code that renders the block rather than being
	// duplicated here, and it is added to the system prompt only on turns that
	// actually carry one — it never varies, so it does not disturb the cached
	// prefix on the turns that do.
	Framing() string
}

// WithRecall attaches the memory layer.
func (a *Agent) WithRecall(r Recaller) *Agent {
	a.recaller = r
	return a
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
	//
	// clientID is passed because some of those tools read the client's own
	// history, and their scope has to be bound at registration rather than
	// taken from an argument the model fills in — the same rule that keeps a
	// session's document library decided by the session.
	Scope(ctx context.Context, clientID int64, agentID string, registry *tool.Registry) (prompt string, err error)
}

// WithScope attaches the profile scoper.
func (a *Agent) WithScope(s Scoper) *Agent {
	a.scope = s
	return a
}

// New builds an Agent. serverTools holds only tools the server executes;
// client-declared tools are layered on per session.
func New(router *llm.Router, s *store.Store, serverTools *tool.Registry, log *slog.Logger, maxIterations int) *Agent {
	return &Agent{
		router:        router,
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

	// A toolless conversation is framed as one. See PlainPrompt: handing it the
	// tool-using prompt makes the model narrate calls it cannot make.
	system := a.system
	if !sess.Tools {
		system = PlainPrompt
	}
	system += "\n\n" + todayLine()
	if extraPrompt != "" {
		system += "\n\n" + extraPrompt
	}

	// Prior context, retrieved once for this turn rather than once per
	// iteration: the question does not change while the model works through
	// its tool calls, and re-running retrieval each time would pay for the
	// same answer repeatedly.
	//
	// It is deliberately not appended to the system prompt. The system prompt
	// is the cached prefix of every request — Anthropic's cache hierarchy and
	// a local backend's KV cache both key on it — and retrieved memory changes
	// every turn, so putting it there would invalidate the whole prefix each
	// time and make local models reprocess the entire transcript. See
	// recall.Render.
	var priorContext string
	if a.recaller != nil && sess.Recall {
		if query := lastUserText(incoming); query != "" {
			priorContext = a.recaller.Recall(ctx, sess, query)
		}
	}
	if priorContext != "" {
		system += "\n\n" + a.recaller.Framing()
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
		// The block goes in front of the user's own words, inside their
		// message, rather than as a message of its own: providers differ on
		// whether two consecutive user messages are acceptable, and this needs
		// no structural change to the conversation at all. It is applied to
		// the copy being sent, never written to the transcript — it is derived
		// from the store and re-deriving it is free, while storing it would
		// both duplicate the content and feed it back into future retrieval.
		history = withPriorContext(history, priorContext)

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

// todayLine tells the model what day it is.
//
// Without it a model asked for "next Friday" has nothing to count from and
// answers out of its training data: on a live server this put a task created
// in August 2026 due on 2023-10-13, which is a Friday — just the wrong one, in
// the wrong year. Nothing else was going to catch that. The date tools take
// YYYY-MM-DD and 2023-10-13 is perfectly well formed, so it was stored, shown
// and never questioned.
//
// The date is not in the constant prompts because it is not constant. It is
// appended here, where every turn is assembled, so a server left running
// across midnight starts saying the new date without a restart.
//
// It does put a daily-changing line in the cached prefix, which is the thing
// the system prompt is otherwise kept free of. That is a cache miss a day per
// session — the rule exists to keep out things that change every *turn*, and
// paying once a day to stop the assistant inventing dates is not a close call.
func todayLine() string {
	now := time.Now()
	return fmt.Sprintf("Today is %s. Work out any relative date — \"tomorrow\", "+
		"\"next Friday\", \"in three weeks\" — from that, and send tools an "+
		"absolute YYYY-MM-DD.", now.Format("Monday, 2 January 2006"))
}

// registryFor layers a client's declared tools over the server's own, then
// narrows the result to what this session's agent profile may reach.
//
// The knowledge tools are added here rather than at startup because they are
// bound to the session's agent, so the library a model can search is decided by
// the session rather than named in a tool argument it could change.
func (a *Agent) registryFor(ctx context.Context, sess *store.Session, clientTools []tool.Definition) (*tool.Registry, string, error) {
	// A session with no tools gets an empty registry, and the client's declared
	// tools are dropped on the floor rather than registered. The refusal is
	// here rather than at the API because this is the only place that can see
	// every source at once — the server's own, the agent's, and whatever the
	// harness on the other end has offered.
	if !sess.Tools {
		return tool.NewRegistry(), "", nil
	}

	registry := a.serverTools.Clone()

	var extraPrompt string
	if a.scope != nil {
		prompt, err := a.scope.Scope(ctx, sess.ClientID, sess.AgentID, registry)
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

// lastUserText finds the newest user message in a batch, which is the query a
// turn is retrieved against. Tool results arriving from a client carry no
// question, so a resumed turn retrieves nothing new — the context fetched when
// the user actually spoke is still in front of them.
func lastUserText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// withPriorContext returns a copy of the history with the retrieved block
// placed immediately before the most recent user message's text.
//
// A copy: the caller's slice is the transcript as stored, and mutating it
// would persist the block on the next append and feed the model's own injected
// context back into the index.
func withPriorContext(history []llm.Message, block string) []llm.Message {
	if block == "" {
		return history
	}
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != llm.RoleUser {
			continue
		}
		out := make([]llm.Message, len(history))
		copy(out, history)
		out[i].Content = block + "\n\n" + out[i].Content
		return out
	}
	return history
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
