package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wintermute/internal/llm"
	"wintermute/internal/store"
	"wintermute/internal/tool"
)

// scriptedProvider replays a fixed list of responses, recording the requests
// it received so tests can assert on what the model was shown.
type scriptedProvider struct {
	responses []llm.Response
	requests  []llm.Request
	calls     int
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.requests = append(p.requests, req)
	if p.calls >= len(p.responses) {
		return nil, errors.New("scripted provider ran out of responses")
	}
	resp := p.responses[p.calls]
	p.calls++
	return &resp, nil
}

func reply(text string) llm.Response {
	return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: text}, StopReason: "stop"}
}

func toolCall(name, input string) llm.Response {
	return llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			ToolCalls: []tool.Call{{ID: "call_1", Name: name, Input: json.RawMessage(input)}},
		},
		StopReason: "tool_calls",
	}
}

func newTestAgent(t *testing.T, p llm.Provider, reg *tool.Registry) (*Agent, *store.Store, *store.Session) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	client, _, err := st.CreateClient(context.Background(), "test-harness", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(context.Background(), client.ID, "test", "", "", "", true)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router, err := llm.NewRouter([]*llm.Backend{{Name: "test", Provider: p}}, "test", "", log)
	if err != nil {
		t.Fatal(err)
	}
	return New(router, st, reg, log, 8), st, sess
}

func clientTool(name string, risk tool.Risk) tool.Definition {
	return tool.Definition{Name: name, Description: name, Risk: risk, Side: tool.SideClient}
}

func TestAdvanceReturnsFinalReply(t *testing.T) {
	p := &scriptedProvider{responses: []llm.Response{reply("Nothing to rename.")}}
	a, st, sess := newTestAgent(t, p, tool.NewRegistry())

	turn, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("check my library"))
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != StatusComplete {
		t.Errorf("status = %q, want %q", turn.Status, StatusComplete)
	}
	if turn.Reply != "Nothing to rename." {
		t.Errorf("reply = %q", turn.Reply)
	}

	// The user message and the assistant reply must both be durable.
	msgs, err := st.Messages(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("transcript has %d messages, want 2: %+v", len(msgs), msgs)
	}
}

func TestAdvanceRunsServerToolsInline(t *testing.T) {
	reg := tool.NewRegistry()
	var gotInput string
	err := reg.Register(
		tool.Definition{Name: "probe_thing", Description: "lookup", Risk: tool.RiskRead},
		func(_ context.Context, in json.RawMessage) (string, error) {
			gotInput = string(in)
			return `{"matches":[{"title":"Pilot"}]}`, nil
		})
	if err != nil {
		t.Fatal(err)
	}

	p := &scriptedProvider{responses: []llm.Response{
		toolCall("probe_thing", `{"kind":"episode","title":"Show"}`),
		reply("Found it."),
	}}
	a, st, sess := newTestAgent(t, p, reg)

	turn, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("identify this"))
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != StatusComplete {
		t.Fatalf("status = %q, want %q (server tools must not pause the turn)", turn.Status, StatusComplete)
	}
	if gotInput != `{"kind":"episode","title":"Show"}` {
		t.Errorf("handler received %q", gotInput)
	}

	// The tool result is recorded in the durable audit trail, not just the chat.
	entries, err := st.AuditForSession(context.Background(), sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d audit entries, want 1", len(entries))
	}
	if entries[0].Decision != store.DecisionAuto || entries[0].Side != tool.SideServer {
		t.Errorf("audit = %+v, want auto/server", entries[0])
	}
}

