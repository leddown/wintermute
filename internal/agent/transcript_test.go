package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wintermute/internal/llm"
	"wintermute/internal/store"
	"wintermute/internal/tool"
)

// countRows is deliberately a raw count over the whole table rather than a
// scoped read. The claim being tested is that nothing was written anywhere,
// and a helper that filtered by session id could not tell the difference
// between "no rows" and "rows this query did not think to look at".
func countRows(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An off-the-record conversation must leave nothing behind at all — asserted
// by counting rows before and after, not by trusting a flag.
func TestEphemeralConversationLeavesNoRows(t *testing.T) {
	ctx := context.Background()
	p := &scriptedProvider{responses: []llm.Response{reply("I won't write that down.")}}
	a, st, sess := newTestAgent(t, p, tool.NewRegistry())

	if err := a.SetMemory(ctx, sess, false, true); err != nil {
		t.Fatal(err)
	}
	sess.Record = false

	before := countRows(t, st, "messages")

	turn, err := a.Advance(ctx, sess, nil, llm.UserMessage("my card PIN is 4417"))
	if err != nil {
		t.Fatal(err)
	}
	if turn.Reply != "I won't write that down." {
		t.Errorf("reply = %q", turn.Reply)
	}

	if after := countRows(t, st, "messages"); after != before {
		t.Errorf("an ephemeral turn wrote %d message rows, want 0", after-before)
	}

	// The text itself must be nowhere in the database, whatever table it might
	// have found its way into.
	assertNotInDatabase(t, st, "4417")

	// And the conversation still worked: the model saw the full history.
	if len(p.requests) != 1 || len(p.requests[0].Messages) != 1 {
		t.Fatalf("model was shown %d messages, want 1", len(p.requests[0].Messages))
	}
	if p.requests[0].Messages[0].Content != "my card PIN is 4417" {
		t.Errorf("model was shown %q", p.requests[0].Messages[0].Content)
	}
}

// The session has to keep working normally across several turns while off the
// record: full history in memory, nothing on disk.
func TestEphemeralConversationKeepsItsHistory(t *testing.T) {
	ctx := context.Background()
	p := &scriptedProvider{responses: []llm.Response{reply("noted"), reply("still here")}}
	a, st, sess := newTestAgent(t, p, tool.NewRegistry())

	if err := a.SetMemory(ctx, sess, false, true); err != nil {
		t.Fatal(err)
	}
	sess.Record = false

	if _, err := a.Advance(ctx, sess, nil, llm.UserMessage("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Advance(ctx, sess, nil, llm.UserMessage("second")); err != nil {
		t.Fatal(err)
	}

	// Second call: user, assistant, user — the whole conversation so far.
	last := p.requests[len(p.requests)-1]
	if len(last.Messages) != 3 {
		t.Fatalf("second turn showed the model %d messages, want 3", len(last.Messages))
	}
	if last.Messages[0].Content != "first" || last.Messages[2].Content != "second" {
		t.Errorf("history out of order: %+v", last.Messages)
	}
	if n := countRows(t, st, "messages"); n != 0 {
		t.Errorf("%d message rows written, want 0", n)
	}
}

// Flipping mid-conversation must take the turns already written with it.
func TestFlipMidConversationErasesWhatWasWritten(t *testing.T) {
	ctx := context.Background()
	p := &scriptedProvider{responses: []llm.Response{reply("on the record"), reply("off the record")}}
	a, st, sess := newTestAgent(t, p, tool.NewRegistry())

	// A normal, recorded turn first.
	if _, err := a.Advance(ctx, sess, nil, llm.UserMessage("my passport number is 533914782")); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "messages"); n != 2 {
		t.Fatalf("recorded turn wrote %d rows, want 2", n)
	}

	// Now go off the record.
	if err := a.SetMemory(ctx, sess, false, true); err != nil {
		t.Fatal(err)
	}
	sess.Record = false

	if n := countRows(t, st, "messages"); n != 0 {
		t.Errorf("%d message rows survived going off the record, want 0", n)
	}
	assertNotInDatabase(t, st, "533914782")

	// The conversation continues with its context intact, from memory.
	if _, err := a.Advance(ctx, sess, nil, llm.UserMessage("and now?")); err != nil {
		t.Fatal(err)
	}
	last := p.requests[len(p.requests)-1]
	if len(last.Messages) != 3 {
		t.Fatalf("after the flip the model saw %d messages, want 3 (history carried into memory)",
			len(last.Messages))
	}
	if last.Messages[0].Content != "my passport number is 533914782" {
		t.Errorf("the pre-flip history was lost: %+v", last.Messages)
	}
	if n := countRows(t, st, "messages"); n != 0 {
		t.Errorf("%d rows written after going off the record, want 0", n)
	}
}

// Flipping back on must not retroactively commit what was said while off the
// record — the operator's stated preference, and the safer of the two.
func TestFlippingBackOnDoesNotCommitOffRecordTurns(t *testing.T) {
	ctx := context.Background()
	p := &scriptedProvider{responses: []llm.Response{reply("secret"), reply("public")}}
	a, st, sess := newTestAgent(t, p, tool.NewRegistry())

	if err := a.SetMemory(ctx, sess, false, true); err != nil {
		t.Fatal(err)
	}
	sess.Record = false
	if _, err := a.Advance(ctx, sess, nil, llm.UserMessage("said in confidence")); err != nil {
		t.Fatal(err)
	}

	// Back on the record.
	if err := a.SetMemory(ctx, sess, true, true); err != nil {
		t.Fatal(err)
	}
	sess.Record = true

	assertNotInDatabase(t, st, "said in confidence")
	if n := countRows(t, st, "messages"); n != 0 {
		t.Fatalf("flipping back committed %d rows, want 0", n)
	}

	// Turns from here on are recorded again.
	if _, err := a.Advance(ctx, sess, nil, llm.UserMessage("this one can be kept")); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "messages"); n != 2 {
		t.Errorf("the on-the-record turn wrote %d rows, want 2", n)
	}
	// But the model still sees the whole conversation, because memory holds it.
	last := p.requests[len(p.requests)-1]
	if len(last.Messages) != 3 {
		t.Errorf("model saw %d messages after resuming recording, want 3", len(last.Messages))
	}
	// And what is on disk is only the part that was on the record.
	assertNotInDatabase(t, st, "said in confidence")
	stored, err := st.Messages(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Content != "this one can be kept" {
		t.Errorf("stored transcript = %+v, want only the on-the-record turn", stored)
	}
}

// Actions are audited even off the record. Muninn records what was *done* to
// the operator's filesystem, not what was said about it, and an audit trail
// with holes wherever the conversation was private is not an audit trail.
func TestActionsAreAuditedEvenOffTheRecord(t *testing.T) {
	ctx := context.Background()
	reg := tool.NewRegistry()
	err := reg.Register(
		tool.Definition{Name: "probe_thing", Description: "lookup", Risk: tool.RiskRead},
		func(context.Context, json.RawMessage) (string, error) { return "found", nil },
	)
	if err != nil {
		t.Fatal(err)
	}

	p := &scriptedProvider{responses: []llm.Response{
		toolCall("probe_thing", `{"title":"x"}`),
		reply("done"),
	}}
	a, st, sess := newTestAgent(t, p, reg)

	if err := a.SetMemory(ctx, sess, false, true); err != nil {
		t.Fatal(err)
	}
	sess.Record = false

	if _, err := a.Advance(ctx, sess, nil, llm.UserMessage("look this up quietly")); err != nil {
		t.Fatal(err)
	}

	if n := countRows(t, st, "messages"); n != 0 {
		t.Errorf("%d message rows written off the record, want 0", n)
	}
	if n := countRows(t, st, "muninn"); n != 1 {
		t.Errorf("%d audit rows, want 1: actions are recorded even off the record", n)
	}
}

// assertNotInDatabase scans every text column of every table for a fragment.
// This is the assertion that actually means something: a row count proves a
// table is empty, but only a search proves the content is not somewhere else.
func assertNotInDatabase(t *testing.T, st *store.Store, needle string) {
	t.Helper()
	rows, err := st.DB().Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, table := range tables {
		cols, err := st.DB().Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				t.Fatal(err)
			}
			names = append(names, c)
		}
		cols.Close()

		for _, col := range names {
			var hits int
			q := `SELECT count(*) FROM "` + table + `" WHERE CAST("` + col + `" AS TEXT) LIKE ?`
			if err := st.DB().QueryRow(q, "%"+needle+"%").Scan(&hits); err != nil {
				continue // column type that cannot be matched as text
			}
			if hits > 0 {
				t.Errorf("%q found in %s.%s — it should not be anywhere in the database",
					needle, table, col)
			}
		}
	}
}

