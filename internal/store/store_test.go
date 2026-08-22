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
	if err := st.AppendMessages(ctx, sess.ID, llm.UserMessage("hello")); err != nil {
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
	if err := st.AppendMessages(ctx, sess.ID, want...); err != nil {
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