func TestAdvancePausesForClientTools(t *testing.T) {
	p := &scriptedProvider{responses: []llm.Response{
		toolCall("rename_file", `{"path":"/media/a.mkv","new_name":"b.mkv"}`),
	}}
	a, _, sess := newTestAgent(t, p, tool.NewRegistry())

	turn, err := a.Advance(context.Background(), sess, []tool.Definition{clientTool("rename_file", tool.RiskWrite)},
		llm.UserMessage("rename it"))
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != StatusAwaitingClient {
		t.Fatalf("status = %q, want %q", turn.Status, StatusAwaitingClient)
	}
	if len(turn.Pending) != 1 {
		t.Fatalf("got %d pending calls, want 1", len(turn.Pending))
	}
	// The risk level travels with the call so the client can apply its policy.
	if turn.Pending[0].Risk != tool.RiskWrite {
		t.Errorf("pending risk = %q, want %q", turn.Pending[0].Risk, tool.RiskWrite)
	}
	if turn.Pending[0].Name != "rename_file" {
		t.Errorf("pending name = %q", turn.Pending[0].Name)
	}

	// Posting the result resumes the same loop.
	p.responses = append(p.responses, reply("Renamed."))
	turn, err = a.Advance(context.Background(), sess, []tool.Definition{clientTool("rename_file", tool.RiskWrite)},
		llm.ToolMessage(tool.Result{CallID: "call_1", Content: "Renamed a.mkv to b.mkv."}))
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != StatusComplete || turn.Reply != "Renamed." {
		t.Errorf("resumed turn = %+v", turn)
	}
}

// A client that declares no tools must never be offered filesystem tools —
// this is what keeps the browser UI from being asked to rename anything.
func TestClientToolsAreScopedToTheCaller(t *testing.T) {
	p := &scriptedProvider{responses: []llm.Response{reply("ok")}}
	a, _, sess := newTestAgent(t, p, tool.NewRegistry())

	if _, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(p.requests))
	}
	if len(p.requests[0].Tools) != 0 {
		t.Errorf("model was offered %d tools for a caller that declared none: %+v",
			len(p.requests[0].Tools), p.requests[0].Tools)
	}
}

// A model can invent a tool name. The turn must recover rather
// than fail, so the model gets a chance to correct itself.
func TestUnknownToolIsFedBackToTheModel(t *testing.T) {
	p := &scriptedProvider{responses: []llm.Response{
		toolCall("delete_everything", `{}`),
		reply("Sorry, I will not do that."),
	}}
	a, st, sess := newTestAgent(t, p, tool.NewRegistry())

	turn, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("go"))
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != StatusComplete {
		t.Fatalf("status = %q, want %q", turn.Status, StatusComplete)
	}

	msgs, err := st.Messages(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for _, m := range msgs {
		if m.Role == llm.RoleTool && m.IsError {
			sawError = true
		}
	}
	if !sawError {
		t.Error("no error tool-result was recorded for the unknown tool")
	}
}

func TestAdvanceStopsAtIterationBudget(t *testing.T) {
	reg := tool.NewRegistry()
	err := reg.Register(
		tool.Definition{Name: "loop", Description: "loops", Risk: tool.RiskRead},
		func(context.Context, json.RawMessage) (string, error) { return "again", nil })
	if err != nil {
		t.Fatal(err)
	}

	// More scripted tool calls than the budget allows.
	var responses []llm.Response
	for i := 0; i < 20; i++ {
		responses = append(responses, toolCall("loop", `{}`))
	}
	p := &scriptedProvider{responses: responses}
	a, _, sess := newTestAgent(t, p, reg)

	_, err = a.Advance(context.Background(), sess, nil, llm.UserMessage("go"))
	if !errors.Is(err, ErrTooManyIterations) {
		t.Fatalf("err = %v, want ErrTooManyIterations", err)
	}
}

func TestTitleTruncates(t *testing.T) {
	long := ""
	for i := 0; i < 100; i++ {
		long += "x"
	}
	if got := Title(long); len(got) != 63 {
		t.Errorf("Title length = %d, want 63 (60 + ellipsis)", len(got))
	}
	if got := Title("  rename my\nshow files  "); got != "rename my show files" {
		t.Errorf("Title = %q", got)
	}
}

