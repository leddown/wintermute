package recall

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"wintermute/internal/llm"
	"wintermute/internal/store"
	"wintermute/internal/store/storetest"
)

// fakeEmbedder produces deterministic vectors from a bag of words, so tests
// can assert on retrieval order without running a model.
//
// It is a real embedding in the only sense that matters here: texts sharing
// words point in similar directions, and texts sharing none do not.
type fakeEmbedder struct {
	name string
	dim  int
	// fail, when set, makes every call fail — for the degradation tests.
	fail error
	// calls counts requests, so a test can show the write path never waits on
	// this.
	calls int
}

func newFakeEmbedder() *fakeEmbedder { return &fakeEmbedder{name: "fake-embed-v1", dim: 64} }

func (f *fakeEmbedder) Name() string { return f.name }

func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	f.calls++
	if f.fail != nil {
		return nil, f.fail
	}
	out := make([][]float32, len(inputs))
	for i, text := range inputs {
		v := make([]float32, f.dim)
		for _, word := range strings.Fields(strings.ToLower(text)) {
			word = strings.Trim(word, ".,?!'\"")
			if word == "" {
				continue
			}
			var h uint32 = 2166136261
			for _, c := range []byte(word) {
				h ^= uint32(c)
				h *= 16777619
			}
			v[h%uint32(f.dim)] += 1
		}
		// Normalise so cosine is comparable regardless of message length.
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if norm > 0 {
			norm = math.Sqrt(norm)
			for j := range v {
				v[j] = float32(float64(v[j]) / norm)
			}
		}
		out[i] = v
	}
	return out, nil
}

type fixture struct {
	store    *store.Store
	recall   *Store
	indexer  *Indexer
	searcher *Searcher
	embedder *fakeEmbedder
	clientID int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := storetest.New(t)

	client, _, err := st.CreateClient(context.Background(), "owner", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}

	emb := newFakeEmbedder()
	rs := NewStore(st.DB())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &fixture{
		store:    st,
		recall:   rs,
		indexer:  NewIndexer(st.DB(), rs, emb, log),
		searcher: NewSearcher(st.DB(), rs, emb),
		embedder: emb,
		clientID: client.ID,
	}
}

// say writes a conversation turn and indexes it, returning the session.
func (f *fixture) say(t *testing.T, sessionID, backend, model, agentID string, msgs ...llm.Message) *store.Session {
	t.Helper()
	ctx := context.Background()
	sess, err := f.store.CreateSession(ctx, f.clientID, sessionID, backend, model, agentID, true)
	if err != nil {
		t.Fatal(err)
	}
	stamped := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		m.Backend, m.Model = backend, model
		stamped[i] = m
	}
	ids, err := f.store.AppendMessages(ctx, sess.ID, stamped...)
	if err != nil {
		t.Fatal(err)
	}
	f.indexer.Enqueue(ctx, ids)
	if _, err := f.indexer.DrainFully(ctx); err != nil {
		t.Fatal(err)
	}
	return sess
}

