package utilities

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wintermute/internal/store"
)

// The prune tests are the point of this file.
//
// Every other operation here fails loudly when it is wrong — a backup that
// cannot write says so, a vacuum that cannot run returns an error. A prune with
// a mismatched timestamp format is the one that fails silently and in the worst
// possible direction: the comparison is between two strings, so a format the
// query did not expect either matches nothing (the prune quietly does nothing
// for months) or matches everything (it deletes the lot). These tests pin both
// stored layouts against a real database.

func newTestDB(t *testing.T) (*store.Store, *Service) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "utilities-test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open error: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, NewService(st.DB(), path)
}

// seedClient inserts the client every session must reference.
func seedClient(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO clients (name, token_hash, kind, created_at) VALUES ('t', 'h', 'browser', ?)`,
		time.Now().UTC())
	if err != nil {
		t.Fatalf("insert client: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("client id: %v", err)
	}
	return id
}

// insertSession writes a session with an explicit updated_at, passing a
// time.Time so the driver renders it exactly as the real code path does. That
// is the whole point: the test must not spell the timestamp itself, or it would
// be asserting against its own assumption rather than the driver's behaviour.
func insertSession(t *testing.T, db *sql.DB, clientID int64, id string, updatedAt time.Time) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO sessions (id, client_id, title, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		id, clientID, updatedAt, updatedAt)
	if err != nil {
		t.Fatalf("insert session %s: %v", id, err)
	}
}

func TestPruneSessionsByAge(t *testing.T) {
	ctx := context.Background()
	st, svc := newTestDB(t)
	clientID := seedClient(t, st.DB())

	now := time.Now().UTC()
	insertSession(t, st.DB(), clientID, "ancient", now.AddDate(0, 0, -400))
	insertSession(t, st.DB(), clientID, "old", now.AddDate(0, 0, -60))
	insertSession(t, st.DB(), clientID, "recent", now.AddDate(0, 0, -3))
	insertSession(t, st.DB(), clientID, "today", now)

	res, err := svc.Prune(ctx, PruneTargetSessions, 30)
	if err != nil {
		t.Fatalf("Prune error: %v", err)
	}
	if res.DeletedRows != 2 {
		t.Fatalf("deleted %d sessions, want 2 (ancient and old)", res.DeletedRows)
	}
	if res.Target != PruneTargetSessions || res.OlderThanDays != 30 {
		t.Errorf("result = %+v, want the target and window echoed back", res)
	}

	var remaining []string
	rows, err := st.DB().QueryContext(ctx, `SELECT id FROM sessions ORDER BY id`)
	if err != nil {
		t.Fatalf("query sessions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		remaining = append(remaining, id)
	}
	if len(remaining) != 2 || remaining[0] != "recent" || remaining[1] != "today" {
		t.Errorf("remaining = %v, want [recent today]", remaining)
	}
}

// A prune of sessions must take the transcript and the audit trail with it.
// Those go through ON DELETE CASCADE, which only works because store.Open sets
// foreign_keys(ON) — a pragma that is off by default in SQLite, and whose
// absence would leave orphaned rows behind with no error anywhere.
func TestPruneSessionsCascades(t *testing.T) {
	ctx := context.Background()
	st, svc := newTestDB(t)
	clientID := seedClient(t, st.DB())

	old := time.Now().UTC().AddDate(0, 0, -90)
	insertSession(t, st.DB(), clientID, "old", old)
	if _, err := st.DB().Exec(
		`INSERT INTO messages (session_id, seq, role, content, created_at) VALUES ('old', 1, 'user', 'hello', ?)`,
		old); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO tool_audit (session_id, call_id, tool_name, side, risk, decision, created_at)
		 VALUES ('old', 'c1', 'list_canary_status', 'server', 'read', 'auto', ?)`,
		old); err != nil {
		t.Fatalf("insert audit: %v", err)
	}

	if _, err := svc.Prune(ctx, PruneTargetSessions, 30); err != nil {
		t.Fatalf("Prune error: %v", err)
	}

	for _, table := range []string{"messages", "tool_audit"} {
		var n int
		if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d row(s) after its session was pruned", table, n)
		}
	}
}

