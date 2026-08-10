package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"wintermute/internal/llm"
	"wintermute/internal/store"
	"wintermute/internal/tool"
)

// workerProvider stands in for a pool member. Unlike scriptedProvider it is
// called from several goroutines at once, so everything it records is guarded.
type workerProvider struct {
	mu       sync.Mutex
	requests []llm.Request
	// reply is called per request to produce a response; it must be safe to
	// call concurrently.
	reply func(req llm.Request) (*llm.Response, error)
}

func (p *workerProvider) Name() string { return "worker" }

func (p *workerProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return p.reply(req)
}

func (p *workerProvider) seen() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

// proposeJSON is a well-behaved worker: one JSON object, no prose.
func proposeJSON(req llm.Request) (*llm.Response, error) {
	name := "Renamed.mkv"
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "Filename: ") {
			_, after, _ := strings.Cut(m.Content, "Filename: ")
			raw, _, _ := strings.Cut(after, "\n")
			name = "Proposed - " + strings.TrimSpace(raw)
		}
	}
	body, _ := json.Marshal(map[string]any{"proposed_name": name, "reason": "looked it up"})
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Content: string(body)},
		StopReason: "stop",
	}, nil
}

// newBatchAgent builds an agent whose main loop is scripted and whose pool is
// served by workers, so a test can drive both halves independently.
func newBatchAgent(t *testing.T, main llm.Provider, workers *workerProvider, reg *tool.Registry) (*Agent, *store.Store, *store.Session) {
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
	sess, err := st.CreateSession(context.Background(), client.ID, "test", "", "")
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router, err := llm.NewRouter([]*llm.Backend{{Name: "main", Provider: main}}, "main", "", log)
	if err != nil {
		t.Fatal(err)
	}

	var pool *llm.Pool
	if workers != nil {
		pool, err = llm.NewPool("batch",
			[]llm.PoolMember{{Backend: &llm.Backend{Name: "worker", Model: "small", Provider: workers}, Slots: 2}}, log)
		if err != nil {
			t.Fatal(err)
		}
	}
	return New(router, pool, st, reg, log, 8), st, sess
}

func batchCall(files ...string) llm.Response {
	in, _ := json.Marshal(batchInput{Directory: "/mnt/media/tv", Files: files})
	return llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			ToolCalls: []tool.Call{{ID: "call_1", Name: "batch_propose_names", Input: in}},
		},
		StopReason: "tool_calls",
	}
}

