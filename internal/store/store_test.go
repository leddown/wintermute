package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

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

	sess, err := st.CreateSession(ctx, owner.ID, "mine")
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

func TestTranscriptRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	client, _, err := st.CreateClient(ctx, "c", KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(ctx, client.ID, "")
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
	sess, err := st.CreateSession(ctx, client.ID, "")
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
	sess, err := st.CreateSession(ctx, client.ID, "")
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