// tool_audit can be pruned on its own, leaving the conversation intact. This is
// the case for an old session worth keeping whose audit rows are the bulk of it.
func TestPruneToolAuditLeavesSessions(t *testing.T) {
	ctx := context.Background()
	st, svc := newTestDB(t)
	clientID := seedClient(t, st.DB())

	now := time.Now().UTC()
	insertSession(t, st.DB(), clientID, "kept", now)
	for i, at := range []time.Time{now.AddDate(0, 0, -90), now.AddDate(0, 0, -1)} {
		if _, err := st.DB().Exec(
			`INSERT INTO tool_audit (session_id, call_id, tool_name, side, risk, decision, created_at)
			 VALUES ('kept', ?, 'x', 'server', 'read', 'auto', ?)`,
			string(rune('a'+i)), at); err != nil {
			t.Fatalf("insert audit: %v", err)
		}
	}

	res, err := svc.Prune(ctx, PruneTargetToolAudit, 30)
	if err != nil {
		t.Fatalf("Prune error: %v", err)
	}
	if res.DeletedRows != 1 {
		t.Errorf("deleted %d audit rows, want 1", res.DeletedRows)
	}
	var sessions int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Errorf("sessions = %d, want the conversation kept", sessions)
	}
}

// fintech_ai_usage is written in RFC 3339 by its own repository, not in the
// driver's layout. Pruning it with the wrong cutoff format is exactly the
// silent failure this file exists to catch.
func TestPruneAIUsageUsesRFC3339(t *testing.T) {
	ctx := context.Background()
	st, svc := newTestDB(t)

	now := time.Now().UTC()
	for _, at := range []time.Time{now.AddDate(0, 0, -90), now.AddDate(0, 0, -2)} {
		if _, err := st.DB().Exec(
			`INSERT INTO fintech_ai_usage (kind, backend, model, input_tokens, output_tokens, created_at)
			 VALUES ('forecast', 'local', 'm', 10, 20, ?)`,
			at.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert usage: %v", err)
		}
	}

	res, err := svc.Prune(ctx, PruneTargetAIUsage, 30)
	if err != nil {
		t.Fatalf("Prune error: %v", err)
	}
	if res.DeletedRows != 1 {
		t.Fatalf("deleted %d usage rows, want 1", res.DeletedRows)
	}
}

func TestPruneRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	_, svc := newTestDB(t)

	if _, err := svc.Prune(ctx, PruneTargetSessions, 0); !errors.Is(err, ErrInvalidDays) {
		t.Errorf("Prune(0 days) = %v, want ErrInvalidDays", err)
	}
	if _, err := svc.Prune(ctx, "users; DROP TABLE sessions", 30); !errors.Is(err, ErrInvalidPruneTarget) {
		t.Errorf("Prune(unknown target) = %v, want ErrInvalidPruneTarget", err)
	}
}

func TestAPIUsageGroupsByKind(t *testing.T) {
	ctx := context.Background()
	st, svc := newTestDB(t)

	now := time.Now().UTC()
	rows := []struct {
		kind    string
		in, out int64
		at      time.Time
	}{
		{"forecast", 100, 10, now},
		{"forecast", 200, 20, now.AddDate(0, 0, -5)},
		{"review", 50, 5, now},
	}
	for _, r := range rows {
		if _, err := st.DB().Exec(
			`INSERT INTO fintech_ai_usage (kind, backend, model, input_tokens, output_tokens, created_at)
			 VALUES (?, 'local', 'm', ?, ?, ?)`,
			r.kind, r.in, r.out, r.at.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert usage: %v", err)
		}
	}

	usage, err := svc.APIUsage(ctx)
	if err != nil {
		t.Fatalf("APIUsage error: %v", err)
	}
	if len(usage.Sources) != 2 {
		t.Fatalf("sources = %d, want one per kind (forecast, review)", len(usage.Sources))
	}
	if usage.Total.RequestCount != 3 || usage.Total.InputTokens != 350 || usage.Total.OutputTokens != 35 {
		t.Errorf("total = %+v, want 3 requests / 350 in / 35 out", usage.Total)
	}
	// Only the two rows stamped today count towards the daily figures.
	if usage.Total.TodayRequestCount != 2 || usage.Total.TodayInputTokens != 150 {
		t.Errorf("today = %d requests / %d in, want 2 / 150",
			usage.Total.TodayRequestCount, usage.Total.TodayInputTokens)
	}
	if usage.Note == "" {
		t.Error("usage carries no note, so nothing tells the reader chat turns are uncounted")
	}
}