// readTool registers a read-only server tool that records that it ran.
func readTool(t *testing.T, reg *tool.Registry, name string, calls *int64, mu *sync.Mutex) {
	t.Helper()
	err := reg.Register(
		tool.Definition{Name: name, Description: name, Risk: tool.RiskRead},
		func(context.Context, json.RawMessage) (string, error) {
			mu.Lock()
			*calls++
			mu.Unlock()
			return `{"matches":[{"title":"Pilot"}]}`, nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBatchFansOutAndCollectsProposals(t *testing.T) {
	files := []string{"a.s01e01.mkv", "b.s01e02.mkv", "c.s01e03.mkv"}
	main := &scriptedProvider{responses: []llm.Response{batchCall(files...), reply("Proposed 3 renames.")}}
	workers := &workerProvider{reply: proposeJSON}

	a, st, sess := newBatchAgent(t, main, workers, tool.NewRegistry())

	turn, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("name these"))
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != StatusComplete {
		t.Fatalf("status = %q, want %q (a batch runs on the server and must not pause the turn)", turn.Status, StatusComplete)
	}

	// One worker request per file, not one for the lot.
	if got := len(workers.seen()); got != len(files) {
		t.Errorf("workers saw %d requests, want %d — items must be independent prompts", got, len(files))
	}

	// The tool result carried a proposal per file back into the transcript.
	msgs, err := st.Messages(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var report BatchReport
	var found bool
	for _, m := range msgs {
		if m.Role != llm.RoleTool {
			continue
		}
		if err := json.Unmarshal([]byte(m.Content), &report); err == nil && report.Proposals != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("no batch report was written to the transcript")
	}
	if len(report.Proposals) != len(files) {
		t.Fatalf("report has %d proposals, want %d", len(report.Proposals), len(files))
	}
	if report.Summary.Proposed != len(files) || report.Summary.Failed != 0 {
		t.Errorf("summary = %+v", report.Summary)
	}
	for i, p := range report.Proposals {
		if p.File != files[i] {
			t.Errorf("proposal %d is for %q, want %q — order must match the input", i, p.File, files[i])
		}
		if p.Proposed != "Proposed - "+files[i] {
			t.Errorf("proposal %d = %q", i, p.Proposed)
		}
		if p.Backend != "worker" {
			t.Errorf("proposal %d credits backend %q", i, p.Backend)
		}
	}
	// The model must be told these are proposals; the whole risk of this tool
	// is a model reading them as work already done.
	if !strings.Contains(report.Note, "proposals only") {
		t.Errorf("report note = %q", report.Note)
	}
}

// The security property that makes fan-out safe: a worker is given server-side
// read-only tools and nothing else, whatever the caller declared.
func TestBatchWorkersNeverSeeClientOrWriteTools(t *testing.T) {
	reg := tool.NewRegistry()
	var calls int64
	var mu sync.Mutex
	readTool(t, reg, "lookup_metadata", &calls, &mu)

	// A server-side tool that mutates something. It must not reach a worker.
	err := reg.Register(
		tool.Definition{Name: "server_write_thing", Description: "writes", Risk: tool.RiskWrite},
		func(context.Context, json.RawMessage) (string, error) { return "done", nil })
	if err != nil {
		t.Fatal(err)
	}

	main := &scriptedProvider{responses: []llm.Response{batchCall("a.mkv"), reply("done")}}
	workers := &workerProvider{reply: proposeJSON}
	a, _, sess := newBatchAgent(t, main, workers, reg)

	clientTools := []tool.Definition{
		clientTool("rename_file", tool.RiskWrite),
		clientTool("list_directory", tool.RiskRead),
	}
	if _, err := a.Advance(context.Background(), sess, clientTools, llm.UserMessage("go")); err != nil {
		t.Fatal(err)
	}

	seen := workers.seen()
	if len(seen) == 0 {
		t.Fatal("no worker request was made")
	}
	for _, req := range seen {
		for _, def := range req.Tools {
			switch def.Name {
			case "lookup_metadata":
				// The one tool a worker is meant to have.
			case "rename_file", "list_directory":
				t.Errorf("a batch worker was offered the client tool %q — a worker must never be able to touch the filesystem", def.Name)
			case "server_write_thing":
				t.Errorf("a batch worker was offered the write-risk tool %q", def.Name)
			case "batch_propose_names":
				t.Errorf("a batch worker was offered the batch tool itself, which would let a batch start a batch")
			default:
				t.Errorf("a batch worker was offered an unexpected tool %q", def.Name)
			}
		}
	}
}

// Each worker's tool calls are audited against the session, namespaced by item
// so concurrent workers reusing a stock call id stay distinguishable.
func TestBatchAuditsEveryWorkerToolCall(t *testing.T) {
	reg := tool.NewRegistry()
	var calls int64
	var mu sync.Mutex
	readTool(t, reg, "lookup_metadata", &calls, &mu)

	// Every worker looks up once, then answers.
	lookupThenAnswer := func(req llm.Request) (*llm.Response, error) {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool {
				return proposeJSON(req)
			}
		}
		return &llm.Response{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []tool.Call{{ID: "call_1", Name: "lookup_metadata", Input: json.RawMessage(`{"kind":"episode","title":"Show"}`)}},
			},
			StopReason: "tool_calls",
		}, nil
	}

	files := []string{"a.mkv", "b.mkv", "c.mkv"}
	main := &scriptedProvider{responses: []llm.Response{batchCall(files...), reply("done")}}
	workers := &workerProvider{reply: lookupThenAnswer}
	a, st, sess := newBatchAgent(t, main, workers, reg)

	if _, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("go")); err != nil {
		t.Fatal(err)
	}

	entries, err := st.AuditForSession(context.Background(), sess.ID, 50)
	if err != nil {
		t.Fatal(err)
	}

	ids := map[string]bool{}
	var lookups int
	for _, e := range entries {
		if e.ToolName != "lookup_metadata" {
			continue
		}
		lookups++
		if !strings.HasPrefix(e.CallID, "batch/") {
			t.Errorf("audit call id %q is not namespaced to its batch item", e.CallID)
		}
		if ids[e.CallID] {
			t.Errorf("two audit rows share the call id %q; concurrent items are indistinguishable", e.CallID)
		}
		ids[e.CallID] = true
	}
	if lookups != len(files) {
		t.Errorf("audited %d worker lookups, want %d — every item's call must be recorded", lookups, len(files))
	}

	// The batch tool call itself is audited too.
	var sawBatch bool
	for _, e := range entries {
		if e.ToolName == "batch_propose_names" && e.Side == tool.SideServer {
			sawBatch = true
		}
	}
	if !sawBatch {
		t.Error("the batch tool call itself was not audited")
	}
}