// A toolless session is the Core chat: a model and nothing else in the room.
//
// The two halves are checked together because either alone is a trap. An empty
// tool list with the tool-using system prompt still produces a model that
// narrates calls it cannot make, and the right prompt with the tools still
// attached is not a plain conversation at all.
func TestToollessSessionOffersNoToolsAndIsFramedAsSuch(t *testing.T) {
	reg := tool.NewRegistry()
	err := reg.Register(
		tool.Definition{Name: "probe_thing", Description: "lookup", Risk: tool.RiskRead},
		func(context.Context, json.RawMessage) (string, error) { return "{}", nil })
	if err != nil {
		t.Fatal(err)
	}

	p := &scriptedProvider{responses: []llm.Response{reply("Just talking.")}}
	a, st, _ := newTestAgent(t, p, reg)

	client, _, err := st.CreateClient(context.Background(), "core-client", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(context.Background(), client.ID, "core", "", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Tools {
		t.Fatal("session created with tools:false came back with tools enabled")
	}

	// The client offers one of its own too; a toolless session must drop it
	// rather than pass it through to the model.
	_, err = a.Advance(context.Background(), sess,
		[]tool.Definition{clientTool("rename_file", tool.RiskWrite)},
		llm.UserMessage("hello"))
	if err != nil {
		t.Fatal(err)
	}

	if len(p.requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(p.requests))
	}
	if n := len(p.requests[0].Tools); n != 0 {
		t.Errorf("toolless session offered %d tools, want 0: %+v", n, p.requests[0].Tools)
	}
	// Prefix rather than equality: the date is appended to whichever prompt is
	// chosen, and a Core chat needs it as much as any other.
	if !strings.HasPrefix(p.requests[0].System, PlainPrompt) {
		t.Errorf("toolless session got the tool-using system prompt:\n%s", p.requests[0].System)
	}
}

// The ordinary session is the control: the same agent, the same registry, and
// the tools are there. Without this the test above passes on an agent that has
// simply stopped offering tools to anybody.
func TestToolUsingSessionStillGetsItsTools(t *testing.T) {
	reg := tool.NewRegistry()
	err := reg.Register(
		tool.Definition{Name: "probe_thing", Description: "lookup", Risk: tool.RiskRead},
		func(context.Context, json.RawMessage) (string, error) { return "{}", nil })
	if err != nil {
		t.Fatal(err)
	}

	p := &scriptedProvider{responses: []llm.Response{reply("ok")}}
	a, _, sess := newTestAgent(t, p, reg)

	if _, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	if len(p.requests[0].Tools) == 0 {
		t.Error("ordinary session was offered no tools")
	}
	if strings.HasPrefix(p.requests[0].System, PlainPrompt) {
		t.Error("ordinary session got the toolless prompt")
	}
}

// The model has to be told what day it is.
//
// Without it, "next Friday" is answered from training data: on the live server
// a task created in August 2026 came back due 2023-10-13 — a Friday, but the
// wrong one, in the wrong year. Nothing downstream could catch that, because
// 2023-10-13 is a perfectly well-formed date.
func TestSystemPromptCarriesTodaysDate(t *testing.T) {
	p := &scriptedProvider{responses: []llm.Response{reply("ok")}}
	a, st, sess := newTestAgent(t, p, tool.NewRegistry())

	if _, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("Monday, 2 January 2006")
	if !strings.Contains(p.requests[0].System, today) {
		t.Errorf("system prompt does not say today is %q:\n%s", today, p.requests[0].System)
	}

	// A toolless Core session needs it just as much: it is asked the date
	// questions a person asks, and has no agenda tool to learn it from.
	client, _, err := st.CreateClient(context.Background(), "core", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := st.CreateSession(context.Background(), client.ID, "core", "", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	p.responses = append(p.responses, reply("ok"))
	if _, err := a.Advance(context.Background(), plain, nil, llm.UserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	last := p.requests[len(p.requests)-1].System
	if !strings.Contains(last, today) {
		t.Errorf("toolless session was not told the date:\n%s", last)
	}
	if !strings.Contains(last, "no tools in this conversation") {
		t.Errorf("toolless session lost its plain framing:\n%s", last)
	}
}