func TestSystemInfo(t *testing.T) {
	ctx := context.Background()
	_, svc := newTestDB(t)

	info, err := svc.SystemInfo(ctx)
	if err != nil {
		t.Fatalf("SystemInfo error: %v", err)
	}
	if info.DatabaseSizeBytes <= 0 {
		t.Error("database size = 0, want the migrated schema to occupy pages")
	}
	if len(info.Tables) == 0 {
		t.Fatal("no tables reported, want one per migrated table")
	}
	// sqlite_master's own bookkeeping tables must not appear as application
	// data, and every table must have been counted.
	for _, tbl := range info.Tables {
		if len(tbl.Name) >= 7 && tbl.Name[:7] == "sqlite_" {
			t.Errorf("internal table %q reported as application data", tbl.Name)
		}
		if tbl.SizeBytes <= 0 {
			t.Errorf("table %q size = 0, want dbstat to have reported pages", tbl.Name)
		}
	}
	// Sizes fold indexes into their parent table, so no index name should show
	// up as a row of its own.
	for _, tbl := range info.Tables {
		if tbl.Name == "idx_sessions_client" || tbl.Name == "idx_audit_session" {
			t.Errorf("index %q listed as a table", tbl.Name)
		}
	}
	if info.Disk.TotalBytes == 0 {
		t.Error("disk total = 0, want the filesystem holding the database")
	}
	if info.GoVersion == "" || info.UptimeSeconds < 0 {
		t.Errorf("go version %q / uptime %v", info.GoVersion, info.UptimeSeconds)
	}
}

func TestBackupWritesARestorableCopy(t *testing.T) {
	ctx := context.Background()
	st, svc := newTestDB(t)
	clientID := seedClient(t, st.DB())
	insertSession(t, st.DB(), clientID, "s1", time.Now().UTC())

	if _, err := svc.Backup(ctx, "relative/path"); !errors.Is(err, ErrInvalidDestination) {
		t.Fatalf("Backup(relative) = %v, want ErrInvalidDestination", err)
	}

	dest := t.TempDir()
	res, err := svc.Backup(ctx, dest)
	if err != nil {
		t.Fatalf("Backup error: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Size <= 0 {
		t.Fatalf("files = %+v, want one non-empty file", res.Files)
	}
	if filepath.Dir(res.Destination) != dest {
		t.Errorf("destination %q is not inside %q", res.Destination, dest)
	}

	// The copy has to be a working database with the data in it — a backup that
	// only proves a file exists is not worth having.
	copied := filepath.Join(res.Destination, res.Files[0].Name)
	if _, err := os.Stat(copied); err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	restored, err := sql.Open("sqlite", copied)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer restored.Close()
	var n int
	if err := restored.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if n != 1 {
		t.Errorf("backup holds %d sessions, want 1", n)
	}
}

func TestVacuumReportsSizes(t *testing.T) {
	ctx := context.Background()
	st, svc := newTestDB(t)
	clientID := seedClient(t, st.DB())

	// Enough rows that deleting them frees whole pages, so the reclaim figure
	// has something to report.
	now := time.Now().UTC()
	for i := 0; i < 200; i++ {
		id := "s" + string(rune('A'+i%26)) + string(rune('a'+i/26))
		insertSession(t, st.DB(), clientID, id, now)
		if _, err := st.DB().Exec(
			`INSERT INTO messages (session_id, seq, role, content, created_at) VALUES (?, 1, 'user', ?, ?)`,
			id, string(make([]byte, 2000)), now); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	if _, err := st.DB().Exec(`DELETE FROM sessions`); err != nil {
		t.Fatalf("delete sessions: %v", err)
	}

	res, err := svc.Vacuum(ctx)
	if err != nil {
		t.Fatalf("Vacuum error: %v", err)
	}
	if res.BeforeBytes <= 0 || res.AfterBytes <= 0 {
		t.Fatalf("sizes = %d -> %d, want both positive", res.BeforeBytes, res.AfterBytes)
	}
	if res.AfterBytes >= res.BeforeBytes {
		t.Errorf("after (%d) is not smaller than before (%d); the vacuum reclaimed nothing",
			res.AfterBytes, res.BeforeBytes)
	}
	if res.ReclaimedBytes != res.BeforeBytes-res.AfterBytes {
		t.Errorf("reclaimed %d, want before-after", res.ReclaimedBytes)
	}
}

// The sampler must not report a rate it cannot measure. The first reading has
// no predecessor, so it says so rather than showing zeros that read as an idle
// machine.
func TestResourcesWarmUp(t *testing.T) {
	_, svc := newTestDB(t)

	first := svc.Resources()
	if !first.Warming {
		t.Error("first sample is not marked warming")
	}
	if first.CPUPercent != 0 {
		t.Errorf("first sample reports %.1f%% CPU with nothing to compare against", first.CPUPercent)
	}

	// A second reading inside the minimum interval must repeat the previous
	// result rather than divide by a near-zero elapsed time.
	if second := svc.Resources(); second != first {
		t.Errorf("immediate re-sample = %+v, want the previous result %+v", second, first)
	}
}
