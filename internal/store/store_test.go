package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"wintermute/internal/llm"
	"wintermute/internal/tool"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Migrations must be idempotent: the server applies them on every start.
func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening an already-migrated database failed: %v", err)
	}
	second.Close()
}

func TestClientTokens(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	client, token, err := st.CreateClient(ctx, "laptop", KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("no token returned")
	}

	t.Run("resolves a valid token", func(t *testing.T) {
		got, err := st.ClientByToken(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != client.ID || got.Name != "laptop" {
			t.Errorf("got %+v, want %+v", got, client)
		}
	})

	t.Run("rejects an unknown or empty token", func(t *testing.T) {
		for _, bad := range []string{"", "wm_nope", token + "x"} {
			if _, err := st.ClientByToken(ctx, bad); !errors.Is(err, ErrNotFound) {
				t.Errorf("ClientByToken(%q) = %v, want ErrNotFound", bad, err)
			}
		}
	})

	// The plaintext token must never be recoverable from the database.
	t.Run("stores only a hash", func(t *testing.T) {
		var stored string
		if err := st.DB().QueryRow(`SELECT token_hash FROM clients WHERE id = ?`, client.ID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored == token {
			t.Error("token stored in plaintext")
		}
	})

	t.Run("names are unique", func(t *testing.T) {
		if _, _, err := st.CreateClient(ctx, "laptop", KindHarness); !errors.Is(err, ErrDuplicate) {
			t.Errorf("duplicate name = %v, want ErrDuplicate", err)
		}
	})

	t.Run("revocation invalidates the token", func(t *testing.T) {
		if err := st.DeleteClient(ctx, "laptop"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClientByToken(ctx, token); !errors.Is(err, ErrNotFound) {
			t.Errorf("revoked token still resolves: %v", err)
		}
		if err := st.DeleteClient(ctx, "laptop"); !errors.Is(err, ErrNotFound) {
			t.Errorf("second delete = %v, want ErrNotFound", err)
		}
	})
}

// A session must be invisible to any client other than its owner, and report
// ErrNotFound rather than a permission error so nothing is leaked.
func TestSessionsAreScopedToTheirClient(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	owner, _, err := st.CreateClient(ctx, "owner", KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := st.CreateClient(ctx, "other", KindBrowser)
	if err != nil {
		t.Fatal(err)
	}

	sess, err := st.CreateSession(ctx, owner.ID, "mine", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.Session(ctx, sess.ID, owner.ID); err != nil {
		t.Fatalf("owner cannot read own session: %v", err)
	}
	if _, err := st.Session(ctx, sess.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-client read = %v, want ErrNotFound", err)
	}

	sessions, err := st.ListSessions(ctx, other.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("other client sees %d sessions, want 0", len(sessions))
	}
}

// Deleting a session is destructive and client-scoped, so both halves are
// worth pinning: another client's delete must not reach it, and the owner's
// must take the transcript and the audit rows with it rather than orphaning
// them. The cascade only fires because store.Open sets foreign_keys(ON) — if
// that pragma is ever dropped, this test is what notices.
func TestDeleteSessionIsScopedAndCascades(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	owner, _, err := st.CreateClient(ctx, "owner", KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := st.CreateClient(ctx, "other", KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(ctx, owner.ID, "doomed", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessages(ctx, sess.ID, llm.UserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordTool(ctx, AuditEntry{
		SessionID: sess.ID, CallID: "1", ToolName: "rename_file",
		Side: tool.SideClient, Risk: tool.RiskWrite, Decision: DecisionApproved,
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteSession(ctx, sess.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-client delete = %v, want ErrNotFound", err)
	}
	if _, err := st.Session(ctx, sess.ID, owner.ID); err != nil {
		t.Fatalf("session gone after a refused cross-client delete: %v", err)
	}

	if err := st.DeleteSession(ctx, sess.ID, owner.ID); err != nil {
		t.Fatalf("owner cannot delete own session: %v", err)
	}
	if _, err := st.Session(ctx, sess.ID, owner.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("read after delete = %v, want ErrNotFound", err)
	}
	if err := st.DeleteSession(ctx, sess.ID, owner.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}

	for _, table := range []string{"messages", "muninn"} {
		var n int
		if err := st.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE session_id = ?`, sess.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s left %d orphaned rows, want 0", table, n)
		}
	}
}

func TestTranscriptRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	client, _, err := st.CreateClient(ctx, "c", KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(ctx, client.ID, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	want := []llm.Message{
		llm.UserMessage("rename these"),
		{
			Role:    llm.RoleAssistant,
			Content: "Looking it up.",
			Thinking: []json.RawMessage{
				json.RawMessage(`{"type":"thinking","thinking":"check the title","signature":"sig"}`),
			},
			ToolCalls: []tool.Call{
				{ID: "call_1", Name: "lookup_metadata", Input: json.RawMessage(`{"kind":"movie"}`)},
			},
		},
		llm.ToolMessage(tool.Result{CallID: "call_1", Content: "no matches", IsError: true}),
	}
	if _, err := st.AppendMessages(ctx, sess.ID, want...); err != nil {
		t.Fatal(err)
	}

	got, err := st.Messages(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	// Order matters: the transcript is replayed to the model verbatim.
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Content != want[i].Content {
			t.Errorf("message %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].Name != "lookup_metadata" {
		t.Errorf("tool calls did not survive the round trip: %+v", got[1].ToolCalls)
	}
	if !got[2].IsError || got[2].ToolCallID != "call_1" {
		t.Errorf("tool result metadata lost: %+v", got[2])
	}
	// Thinking must come back byte-identical: the Messages API validates the
	// block on the next turn, so a re-encoded one is not good enough.
	if len(got[1].Thinking) != 1 {
		t.Fatalf("thinking did not survive the round trip: %+v", got[1].Thinking)
	}
	if string(got[1].Thinking[0]) != string(want[1].Thinking[0]) {
		t.Errorf("thinking = %s, want %s", got[1].Thinking[0], want[1].Thinking[0])
	}
}

func TestAuditTrail(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	client, _, err := st.CreateClient(ctx, "c", KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(ctx, client.ID, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	entries := []AuditEntry{
		{SessionID: sess.ID, CallID: "1", ToolName: "list_directory", Side: tool.SideClient, Risk: tool.RiskRead, Decision: DecisionAuto},
		{SessionID: sess.ID, CallID: "2", ToolName: "rename_file", Side: tool.SideClient, Risk: tool.RiskWrite, Decision: DecisionDenied},
	}
	for _, e := range entries {
		if err := st.RecordTool(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.AuditForSession(ctx, sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	// Newest first.
	if got[0].CallID != "2" || got[0].Decision != DecisionDenied {
		t.Errorf("first entry = %+v, want the denied rename", got[0])
	}
}

// A large tool result must not be stored unbounded in the audit trail.
func TestAuditOutcomeIsTruncated(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	client, _, err := st.CreateClient(ctx, "c", KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(ctx, client.ID, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	huge := make([]byte, 20000)
	for i := range huge {
		huge[i] = 'x'
	}
	err = st.RecordTool(ctx, AuditEntry{
		SessionID: sess.ID, CallID: "1", ToolName: "list_directory",
		Side: tool.SideClient, Risk: tool.RiskRead, Decision: DecisionAuto,
		Outcome: string(huge),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.AuditForSession(ctx, sess.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Outcome) >= len(huge) {
		t.Errorf("outcome length = %d, want it truncated", len(got[0].Outcome))
	}
}

// The rename in 0012_muninn.sql has to carry the audit trail across intact.
// This is the one table whose whole purpose is to still be there afterwards,
// so the test builds a genuinely pre-0012 database — migrations 0001 through
// 0011, applied in order, with a row written through the old name — and then
// opens it normally to let 0012 run.
func TestMuninnRenamePreservesAuditTrail(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	// Build the database as it stood before this migration existed.
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && e.Name() < "0012" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no pre-0012 migrations found")
	}
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}

	// A client, a session and one audit row, written through the old name.
	now := time.Now().UTC()
	if _, err := raw.Exec(`INSERT INTO clients (name, token_hash, kind, created_at)
		VALUES ('harness', 'hash', 'desktop', ?)`, now); err != nil {
		t.Fatal(err)
	}
	var clientID int64
	if err := raw.QueryRow(`SELECT id FROM clients WHERE name = 'harness'`).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO sessions (id, client_id, title, created_at, updated_at)
		VALUES ('sess-1', ?, 'before the rename', ?, ?)`, clientID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO tool_audit
		(session_id, call_id, tool_name, side, risk, input, decision, outcome, is_error, created_at)
		VALUES ('sess-1', 'call-1', 'rename_file', 'client', 'write', '{"path":"x"}', 'denied', 'user refused', 0, ?)`,
		now); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening normally applies 0012 and nothing else.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("migrating across the rename failed: %v", err)
	}
	defer st.Close()

	// The old name must be gone, or two tables would be accepting writes.
	var oldTables int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'tool_audit'`).Scan(&oldTables); err != nil {
		t.Fatal(err)
	}
	if oldTables != 0 {
		t.Error("tool_audit still exists after the rename")
	}

	// The row has to have survived with every column intact: a rename that
	// silently dropped the decision would leave an audit trail that no longer
	// records whether the user said yes.
	entriesOut, err := st.AuditForSession(ctx, "sess-1", 10)
	if err != nil {
		t.Fatalf("read audit after rename: %v", err)
	}
	if len(entriesOut) != 1 {
		t.Fatalf("audit rows after rename = %d, want 1", len(entriesOut))
	}
	got := entriesOut[0]
	if got.CallID != "call-1" || got.ToolName != "rename_file" ||
		got.Decision != "denied" || got.Outcome != "user refused" ||
		string(got.Side) != "client" || string(got.Risk) != "write" ||
		got.Input != `{"path":"x"}` {
		t.Errorf("audit row changed across the rename: %+v", got)
	}

	// And new writes must land in the renamed table alongside the old rows.
	if err := st.RecordTool(ctx, AuditEntry{
		SessionID: "sess-1", CallID: "call-2", ToolName: "list_dir",
		Side: tool.SideClient, Risk: tool.RiskRead, Decision: DecisionAuto,
	}); err != nil {
		t.Fatalf("record after rename: %v", err)
	}
	entriesOut, err = st.AuditForSession(ctx, "sess-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesOut) != 2 {
		t.Errorf("audit rows after a new write = %d, want 2", len(entriesOut))
	}
}

// Provenance has to survive the round trip, or the archive cannot say which
// model wrote which line — the one thing it can never reconstruct later.
func TestMessageProvenanceRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sess := newTestSession(t, st)

	want := []llm.Message{
		{Role: llm.RoleUser, Content: "which model am I talking to?",
			Backend: "local", Model: "qwen3:8b"},
		{Role: llm.RoleAssistant, Content: "a local one",
			Backend: "local", Model: "qwen3:8b", TokenCount: 42},
		// The same conversation continued against a different model, which is
		// exactly what SetSessionModel exists to allow.
		{Role: llm.RoleAssistant, Content: "and now a cloud one",
			Backend: "claude", Model: "claude-opus-5", TokenCount: 17},
	}
	if _, err := st.AppendMessages(ctx, sess.ID, want...); err != nil {
		t.Fatal(err)
	}

	got, err := st.Messages(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Backend != want[i].Backend || got[i].Model != want[i].Model ||
			got[i].TokenCount != want[i].TokenCount {
			t.Errorf("message %d provenance = %q/%q/%d, want %q/%q/%d", i,
				got[i].Backend, got[i].Model, got[i].TokenCount,
				want[i].Backend, want[i].Model, want[i].TokenCount)
		}
	}
}

// A new conversation is on the record and recalling. Ephemeral is never a
// state a session arrives in.
func TestNewSessionIsOnTheRecord(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sess := newTestSession(t, st)

	if !sess.Record || !sess.Recall {
		t.Errorf("new session record/recall = %v/%v, want true/true", sess.Record, sess.Recall)
	}
	// And as persisted, not just as returned.
	reread, err := st.Session(ctx, sess.ID, sess.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	if !reread.Record || !reread.Recall {
		t.Errorf("stored record/recall = %v/%v, want true/true", reread.Record, reread.Recall)
	}
}

// The switches are independent: drawing on history while leaving no trace is a
// combination the operator is entitled to ask for.
func TestMemorySwitchesAreIndependent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sess := newTestSession(t, st)

	if err := st.SetSessionMemory(ctx, sess.ID, sess.ClientID, false, true); err != nil {
		t.Fatal(err)
	}
	got, err := st.Session(ctx, sess.ID, sess.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Record || !got.Recall {
		t.Errorf("record/recall = %v/%v, want false/true", got.Record, got.Recall)
	}
}

// Flipping a conversation off the record mid-stream has to take the turns
// already written with it. A partial transcript of a conversation the operator
// has just declared private is the failure this switch exists to prevent.
func TestFlipToEphemeralPurgesWhatWasWritten(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sess := newTestSession(t, st)

	if _, err := st.AppendMessages(ctx, sess.ID,
		llm.UserMessage("my NHS number is 943 476 5919"),
		llm.Message{Role: llm.RoleAssistant, Content: "noted"},
	); err != nil {
		t.Fatal(err)
	}
	if n := countMessages(t, st, sess.ID); n != 2 {
		t.Fatalf("setup wrote %d messages, want 2", n)
	}

	if err := st.SetSessionMemory(ctx, sess.ID, sess.ClientID, false, true); err != nil {
		t.Fatal(err)
	}
	if n := countMessages(t, st, sess.ID); n != 0 {
		t.Errorf("after going off the record %d messages remain, want 0", n)
	}

	// Flipping back must not resurrect them: turns exchanged off the record
	// stay unrecorded rather than being retroactively committed.
	if err := st.SetSessionMemory(ctx, sess.ID, sess.ClientID, true, true); err != nil {
		t.Fatal(err)
	}
	if n := countMessages(t, st, sess.ID); n != 0 {
		t.Errorf("flipping back restored %d messages, want 0", n)
	}
}

// The switches are session state, so they answer to the same client scoping as
// everything else: one client must not be able to flip another's conversation.
func TestSetSessionMemoryIsScopedToOwner(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sess := newTestSession(t, st)

	other, _, err := st.CreateClient(ctx, "intruder", "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionMemory(ctx, sess.ID, other.ID, false, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-client flip = %v, want ErrNotFound", err)
	}
	got, err := st.Session(ctx, sess.ID, sess.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Record {
		t.Error("another client's flip took effect")
	}
}

func countMessages(t *testing.T, st *Store, sessionID string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// newTestSession makes a client and one session it owns, which is the setup
// every memory test needs before it can say anything interesting.
func newTestSession(t *testing.T, st *Store) *Session {
	t.Helper()
	ctx := context.Background()
	client, _, err := st.CreateClient(ctx, "owner-"+t.Name(), KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(ctx, client.ID, "test", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// Notes and champions are the operator's own judgements, and the whole reason
// they are separate tables is that the catalog is a probe cache. This is the
// property that matters: a refresh must not wipe an opinion.
func TestModelJudgementsSurviveACatalogRefresh(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.UpsertBackend(ctx, BackendRow{Name: "local", Kind: "ollama", Status: BackendOK}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceCatalog(ctx, "local", []CatalogRow{
		{Backend: "local", ModelID: "qwen3:8b", ParamsB: 8},
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.SetModelNote(ctx, "qwen3:8b", "Current best coding."); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChampion(ctx, "coding", "qwen3:8b"); err != nil {
		t.Fatal(err)
	}

	// A probe sweep rewrites every catalog row.
	if err := st.ReplaceCatalog(ctx, "local", []CatalogRow{
		{Backend: "local", ModelID: "qwen3:8b", ParamsB: 8, Loaded: true},
	}); err != nil {
		t.Fatal(err)
	}

	notes, err := st.ModelNotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := notes["qwen3:8b"].Note; got != "Current best coding." {
		t.Errorf("note after a refresh = %q, want it intact", got)
	}
	champions, err := st.Champions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(champions) != 1 || champions[0].ModelID != "qwen3:8b" {
		t.Errorf("champions after a refresh = %+v", champions)
	}
}

// Naming a new champion replaces the old one rather than adding a second, so
// there is never a moment with two and never a stale pointer left behind.
func TestChampionIsAMovingPointer(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.SetChampion(ctx, "coding", "qwen3:8b"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChampion(ctx, "coding", "devstral:24b"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChampion(ctx, "general", "qwen3:8b"); err != nil {
		t.Fatal(err)
	}

	champions, err := st.Champions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(champions) != 2 {
		t.Fatalf("got %d champions, want 2 (one per task): %+v", len(champions), champions)
	}
	byTask := map[string]string{}
	for _, c := range champions {
		byTask[c.Task] = c.ModelID
	}
	if byTask["coding"] != "devstral:24b" {
		t.Errorf("coding champion = %q, want the model that replaced it", byTask["coding"])
	}
	if byTask["general"] != "qwen3:8b" {
		t.Errorf("general champion = %q", byTask["general"])
	}

	// A model can hold more than one title at once.
	if err := st.SetChampion(ctx, "coding", "qwen3:8b"); err != nil {
		t.Fatal(err)
	}
	champions, err = st.Champions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(champions) != 2 {
		t.Errorf("got %d champions, want 2", len(champions))
	}
}

// Clearing is expressed as an empty value rather than a separate verb, so
// "no note" and "no champion" are one state each.
func TestEmptyValueClearsAJudgement(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.SetModelNote(ctx, "qwen3:8b", "temporary"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelNote(ctx, "qwen3:8b", "   "); err != nil {
		t.Fatal(err)
	}
	notes, err := st.ModelNotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := notes["qwen3:8b"]; ok {
		t.Error("a blank note was stored instead of clearing the row")
	}

	if err := st.SetChampion(ctx, "coding", "qwen3:8b"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChampion(ctx, "coding", ""); err != nil {
		t.Fatal(err)
	}
	champions, err := st.Champions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(champions) != 0 {
		t.Errorf("clearing left %+v", champions)
	}
}

// The same model on several hosts reports the same id, so one note covers all
// of them — and case differences must not split it in two.
func TestNoteKeyFoldsCase(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.SetModelNote(ctx, "Qwen3:8B", "first"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelNote(ctx, "qwen3:8b", "second"); err != nil {
		t.Fatal(err)
	}
	notes, err := st.ModelNotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("case difference created %d notes, want 1: %+v", len(notes), notes)
	}
	if notes["qwen3:8b"].Note != "second" {
		t.Errorf("note = %q, want the later write to have won", notes["qwen3:8b"].Note)
	}
}

// Performance is summarised from summed tokens over summed time, not from an
// average of per-call rates — averaging rates weights a two-token reply the
// same as a two-thousand-token one.
func TestModelPerformanceSummarises(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := st.RecordInference(ctx, []InferenceSample{
		// 100 tokens in 1s, then 300 tokens in 3s: 400 tokens over 4s = 100/s.
		{Backend: "rig", Model: "qwen3:8b", CompletionTokens: 100, PromptTokens: 10,
			DurationMS: 1000, CreatedAt: now.Add(-time.Minute)},
		{Backend: "rig", Model: "qwen3:8b", CompletionTokens: 300, PromptTokens: 20,
			DurationMS: 3000, CreatedAt: now.Add(-time.Minute)},
		// A failure: counted, but kept out of the timing so a backend that
		// refuses quickly does not look fast.
		{Backend: "rig", Model: "qwen3:8b", Failed: true, DurationMS: 50, CreatedAt: now},
		// A different model, so the grouping has something to separate.
		{Backend: "rig", Model: "devstral:24b", CompletionTokens: 50,
			DurationMS: 5000, CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	perf, err := st.ModelPerformanceSince(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	byModel := map[string]ModelPerformance{}
	for _, p := range perf {
		byModel[p.Model] = p
	}

	qwen := byModel["qwen3:8b"]
	if qwen.Calls != 3 || qwen.Failed != 1 {
		t.Errorf("calls/failed = %d/%d, want 3/1", qwen.Calls, qwen.Failed)
	}
	if qwen.TokensPerSecond < 99 || qwen.TokensPerSecond > 101 {
		t.Errorf("tokens/sec = %.2f, want ~100 (400 tokens over 4s)", qwen.TokensPerSecond)
	}
	if qwen.CompletionTokens != 400 {
		t.Errorf("completion tokens = %d, want 400", qwen.CompletionTokens)
	}
	// The failed call's 50ms must not have become the slowest successful one.
	if qwen.SlowestMS != 3000 {
		t.Errorf("slowest = %dms, want 3000", qwen.SlowestMS)
	}

	dev := byModel["devstral:24b"]
	if dev.TokensPerSecond < 9 || dev.TokensPerSecond > 11 {
		t.Errorf("devstral tokens/sec = %.2f, want ~10", dev.TokensPerSecond)
	}
}

// A window must not pick up calls from before it.
func TestModelPerformanceRespectsTheWindow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := st.RecordInference(ctx, []InferenceSample{
		{Backend: "rig", Model: "old", CompletionTokens: 100, DurationMS: 1000,
			CreatedAt: now.AddDate(0, 0, -30)},
		{Backend: "rig", Model: "recent", CompletionTokens: 100, DurationMS: 1000,
			CreatedAt: now.Add(-time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	perf, err := st.ModelPerformanceSince(ctx, now.AddDate(0, 0, -7))
	if err != nil {
		t.Fatal(err)
	}
	if len(perf) != 1 || perf[0].Model != "recent" {
		t.Errorf("window returned %+v, want only the recent model", perf)
	}
}

// Nothing measured is a normal answer, not an error — a fresh install has no
// samples and the screen has to render anyway.
func TestModelPerformanceOnAnEmptyTable(t *testing.T) {
	st := newTestStore(t)
	perf, err := st.ModelPerformanceSince(context.Background(), time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("empty table errored: %v", err)
	}
	if len(perf) != 0 {
		t.Errorf("got %d rows from an empty table", len(perf))
	}
}

// The repository index is provenance, not truth about existence — but what it
// does record must survive a re-download and normalise paths consistently.
func TestRepoFilesAndTags(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first := RepoFile{
		RelPath: "Qwen/Qwen3-8B-GGUF/model.gguf", HubID: "Qwen/Qwen3-8B-GGUF",
		Quant: "Q4_K_M", ParamsB: 8, SizeBytes: 100, SHA256: "abc",
	}
	if err := st.RecordRepoFile(ctx, first); err != nil {
		t.Fatal(err)
	}
	files, err := st.RepoFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	added := files[first.RelPath].AddedAt
	if added.IsZero() {
		t.Fatal("AddedAt should have been set")
	}

	// Re-downloading a corrupted file is the same acquisition arriving again,
	// so the date it was first chosen must not be reset.
	time.Sleep(10 * time.Millisecond)
	second := first
	second.SizeBytes = 200
	second.AddedAt = added
	if err := st.RecordRepoFile(ctx, second); err != nil {
		t.Fatal(err)
	}
	files, err = st.RepoFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := files[first.RelPath]
	if got.SizeBytes != 200 {
		t.Errorf("size = %d, want the updated 200", got.SizeBytes)
	}
	if !got.AddedAt.Equal(added) {
		t.Errorf("AddedAt moved from %v to %v on re-download", added, got.AddedAt)
	}
	if len(files) != 1 {
		t.Errorf("re-recording should update one row, got %d", len(files))
	}

	// Paths normalise, so the same file named two ways is one row.
	if err := st.RecordRepoFile(ctx, RepoFile{RelPath: "/Qwen/Qwen3-8B-GGUF/model.gguf"}); err != nil {
		t.Fatal(err)
	}
	if files, _ := st.RepoFiles(ctx); len(files) != 1 {
		t.Errorf("a leading slash should not make a second row, got %d", len(files))
	}

	// Case is preserved: on Linux two paths differing in case are two files.
	if err := st.RecordRepoFile(ctx, RepoFile{RelPath: "qwen/qwen3-8b-gguf/model.gguf"}); err != nil {
		t.Fatal(err)
	}
	if files, _ := st.RepoFiles(ctx); len(files) != 2 {
		t.Errorf("case-differing paths are different files, got %d rows", len(files))
	}

	if err := st.ForgetRepoFile(ctx, first.RelPath); err != nil {
		t.Fatal(err)
	}
	if files, _ := st.RepoFiles(ctx); len(files) != 1 {
		t.Errorf("after forgetting one row, want 1 left")
	}
}

func TestTagsNormaliseAndDeduplicate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const subject = "Qwen/Qwen3-8B-GGUF/model.gguf"

	// "Coding" and "coding" are one label a person typed twice.
	for _, tag := range []string{"coding", "Coding", "  CODING  ", "long context"} {
		if err := st.AddTag(ctx, subject, tag); err != nil {
			t.Fatal(err)
		}
	}
	tags, err := st.Tags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"coding", "long-context"}
	if len(tags[subject]) != len(want) {
		t.Fatalf("tags = %v, want %v", tags[subject], want)
	}
	for i, w := range want {
		if tags[subject][i] != w {
			t.Errorf("tags[%d] = %q, want %q", i, tags[subject][i], w)
		}
	}

	// Removing uses the same normalisation, so the label as typed comes off.
	if err := st.RemoveTag(ctx, subject, "Long Context"); err != nil {
		t.Fatal(err)
	}
	tags, _ = st.Tags(ctx)
	if len(tags[subject]) != 1 || tags[subject][0] != "coding" {
		t.Errorf("after removal tags = %v", tags[subject])
	}

	if err := st.AddTag(ctx, subject, "   "); err == nil {
		t.Error("an empty tag must be refused")
	}
	if err := st.AddTag(ctx, "", "coding"); err == nil {
		t.Error("a tag with no subject must be refused")
	}
}