// The test that the whole feature exists for.
//
// A fact stated to one model, in one conversation, has to be retrievable in a
// brand new conversation held with a *different* model. The two sessions use
// different backends and different model identifiers, and nothing is shared
// between them except the store.
func TestFactStatedToModelAIsRecalledByModelB(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// Conversation one, against a local model.
	f.say(t, "with-model-a", "local", "qwen3:8b", "",
		llm.Message{Role: llm.RoleUser, Content: "The stopcock for the house is under the stairs, behind the coats."},
		llm.Message{Role: llm.RoleAssistant, Content: "Noted — stopcock under the stairs."},
	)

	// Conversation two, a fresh session against a different model entirely.
	sessB, err := f.store.CreateSession(ctx, f.clientID, "with-model-b", "claude", "claude-opus-5", "", true)
	if err != nil {
		t.Fatal(err)
	}

	hits, err := f.searcher.Search(ctx, "where is the stopcock?", Scope{
		ClientID:         f.clientID,
		ExcludeSessionID: sessB.ID,
	}, Options{TopK: 4, RecentTurns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("model B retrieved nothing; the fact stated to model A was not carried across")
	}

	var found bool
	for _, h := range hits {
		if strings.Contains(h.Content, "under the stairs") {
			found = true
			// And the provenance says which model it came from, which is what
			// makes the archive readable years later.
			if h.Model != "qwen3:8b" {
				t.Errorf("recalled hit names model %q, want qwen3:8b", h.Model)
			}
		}
	}
	if !found {
		t.Errorf("the stopcock fact was not among the hits: %+v", hits)
	}

	// And it renders into a block the other model can actually read.
	block := Render(hits, time.Now().UTC())
	if !strings.Contains(block, "under the stairs") {
		t.Error("rendered context does not contain the recalled fact")
	}
	if !strings.Contains(block, "<prior_context>") {
		t.Error("rendered context is not delimited")
	}
}

// Startup must refuse to run against an index built by a different embedder.
func TestStartupFailsOnEmbedderMismatch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	f.say(t, "pinned", "local", "qwen3:8b", "",
		llm.Message{Role: llm.RoleUser, Content: "pin the index with something"},
	)

	pin, err := f.recall.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pin == nil || pin.Model != "fake-embed-v1" {
		t.Fatalf("index was not pinned: %+v", pin)
	}
	if pin.Dimension != 64 {
		t.Errorf("pinned dimension = %d, want 64", pin.Dimension)
	}

	// The same embedder is fine.
	if err := f.recall.CheckPin(ctx, f.embedder); err != nil {
		t.Errorf("matching embedder rejected: %v", err)
	}

	// A different one is refused, loudly and with instructions.
	other := &fakeEmbedder{name: "some-other-embedder", dim: 64}
	err = f.recall.CheckPin(ctx, other)
	if !errors.Is(err, ErrEmbedderMismatch) {
		t.Fatalf("mismatched embedder gave %v, want ErrEmbedderMismatch", err)
	}
	for _, want := range []string{"fake-embed-v1", "some-other-embedder", "-reindex-memory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// A reindex is the deliberate way to change embedder, and it must not cost the
// messages the index was derived from.
func TestReindexClearsTheIndexButKeepsTheMessages(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	f.say(t, "history", "local", "qwen3:8b", "",
		llm.Message{Role: llm.RoleUser, Content: "the boiler was serviced in March"},
	)

	if err := f.recall.ClearIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countTable(t, f, "recall_vectors"); n != 0 {
		t.Errorf("%d vectors survived the reindex, want 0", n)
	}
	if n := countTable(t, f, "messages"); n == 0 {
		t.Fatal("reindex destroyed the messages it was derived from")
	}
	if pin, _ := f.recall.Pin(ctx); pin != nil {
		t.Error("pin survived a reindex; the new embedder could not claim the index")
	}

	// Rebuilding with a different embedder now works and pins the new one.
	f.embedder.name = "replacement-embedder"
	queued, err := f.indexer.QueueEverything(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if queued == 0 {
		t.Fatal("backfill queued nothing")
	}
	if _, err := f.indexer.DrainFully(ctx); err != nil {
		t.Fatal(err)
	}
	pin, err := f.recall.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pin == nil || pin.Model != "replacement-embedder" {
		t.Errorf("pin after reindex = %+v, want replacement-embedder", pin)
	}
}

// Losing an embedding must never cost the message. The write path commits
// first and indexes afterwards.
func TestMessagesSurviveAnEmbedderOutage(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	f.embedder.fail = llm.ErrEmbedderUnavailable

	sess, err := f.store.CreateSession(ctx, f.clientID, "during-outage", "local", "qwen3:8b", "", true)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := f.store.AppendMessages(ctx, sess.ID,
		llm.Message{Role: llm.RoleUser, Content: "the meter reading is 41028"})
	if err != nil {
		t.Fatalf("the write path failed because the embedder was down: %v", err)
	}
	f.indexer.Enqueue(ctx, ids)

	// Indexing fails, and says so.
	if _, err := f.indexer.IndexBatch(ctx); !errors.Is(err, llm.ErrEmbedderUnavailable) {
		t.Errorf("IndexBatch error = %v, want ErrEmbedderUnavailable", err)
	}
	// The message is safe, and the work is still queued.
	if n := countTable(t, f, "messages"); n != 1 {
		t.Errorf("%d messages, want 1: the message must outlive a failed embedding", n)
	}
	backlog, err := f.indexer.Backlog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if backlog != 1 {
		t.Errorf("backlog = %d, want 1", backlog)
	}

	// When the embedder comes back, the backlog is picked up.
	f.embedder.fail = nil
	if _, err := f.indexer.DrainFully(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countTable(t, f, "recall_vectors"); n != 1 {
		t.Errorf("%d vectors after recovery, want 1", n)
	}
}

// Retrieval failure must not break normal chat.
func TestRetrievalDegradesRatherThanFailing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	f.say(t, "earlier", "local", "qwen3:8b", "",
		llm.Message{Role: llm.RoleUser, Content: "the spare key lives in the shed"},
	)

	// Embedder down: the lexical and recency rankers still answer.
	f.embedder.fail = llm.ErrEmbedderUnavailable
	hits, err := f.searcher.Search(ctx, "where is the spare key", Scope{ClientID: f.clientID},
		Options{TopK: 4, RecentTurns: 2})
	if err != nil {
		t.Fatalf("search returned an error instead of degrading: %v", err)
	}
	if len(hits) == 0 {
		t.Error("with the embedder down, lexical and recency retrieval should still find the message")
	}

	// An empty index is simply no context, not an error.
	empty := newFixture(t)
	hits, err = empty.searcher.Search(ctx, "anything at all", Scope{ClientID: empty.clientID},
		Options{TopK: 4, RecentTurns: 2})
	if err != nil {
		t.Errorf("searching an empty index errored: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("empty index returned %d hits", len(hits))
	}
}

// Injected context must stay inside its budget however much history exists.
func TestInjectionStaysWithinBudget(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	long := strings.Repeat("the quick brown fox jumped over the lazy dog. ", 40)
	for i := range 25 {
		f.say(t, "bulk", "local", "qwen3:8b", "",
			llm.Message{Role: llm.RoleUser, Content: long + " entry " + string(rune('a'+i))},
		)
	}

	const budget = 400
	hits, err := f.searcher.Search(ctx, "quick brown fox", Scope{ClientID: f.clientID},
		Options{TopK: 20, RecentTurns: 10, TokenBudget: budget})
	if err != nil {
		t.Fatal(err)
	}

	var total int
	for _, h := range hits {
		total += EstimateTokens(h.Content)
	}
	if total > budget {
		t.Errorf("retrieved %d estimated tokens, over the %d budget", total, budget)
	}
	if len(hits) == 0 {
		t.Error("budget truncated everything; at least the best hit should survive")
	}
}

// The budget is a fraction of the answering model's context window, so a small
// local model is not handed the same block as a large one.
func TestBudgetScalesWithContextWindow(t *testing.T) {
	small := BudgetFor(8192, 0.12, 1500)
	large := BudgetFor(200000, 0.12, 1500)
	if small >= large {
		t.Errorf("budget did not scale: %d for 8k, %d for 200k", small, large)
	}
	if small > 8192/2 {
		t.Errorf("budget of %d is more than half an 8k window", small)
	}
	// Unknown context length falls back rather than guessing.
	if got := BudgetFor(0, 0.12, 1500); got != 1500 {
		t.Errorf("unknown context length gave %d, want the 1500 fallback", got)
	}
}

// Deleting a conversation must leave nothing retrievable — asserted by
// searching for the content, not only by counting rows.
func TestDeletingAConversationLeavesNothingRetrievable(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	sess := f.say(t, "doomed", "local", "qwen3:8b", "",
		llm.Message{Role: llm.RoleUser, Content: "my national insurance number is QQ123456C"},
		llm.Message{Role: llm.RoleAssistant, Content: "noted"},
	)

	// It is retrievable to begin with, or the test proves nothing.
	hits, err := f.searcher.Search(ctx, "national insurance number", Scope{ClientID: f.clientID},
		Options{TopK: 5, RecentTurns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("setup failed: the content was not retrievable before deletion")
	}

	if err := f.store.DeleteSession(ctx, sess.ID, f.clientID); err != nil {
		t.Fatal(err)
	}

	// The real assertion: search for it and get nothing.
	hits, err = f.searcher.Search(ctx, "national insurance number QQ123456C", Scope{ClientID: f.clientID},
		Options{TopK: 5, RecentTurns: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if strings.Contains(h.Content, "QQ123456C") {
			t.Errorf("deleted content is still retrievable: %+v", h)
		}
	}

	// And the derived rows went with it, including the lexical index, which
	// has no foreign key and relies on a trigger.
	for _, table := range []string{"recall_vectors", "messages"} {
		if n := countTable(t, f, table); n != 0 {
			t.Errorf("%s still holds %d rows after deleting the conversation", table, n)
		}
	}
	var ftsRows int
	if err := f.store.DB().QueryRow(
		`SELECT count(*) FROM recall_fts WHERE recall_fts MATCH 'insurance'`).Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if ftsRows != 0 {
		t.Errorf("the lexical index still holds %d entries for the deleted conversation", ftsRows)
	}
}

func countTable(t *testing.T, f *fixture, table string) int {
	t.Helper()
	var n int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The master switch sits above the per-session flag: when it is off, nothing
// is recalled anywhere, whatever an individual conversation asks for.
func TestMasterSwitchDefaultsOnAndPersists(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// An install that has never touched the switch behaves as it always did.
	if !f.recall.Enabled(ctx) {
		t.Error("shared memory defaulted to off; an existing install must behave as before")
	}

	if err := f.recall.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if f.recall.Enabled(ctx) {
		t.Error("switch did not take effect")
	}
	if err := f.recall.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	if !f.recall.Enabled(ctx) {
		t.Error("switch did not turn back on")
	}
}

// Turning recall off must not stop the store recording. Switching it back on
// should not reveal a hole covering however long it was off.
func TestMasterSwitchDoesNotStopIndexing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	if err := f.recall.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.say(t, "while-off", "local", "qwen3:8b", "",
		llm.Message{Role: llm.RoleUser, Content: "the fuse box is in the garage"},
	)
	if n := countTable(t, f, "recall_vectors"); n == 0 {
		t.Error("nothing was indexed while recall was off; turning it back on would leave a gap")
	}

	// And once it is back on, that period is retrievable.
	if err := f.recall.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	hits, err := f.searcher.Search(ctx, "where is the fuse box", Scope{ClientID: f.clientID},
		Options{TopK: 3, RecentTurns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Error("what was said while recall was off is not retrievable now that it is on")
	}
}

// Clearing the index must be the reversible operation: conversations stay, and
// a backfill rebuilds what was thrown away.
func TestClearIndexIsReversible(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	f.say(t, "kept", "local", "qwen3:8b", "",
		llm.Message{Role: llm.RoleUser, Content: "the bins go out on Tuesday"},
	)

	if err := f.recall.ClearIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countTable(t, f, "messages"); n != 1 {
		t.Fatalf("clearing the index took %d messages with it", 1-n)
	}

	if _, err := f.indexer.QueueEverything(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.indexer.DrainFully(ctx); err != nil {
		t.Fatal(err)
	}
	hits, err := f.searcher.Search(ctx, "when do the bins go out", Scope{ClientID: f.clientID},
		Options{TopK: 3, RecentTurns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Error("the index did not come back after a backfill")
	}
}

// Wiping the store is the irreversible one, and it has to reach everything —
// asserted by searching for the content afterwards, not only by counting rows.
func TestForgetEverythingLeavesNothing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	sess := f.say(t, "rubbish", "local", "qwen3:8b", "",
		llm.Message{Role: llm.RoleUser, Content: "test test my card number is 4111111111111111"},
		llm.Message{Role: llm.RoleAssistant, Content: "noted"},
	)
	if err := f.store.RecordTool(ctx, store.AuditEntry{
		SessionID: sess.ID, CallID: "c1", ToolName: "rename_file",
		Side: "client", Risk: "write", Decision: store.DecisionApproved,
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := f.recall.ForgetEverything(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d sessions, want 1", deleted)
	}

	for _, table := range []string{"sessions", "messages", "recall_vectors", "recall_queue", "muninn"} {
		if n := countTable(t, f, table); n != 0 {
			t.Errorf("%s still holds %d rows after the wipe", table, n)
		}
	}

	// The real assertion: it is not retrievable any more.
	hits, err := f.searcher.Search(ctx, "card number 4111111111111111", Scope{ClientID: f.clientID},
		Options{TopK: 5, RecentTurns: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("wiped content is still retrievable: %+v", hits)
	}
	var ftsRows int
	if err := f.store.DB().QueryRow(
		`SELECT count(*) FROM recall_fts WHERE recall_fts MATCH 'card'`).Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if ftsRows != 0 {
		t.Errorf("the lexical index still holds %d entries after the wipe", ftsRows)
	}

	// The pin goes with it. With no vectors left there is nothing for it to
	// describe, and keeping it would refuse a change of embedder over an empty
	// index — which is exactly what someone does after clearing out a new
	// install.
	pin, err := f.recall.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pin != nil {
		t.Errorf("the embedder pin survived the wipe: %+v", pin)
	}
	other := &fakeEmbedder{name: "a-fresh-choice", dim: 128}
	if err := f.recall.CheckPin(ctx, other); err != nil {
		t.Errorf("a wiped store refused a new embedder: %v", err)
	}

	// Clients survive: wiping conversations must not log the operator out of
	// their own server.
	var clients int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM clients`).Scan(&clients); err != nil {
		t.Fatal(err)
	}
	if clients == 0 {
		t.Error("the wipe deleted the registered clients along with the conversations")
	}
}
