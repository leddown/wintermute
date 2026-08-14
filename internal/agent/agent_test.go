package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

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
	sess, err := st.CreateSession(context.Background(), client.ID, "test", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router, err := llm.NewRouter([]*llm.Backend{{Name: "test", Provider: p}}, "test", "", log)
	if err != nil {
		t.Fatal(err)
	}
	return New(router, nil, st, reg, log, 8), st, sess
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
		tool.Definition{Name: "lookup_metadata", Description: "lookup", Risk: tool.RiskRead},
		func(_ context.Context, in json.RawMessage) (string, error) {
			gotInput = string(in)
			return `{"matches":[{"title":"Pilot"}]}`, nil
		})
	if err != nil {
		t.Fatal(err)
	}

	p := &scriptedProvider{responses: []llm.Response{
		toolCall("lookup_metadata", `{"kind":"episode","title":"Show"}`),
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
