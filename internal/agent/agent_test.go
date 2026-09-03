package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"wintermute/internal/llm"
	"wintermute/internal/store"
	"wintermute/internal/store/storetest"
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

	st := storetest.New(t)

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

// The prompt must name what is actually serving the turn, and claim nothing
// else.
//
// SystemPrompt used to assert "You are Claude, running on Anthropic's API" for
// every conversation. A session pinned to the local core backend answered
// "under the hood I'm Claude, running on Anthropic's API" on a turn whose own
// result named qwen3.8-27b — the model dutifully repeating the only thing it
// had been told about itself.
func TestSystemPromptNamesTheBackendServingIt(t *testing.T) {
	p := &scriptedProvider{responses: []llm.Response{reply("ok")}}
	a, st, sess := newTestAgent(t, p, tool.NewRegistry())

	if _, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	system := p.requests[0].System
	if !strings.Contains(system, "the test backend") {
		t.Errorf("system prompt does not name the backend serving the turn:\n%s", system)
	}
	// The specific words that were wrong, rather than a general vendor sweep:
	// this is the sentence that was there and must not come back.
	for _, claim := range []string{"You are Claude", "Anthropic's API"} {
		if strings.Contains(system, claim) {
			t.Errorf("system prompt still asserts %q whatever backend answers:\n%s", claim, system)
		}
	}

	// It follows the session rather than being fixed at startup: repointing a
	// conversation at another backend has to change what it is told it is.
	if err := st.SetSessionModel(context.Background(), sess.ID, "test", "some-local-model"); err != nil {
		t.Fatal(err)
	}
	sess.Backend, sess.Model = "test", "some-local-model"
	p.responses = append(p.responses, reply("ok"))
	if _, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("and now?")); err != nil {
		t.Fatal(err)
	}
	if last := p.requests[len(p.requests)-1].System; !strings.Contains(last, "some-local-model") {
		t.Errorf("repointed session was not told its new model:\n%s", last)
	}

	// A Core chat is not given it. Its whole purpose is to show the model
	// rather than the harness, and it never carried the wrong claim either.
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
	if last := p.requests[len(p.requests)-1].System; strings.Contains(last, "This conversation is being served by") {
		t.Errorf("toolless session was framed with the model line:\n%s", last)
	}
}

// webScope is a Scoper that grants only the web, as internal/app's does for a
// Core chat. configured=false stands for a server with no SEARXNG_URL.
type webScope struct {
	configured bool
	scoped     bool
}

func (s *webScope) Scope(context.Context, int64, string, *tool.Registry) (string, error) {
	s.scoped = true
	return "", nil
}

func (s *webScope) ScopeWeb(reg *tool.Registry) (bool, string, error) {
	if !s.configured {
		return false, "", nil
	}
	err := reg.Register(
		tool.Definition{Name: "web_search", Description: "search", Risk: tool.RiskRead},
		func(context.Context, json.RawMessage) (string, error) { return "{}", nil })
	if err != nil {
		return false, "", err
	}
	return true, "", nil
}

// A Core chat with the web switched on gets the two web tools and nothing else.
//
// The three assertions are one fact between them: it is offered the web, it is
// not offered the server's own tools, and it is told which of those is true.
// The prompt matters as much as the registry — PlainPrompt states outright that
// the model cannot search, and a model handed that sentence alongside a working
// web_search either refuses or apologises for using it.
func TestToollessSessionWithWebGetsTheWebAndNothingElse(t *testing.T) {
	reg := tool.NewRegistry()
	err := reg.Register(
		tool.Definition{Name: "probe_thing", Description: "lookup", Risk: tool.RiskRead},
		func(context.Context, json.RawMessage) (string, error) { return "{}", nil })
	if err != nil {
		t.Fatal(err)
	}

	p := &scriptedProvider{responses: []llm.Response{reply("Looked it up.")}}
	scope := &webScope{configured: true}
	a, st, _ := newTestAgent(t, p, reg)
	a = a.WithScope(scope)

	client, _, err := st.CreateClient(context.Background(), "core-client", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(context.Background(), client.ID, "core", "", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Web {
		t.Fatal("a new session came back with web access nobody granted")
	}
	if err := st.SetSessionWeb(context.Background(), sess.ID, client.ID, true); err != nil {
		t.Fatal(err)
	}
	sess.Web = true

	// The client offers one of its own, which a toolless session drops whether
	// or not it has the web: the grant is the network, not the harness.
	_, err = a.Advance(context.Background(), sess,
		[]tool.Definition{clientTool("rename_file", tool.RiskWrite)},
		llm.UserMessage("what happened today"))
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, d := range p.requests[0].Tools {
		names = append(names, d.Name)
	}
	if len(names) != 1 || names[0] != "web_search" {
		t.Errorf("toolless web session was offered %v, want [web_search]", names)
	}
	if !strings.HasPrefix(p.requests[0].System, PlainWebPrompt) {
		t.Errorf("web-enabled Core chat was not framed as one:\n%s", p.requests[0].System)
	}
	if scope.scoped {
		t.Error("a toolless session was given the agent-profile scope as well as the web")
	}
}

// A server with no web search must produce a plain toolless turn, not a ticked
// box and an empty promise. The flag is the operator's intent; whether it can
// be honoured is the server's answer, and the prompt has to follow the answer.
func TestToollessWebSessionFallsBackWhenWebIsNotConfigured(t *testing.T) {
	p := &scriptedProvider{responses: []llm.Response{reply("From memory, then.")}}
	a, st, _ := newTestAgent(t, p, tool.NewRegistry())
	a = a.WithScope(&webScope{configured: false})

	client, _, err := st.CreateClient(context.Background(), "core-client", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(context.Background(), client.ID, "core", "", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionWeb(context.Background(), sess.ID, client.ID, true); err != nil {
		t.Fatal(err)
	}
	sess.Web = true

	if _, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	if n := len(p.requests[0].Tools); n != 0 {
		t.Errorf("unconfigured web session was offered %d tools, want 0", n)
	}
	if !strings.HasPrefix(p.requests[0].System, PlainPrompt) {
		t.Errorf("unconfigured web session was told it could search:\n%s", p.requests[0].System)
	}
}
