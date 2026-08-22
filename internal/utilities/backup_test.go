package utilities

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedConversation writes a client, a session and a couple of messages, which
// is the minimum needed for a backup or export to be about anything.
func seedConversation(t *testing.T, svc *Service, sessionID, content string) {
	t.Helper()
	db := svc.repo.db
	var clientID int64
	err := db.QueryRow(`SELECT id FROM clients WHERE name = 'archivist'`).Scan(&clientID)
	if err != nil {
		res, err := db.Exec(
			`INSERT INTO clients (name, token_hash, kind, created_at) VALUES ('archivist', 'hash', 'browser', ?)`,
			time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		clientID, err = res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if _, err := db.Exec(
		`INSERT INTO sessions (id, client_id, title, backend, model, agent_id, created_at, updated_at)
		 VALUES (?, ?, 'archived', 'local', 'qwen3:8b', '', ?, ?)`,
		sessionID, clientID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO messages (session_id, seq, role, content, backend, model, token_count, created_at)
		 VALUES (?, 1, 'user', ?, 'local', 'qwen3:8b', 0, ?)`,
		sessionID, content, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO messages (session_id, seq, role, content, backend, model, token_count, created_at)
		 VALUES (?, 2, 'assistant', 'understood', 'local', 'qwen3:8b', 12, ?)`,
		sessionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO muninn (session_id, call_id, tool_name, side, risk, input, decision, outcome, is_error, created_at)
		 VALUES (?, 'call-1', 'rename_file', 'client', 'write', '{}', 'denied', 'refused', 0, ?)`,
		sessionID, now); err != nil {
		t.Fatal(err)
	}
}

// A backup is only a backup once it has been opened and read back. This
// asserts the verification actually happened and reported what the snapshot
// contains, rather than trusting that VACUUM INTO returned cleanly.
func TestBackupIsVerifiedAndSelfDescribing(t *testing.T) {
	_, svc := newTestDB(t)
	ctx := context.Background()
	seedConversation(t, svc, "sess-backup", "remember the wifi password is hunter2")

	res, err := svc.Backup(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !res.Verified || res.Integrity != "ok" {
		t.Errorf("verified/integrity = %v/%q, want true/ok", res.Verified, res.Integrity)
	}
	if res.Rows["messages"] != 2 || res.Rows["sessions"] != 1 || res.Rows["muninn"] != 1 {
		t.Errorf("snapshot row counts = %v, want 2 messages, 1 session, 1 muninn", res.Rows)
	}
	if len(res.Files) != 1 || res.Files[0].SHA256 == "" {
		t.Fatalf("backup file missing a checksum: %+v", res.Files)
	}

	// The directory has to describe itself: years later the reader may have
	// nothing but the files.
	raw, err := os.ReadFile(filepath.Join(res.Destination, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest BackupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.ManifestVersion != backupManifestVersion || manifest.Application != "wintermute" {
		t.Errorf("manifest identity = v%d/%q", manifest.ManifestVersion, manifest.Application)
	}
	if !strings.HasPrefix(manifest.SchemaVersion, "0") {
		t.Errorf("manifest schema version = %q, want a migration name", manifest.SchemaVersion)
	}
	if manifest.Rows["messages"] != 2 {
		t.Errorf("manifest rows = %v", manifest.Rows)
	}

	// And the checksum must actually match the file on disk.
	sum, err := sha256File(filepath.Join(res.Destination, manifest.Files[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	if sum != manifest.Files[0].SHA256 {
		t.Error("manifest checksum does not match the snapshot it describes")
	}
}

// The snapshot has to be a real, independently readable database — the whole
// premise of keeping it for years is that some future thing can open it
// without this program's help.
func TestBackupSnapshotIsIndependentlyReadable(t *testing.T) {
	_, svc := newTestDB(t)
	ctx := context.Background()
	seedConversation(t, svc, "sess-read", "the boiler service is due in March")

	res, err := svc.Backup(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	check, err := verifySnapshot(ctx, filepath.Join(res.Destination, res.Files[0].Name))
	if err != nil {
		t.Fatalf("reopen snapshot: %v", err)
	}
	if check.Integrity != "ok" {
		t.Errorf("integrity = %q", check.Integrity)
	}
	if check.Rows["messages"] != 2 {
		t.Errorf("messages in snapshot = %d, want 2", check.Rows["messages"])
	}
}

// Retention must never be able to leave the operator with nothing, and must
// never touch a directory it did not write.
func TestPruneBackupsKeepsNewestAndIgnoresStrangers(t *testing.T) {
	_, svc := newTestDB(t)
	dir := t.TempDir()

	for _, name := range []string{
		backupDirPrefix + "2026-01-01T00-00-00",
		backupDirPrefix + "2026-02-01T00-00-00",
		backupDirPrefix + "2026-03-01T00-00-00",
	} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// Something else living in the same directory.
	if err := os.MkdirAll(filepath.Join(dir, "family-photos"), 0o750); err != nil {
		t.Fatal(err)
	}

	removed, err := svc.PruneBackups(dir, 2)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d, want 1", removed)
	}
	// Oldest gone, newest two kept, stranger untouched.
	for name, wantPresent := range map[string]bool{
		backupDirPrefix + "2026-01-01T00-00-00": false,
		backupDirPrefix + "2026-02-01T00-00-00": true,
		backupDirPrefix + "2026-03-01T00-00-00": true,
		"family-photos":                         true,
	} {
		_, err := os.Stat(filepath.Join(dir, name))
		if wantPresent && err != nil {
			t.Errorf("%s was removed and should not have been", name)
		}
		if !wantPresent && err == nil {
			t.Errorf("%s survived and should not have", name)
		}
	}
}

// keep <= 0 means keep everything. This is a deletion routine pointed at the
// operator's backups, and doing nothing is the right default.
func TestPruneBackupsKeepsEverythingByDefault(t *testing.T) {
	_, svc := newTestDB(t)
	dir := t.TempDir()
	for i := range 5 {
		name := backupDirPrefix + "2026-0" + string(rune('1'+i)) + "-01T00-00-00"
		if err := os.MkdirAll(filepath.Join(dir, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := svc.PruneBackups(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("removed %d with keep=0, want 0", removed)
	}
}