// The in-memory pool cannot be allowed to grow without bound in a process that
// runs for months.
func TestEphemeralEvictsByAgeAndCount(t *testing.T) {
	ctx := context.Background()
	e := NewEphemeral(2, time.Hour)
	now := time.Now()
	e.now = func() time.Time { return now }

	// Distinct times, so "least recently used" is unambiguous rather than a
	// tie the eviction has to break arbitrarily.
	for _, id := range []string{"a", "b"} {
		if err := e.Append(ctx, id, llm.UserMessage("hello")); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	// A third session evicts the least recently used.
	now = now.Add(time.Minute)
	if err := e.Append(ctx, "c", llm.UserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	if e.Has("a") {
		t.Error("least recently used session was not evicted")
	}
	if !e.Has("b") || !e.Has("c") {
		t.Error("a live session was evicted")
	}

	// Everything goes stale eventually.
	now = now.Add(2 * time.Hour)
	if err := e.Append(ctx, "d", llm.UserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	if e.Has("b") || e.Has("c") {
		t.Error("expired sessions were not dropped")
	}
}

// A recorded conversation must not touch the ephemeral pool at all — the
// ordinary path stays exactly as it was.
func TestRecordedConversationDoesNotUseMemory(t *testing.T) {
	ctx := context.Background()
	p := &scriptedProvider{responses: []llm.Response{reply("written down")}}
	a, st, sess := newTestAgent(t, p, tool.NewRegistry())

	if _, err := a.Advance(ctx, sess, nil, llm.UserMessage("keep this")); err != nil {
		t.Fatal(err)
	}
	if a.ephemeral.Has(sess.ID) {
		t.Error("a recorded conversation was held in the ephemeral pool")
	}
	if n := countRows(t, st, "messages"); n != 2 {
		t.Errorf("%d rows written, want 2", n)
	}
	if _, ok := a.transcriptFor(sess).(storeTranscript); !ok {
		t.Errorf("recorded session did not use the store transcript: %T", a.transcriptFor(sess))
	}
}

// stubRecaller stands in for the memory layer.
type stubRecaller struct {
	block   string
	queries []string
	// fail makes it behave as a broken retrieval: it returns nothing, which is
	// the contract — a failed recall is indistinguishable from an empty one.
	fail bool
}

func (r *stubRecaller) Framing() string { return "PRIOR-CONTEXT-FRAMING" }

func (r *stubRecaller) Recall(_ context.Context, _ *store.Session, query string) string {
	r.queries = append(r.queries, query)
	if r.fail {
		return ""
	}
	return r.block
}

// Retrieved context has to reach the model in front of the user's message, and
// the framing has to reach the system prompt — but neither may be written to
// the transcript, or the block would be stored, replayed, and eventually
// indexed as if the user had said it.
func TestRecalledContextReachesTheModelWithoutBeingStored(t *testing.T) {
	ctx := context.Background()
	p := &scriptedProvider{responses: []llm.Response{reply("under the stairs")}}
	rec := &stubRecaller{block: "<prior_context>the stopcock is under the stairs</prior_context>"}
	a, st, sess := newTestAgent(t, p, tool.NewRegistry())
	a = a.WithRecall(rec)

	if _, err := a.Advance(ctx, sess, nil, llm.UserMessage("where is the stopcock?")); err != nil {
		t.Fatal(err)
	}

	// Retrieved against the user's question.
	if len(rec.queries) != 1 || rec.queries[0] != "where is the stopcock?" {
		t.Errorf("recall queries = %v", rec.queries)
	}

	req := p.requests[0]
	// The framing is in the system prompt...
	if !strings.Contains(req.System, "PRIOR-CONTEXT-FRAMING") {
		t.Error("the framing did not reach the system prompt")
	}
	// ...and the block is in front of the user's own words, in their message.
	last := req.Messages[len(req.Messages)-1]
	if !strings.Contains(last.Content, "under the stairs") {
		t.Errorf("prior context did not reach the model: %q", last.Content)
	}
	if !strings.HasSuffix(last.Content, "where is the stopcock?") {
		t.Errorf("the user's message should follow the block, got %q", last.Content)
	}

	// And none of it was persisted.
	stored, err := st.Messages(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range stored {
		if strings.Contains(m.Content, "prior_context") {
			t.Errorf("the injected block was written to the transcript: %q", m.Content)
		}
	}
	if stored[0].Content != "where is the stopcock?" {
		t.Errorf("stored user message = %q, want the user's own words alone", stored[0].Content)
	}
}

// A conversation with recall switched off must not be retrieved for at all.
func TestRecallSwitchIsHonoured(t *testing.T) {
	ctx := context.Background()
	p := &scriptedProvider{responses: []llm.Response{reply("ok")}}
	rec := &stubRecaller{block: "<prior_context>something</prior_context>"}
	a, _, sess := newTestAgent(t, p, tool.NewRegistry())
	a = a.WithRecall(rec)

	if err := a.SetMemory(ctx, sess, true, false); err != nil {
		t.Fatal(err)
	}
	sess.Recall = false

	if _, err := a.Advance(ctx, sess, nil, llm.UserMessage("anything")); err != nil {
		t.Fatal(err)
	}
	if len(rec.queries) != 0 {
		t.Errorf("recall ran %d times for a session with recall off", len(rec.queries))
	}
	if strings.Contains(p.requests[0].Messages[0].Content, "prior_context") {
		t.Error("context was injected into a session with recall switched off")
	}
}

// Retrieval failing must leave the conversation working exactly as it would
// without memory at all.
func TestRetrievalFailureLeavesChatWorking(t *testing.T) {
	ctx := context.Background()
	p := &scriptedProvider{responses: []llm.Response{reply("answered anyway")}}
	rec := &stubRecaller{fail: true}
	a, _, sess := newTestAgent(t, p, tool.NewRegistry())
	a = a.WithRecall(rec)

	turn, err := a.Advance(ctx, sess, nil, llm.UserMessage("still there?"))
	if err != nil {
		t.Fatalf("a failed recall broke the turn: %v", err)
	}
	if turn.Reply != "answered anyway" {
		t.Errorf("reply = %q", turn.Reply)
	}
	// With nothing retrieved, the framing must not be added either — it would
	// tell the model to expect a block that is not there.
	if strings.Contains(p.requests[0].System, "PRIOR-CONTEXT-FRAMING") {
		t.Error("framing was added to the system prompt with no context to frame")
	}
}
