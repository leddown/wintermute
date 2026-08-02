package client

import (
	"context"
	"errors"
	"fmt"
	"io"

	"wintermute/internal/client/actions"
	"wintermute/internal/tool"
)

// Harness drives one conversation from the client side.
type Harness struct {
	api      *API
	actions  *actions.Set
	policy   Policy
	prompter *Prompter
	out      io.Writer

	sessionID string
}

// NewHarness builds a harness. The caller starts or resumes a session with
// Start before calling Ask.
func NewHarness(api *API, set *actions.Set, policy Policy, prompter *Prompter, out io.Writer) *Harness {
	return &Harness{api: api, actions: set, policy: policy, prompter: prompter, out: out}
}

// Start opens a new session on the server.
func (h *Harness) Start(ctx context.Context, title string) error {
	sess, err := h.api.CreateSession(ctx, title)
	if err != nil {
		return err
	}
	h.sessionID = sess.ID
	return nil
}

// SessionID reports the active session.
func (h *Harness) SessionID() string { return h.sessionID }

// ErrNoSession is returned when Ask is called before Start.
var ErrNoSession = errors.New("no active session")

// Ask sends one user message and runs the loop until the model produces a
// final reply. Each round trip is: server asks for local tools → policy and
// user decide → tools run here → results go back.
func (h *Harness) Ask(ctx context.Context, text string) error {
	if h.sessionID == "" {
		return ErrNoSession
	}
	h.prompter.Reset()

	defs := h.actions.Definitions()
	turn, err := h.api.SendMessage(ctx, h.sessionID, text, defs)
	if err != nil {
		return err
	}

	for {
		if turn.Reply != "" {
			fmt.Fprintf(h.out, "\n%s\n", turn.Reply)
		}
		if turn.Status != StatusAwaitingClient {
			return nil
		}
		if len(turn.Pending) == 0 {
			// The server said it was waiting on us but named nothing to do.
			// Continuing would spin, so stop and say why.
			return errors.New("server requested client actions but sent none")
		}

		results := h.handle(ctx, turn.Pending)
		turn, err = h.api.SendResults(ctx, h.sessionID, results, defs)
		if err != nil {
			return err
		}
	}
}

// handle applies the approval policy to each pending call and executes the
// approved ones. Refusals still produce a result: the model needs to be told
// the action did not happen, or it will report success it never achieved.
func (h *Harness) handle(ctx context.Context, pending []PendingCall) []ResultPayload {
	results := make([]ResultPayload, 0, len(pending))

	for _, call := range pending {
		payload := ResultPayload{
			CallID:   call.ID,
			ToolName: call.Name,
			Input:    call.Input,
			Risk:     call.Risk,
		}

		// A tool this machine doesn't offer never reaches the approval stage.
		if _, ok := h.actions.Lookup(call.Name); !ok {
			payload.Decision = DecisionBlocked
			payload.Content = fmt.Sprintf("This machine has no tool named %q.", call.Name)
			payload.IsError = true
			results = append(results, payload)
			continue
		}

		decision, settled := h.policy.Evaluate(call.Name, call.Risk)
		if !settled {
			var err error
			decision, err = h.prompter.Confirm(call)
			if err != nil {
				payload.Decision = DecisionDenied
				payload.Content = fmt.Sprintf("Approval could not be collected: %v", err)
				payload.IsError = true
				results = append(results, payload)
				continue
			}
		}

		if !decision.Allow {
			reason := decision.Reason
			if reason == "" {
				reason = "The user declined this action."
			}
			fmt.Fprintf(h.out, "  ✗ %s — declined\n", call.Name)
			payload.Decision = decision.Record
			payload.Content = reason + " The file was not changed. Do not retry it without being asked."
			results = append(results, payload)
			continue
		}

		res := h.actions.Execute(ctx, tool.Call{ID: call.ID, Name: call.Name, Input: call.Input})
		payload.Decision = decision.Record
		payload.Content = res.Content
		payload.IsError = res.IsError

		if res.IsError {
			fmt.Fprintf(h.out, "  ! %s — %s\n", call.Name, res.Content)
		} else if call.Risk != tool.RiskRead {
			fmt.Fprintf(h.out, "  ✓ %s\n", res.Content)
		}
		results = append(results, payload)
	}

	return results
}