// A failing item is reported as failed and does not take the batch with it.
func TestBatchReportsPerItemFailure(t *testing.T) {
	failB := func(req llm.Request) (*llm.Response, error) {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "Filename: b.mkv") {
				return nil, errors.New("backend exploded")
			}
		}
		return proposeJSON(req)
	}

	files := []string{"a.mkv", "b.mkv", "c.mkv"}
	main := &scriptedProvider{responses: []llm.Response{batchCall(files...), reply("done")}}
	workers := &workerProvider{reply: failB}
	a, st, sess := newBatchAgent(t, main, workers, tool.NewRegistry())

	if _, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("go")); err != nil {
		t.Fatalf("one failing item aborted the whole turn: %v", err)
	}

	report := lastReport(t, st, sess)
	if report.Summary.Failed != 1 || report.Summary.Proposed != 2 {
		t.Errorf("summary = %+v, want 2 proposed and 1 failed", report.Summary)
	}
	for _, p := range report.Proposals {
		if p.File == "b.mkv" {
			if p.Error == "" {
				t.Error("the failing item reported no error")
			}
			if p.Proposed != "" {
				t.Errorf("a failed item carries a proposal %q — a retry must not leave half an answer", p.Proposed)
			}
		}
	}
}

func TestBatchToolIsAbsentWithoutAPool(t *testing.T) {
	main := &scriptedProvider{responses: []llm.Response{reply("ok")}}
	a, _, sess := newBatchAgent(t, main, nil, tool.NewRegistry())

	if _, err := a.Advance(context.Background(), sess, nil, llm.UserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	for _, def := range main.requests[0].Tools {
		if def.Name == "batch_propose_names" {
			t.Error("the batch tool was offered although no pool is configured")
		}
	}
}

func TestBatchRejectsOversizedAndEmptyInput(t *testing.T) {
	main := &scriptedProvider{responses: []llm.Response{reply("ok")}}
	workers := &workerProvider{reply: proposeJSON}
	a, _, sess := newBatchAgent(t, main, workers, tool.NewRegistry())

	h := a.batchHandler(sess)

	if _, err := h(context.Background(), json.RawMessage(`{"files":[]}`)); err == nil {
		t.Error("an empty file list was accepted")
	}

	too := make([]string, maxBatchItems+1)
	for i := range too {
		too[i] = "f.mkv"
	}
	in, _ := json.Marshal(batchInput{Files: too})
	if _, err := h(context.Background(), in); err == nil {
		t.Errorf("a batch of %d files was accepted, above the %d limit", len(too), maxBatchItems)
	}
}

func lastReport(t *testing.T, st *store.Store, sess *store.Session) BatchReport {
	t.Helper()
	msgs, err := st.Messages(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var report BatchReport
	for _, m := range msgs {
		if m.Role != llm.RoleTool {
			continue
		}
		var candidate BatchReport
		if err := json.Unmarshal([]byte(m.Content), &candidate); err == nil && candidate.Proposals != nil {
			report = candidate
		}
	}
	if report.Proposals == nil {
		t.Fatal("no batch report in the transcript")
	}
	return report
}

func TestParseProposal(t *testing.T) {
	tests := []struct {
		name     string
		reply    string
		proposed string
		skip     bool
		raw      bool
	}{
		{
			name:     "plain object",
			reply:    `{"proposed_name":"Show - S01E01 - Pilot.mkv","reason":"tmdb"}`,
			proposed: "Show - S01E01 - Pilot.mkv",
		},
		{
			name:     "wrapped in a code fence with prose",
			reply:    "Here you go:\n```json\n{\"proposed_name\": \"A.mkv\", \"reason\": \"x\"}\n```\nHope that helps!",
			proposed: "A.mkv",
		},
		{
			name:  "skip",
			reply: `{"skip":true,"reason":"no match"}`,
			skip:  true,
		},
		{
			// A worker that returns a path has ignored its instructions;
			// renames happen in place, so only the final element survives.
			name:     "path is reduced to a bare filename",
			reply:    `{"proposed_name":"/mnt/media/tv/A.mkv","reason":"x"}`,
			proposed: "A.mkv",
		},
		{
			name:     "windows path",
			reply:    `{"proposed_name":"D:\\media\\A.mkv","reason":"x"}`,
			proposed: "A.mkv",
		},
		{
			// Braces inside a value must not end the object early.
			name:     "braces inside a string",
			reply:    `{"proposed_name":"Show {2019}.mkv","reason":"x"}`,
			proposed: "Show {2019}.mkv",
		},
		{
			name:  "unparseable reply is kept verbatim rather than dropped",
			reply: "I think it should be called Pilot.",
			raw:   true,
		},
		{
			name:  "object with neither a name nor a skip",
			reply: `{"reason":"unsure"}`,
			raw:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseProposal(tc.reply)
			if tc.raw {
				if got.Raw == "" {
					t.Errorf("Raw is empty; the worker's answer was discarded (got %+v)", got)
				}
				return
			}
			if got.Proposed != tc.proposed {
				t.Errorf("Proposed = %q, want %q", got.Proposed, tc.proposed)
			}
			if got.Skip != tc.skip {
				t.Errorf("Skip = %v, want %v", got.Skip, tc.skip)
			}
			if got.Raw != "" {
				t.Errorf("Raw = %q, want empty for a parseable reply", got.Raw)
			}
		})
	}

	if got := parseProposal("   "); got.Error == "" {
		t.Error("an empty reply produced no error")
	}
}
