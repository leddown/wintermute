package utilities

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The test this whole format exists for: a history exported from one
// installation lands intact in a different, empty one.
//
// "A different installation" is modelled honestly — a second database opened
// from scratch, with its own migrations and its own client row ids — because
// the failure this guards against is exactly an export that only works when
// the ids happen to line up.
func TestExportImportCarriesHistoryToAFreshInstall(t *testing.T) {
	ctx := context.Background()
	_, source := newTestDB(t)
	seedConversation(t, source, "sess-portable", "the stopcock is under the stairs")

	exported, err := source.ExportMemory(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if exported.Counts[exportMessages] != 2 || exported.Counts[exportSessions] != 1 {
		t.Fatalf("exported counts = %v", exported.Counts)
	}

	// A brand new installation, sharing nothing with the first.
	_, fresh := newTestDB(t)
	res, err := fresh.ImportMemory(ctx, exported.Destination)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Inserted[exportMessages] != 2 {
		t.Errorf("inserted messages = %d, want 2", res.Inserted[exportMessages])
	}

	// The content has to be there, and it has to still be the *text*, not a
	// prompt with some model's chat template baked into it.
	var content, backend, model string
	var tokens int
	err = fresh.repo.db.QueryRow(
		`SELECT content, backend, model, token_count FROM messages
		 WHERE session_id = 'sess-portable' AND seq = 1`).Scan(&content, &backend, &model, &tokens)
	if err != nil {
		t.Fatalf("read imported message: %v", err)
	}
	if content != "the stopcock is under the stairs" {
		t.Errorf("imported content = %q", content)
	}
	if strings.ContainsAny(content, "<|") {
		t.Error("imported content carries chat-template markup; it must be neutral text")
	}
	if backend != "local" || model != "qwen3:8b" {
		t.Errorf("provenance lost in transit: %q/%q", backend, model)
	}

	// The session, its memory switches and the episodic record come too.
	var record, recall bool
	if err := fresh.repo.db.QueryRow(
		`SELECT record, recall FROM sessions WHERE id = 'sess-portable'`).Scan(&record, &recall); err != nil {
		t.Fatalf("read imported session: %v", err)
	}
	if !record || !recall {
		t.Errorf("imported switches = %v/%v, want true/true", record, recall)
	}
	var auditRows int
	if err := fresh.repo.db.QueryRow(
		`SELECT count(*) FROM muninn WHERE session_id = 'sess-portable'`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows != 1 {
		t.Errorf("imported muninn rows = %d, want 1", auditRows)
	}
}

// Importing the same archive twice must change nothing the second time. The
// realistic use is repeated merges by someone who cannot remember whether they
// already did this one.
func TestImportIsIdempotent(t *testing.T) {
	ctx := context.Background()
	_, source := newTestDB(t)
	seedConversation(t, source, "sess-twice", "bin day is Tuesday")

	exported, err := source.ExportMemory(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, fresh := newTestDB(t)
	if _, err := fresh.ImportMemory(ctx, exported.Destination); err != nil {
		t.Fatal(err)
	}
	second, err := fresh.ImportMemory(ctx, exported.Destination)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Inserted[exportMessages] != 0 || second.Inserted[exportMuninn] != 0 {
		t.Errorf("second import inserted %v, want nothing", second.Inserted)
	}

	var n int
	if err := fresh.repo.db.QueryRow(
		`SELECT count(*) FROM messages WHERE session_id = 'sess-twice'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("after a double import there are %d messages, want 2", n)
	}
}

// An archive that has rotted on disk must be refused outright rather than
// half-imported, which would leave the database holding an unknown fraction of
// a conversation.
func TestImportRefusesADamagedArchive(t *testing.T) {
	ctx := context.Background()
	_, source := newTestDB(t)
	seedConversation(t, source, "sess-rot", "the meter reading was 41028")

	exported, err := source.ExportMemory(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Tamper with one line, leaving the manifest's checksum stale.
	path := filepath.Join(exported.Destination, exportMessages)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	altered := strings.Replace(string(raw), "41028", "99999", 1)
	if altered == string(raw) {
		t.Fatal("test did not actually alter the archive")
	}
	if err := os.WriteFile(path, []byte(altered), 0o640); err != nil {
		t.Fatal(err)
	}

	_, fresh := newTestDB(t)
	if _, err := fresh.ImportMemory(ctx, exported.Destination); err == nil {
		t.Fatal("import accepted a damaged archive")
	}

	// And nothing was written on the way to refusing.
	var n int
	if err := fresh.repo.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a refused import left %d messages behind, want 0", n)
	}
}

// The archive must not carry credentials. It is going to be copied to a NAS
// and kept for years.
func TestExportCarriesNoTokens(t *testing.T) {
	ctx := context.Background()
	_, source := newTestDB(t)
	seedConversation(t, source, "sess-secrets", "hello")

	exported, err := source.ExportMemory(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(exported.Destination, exportClients))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hash") {
		t.Error("client export contains a token hash")
	}

	// And an imported client cannot authenticate with anything.
	_, fresh := newTestDB(t)
	if _, err := fresh.ImportMemory(ctx, exported.Destination); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := fresh.repo.db.QueryRow(
		`SELECT token_hash FROM clients WHERE name = 'archivist'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "imported-no-token-") {
		t.Errorf("imported client token hash = %q, want an unusable placeholder", stored)
	}
}

// The manifest is what makes the directory readable by something that is not
// this program.
func TestExportManifestDescribesTheArchive(t *testing.T) {
	ctx := context.Background()
	_, source := newTestDB(t)
	seedConversation(t, source, "sess-manifest", "hello")

	exported, err := source.ExportMemory(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(exported.Destination, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m ExportManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.FormatVersion != exportFormatVersion {
		t.Errorf("format version = %d", m.FormatVersion)
	}
	if m.Counts[exportMessages] != 2 {
		t.Errorf("manifest counts = %v", m.Counts)
	}
	if len(m.Files) != 4 {
		t.Errorf("manifest lists %d files, want 4", len(m.Files))
	}
	for _, f := range m.Files {
		if f.SHA256 == "" {
			t.Errorf("%s has no checksum", f.Name)
		}
	}
}

// A future build reading an archive it does not understand must say so rather
// than guess at it.
func TestImportRefusesAnUnknownFormatVersion(t *testing.T) {
	ctx := context.Background()
	_, source := newTestDB(t)
	seedConversation(t, source, "sess-future", "hello")

	exported, err := source.ExportMemory(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(exported.Destination, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["format_version"] = exportFormatVersion + 99
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o640); err != nil {
		t.Fatal(err)
	}

	_, fresh := newTestDB(t)
	_, err = fresh.ImportMemory(ctx, exported.Destination)
	if err == nil {
		t.Fatal("import accepted an archive from a newer format")
	}
	if !strings.Contains(err.Error(), "format version") {
		t.Errorf("error should name the version problem, got: %v", err)
	}
}
