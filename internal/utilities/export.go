package utilities

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Portable memory export.
//
// A snapshot (see Backup) is the whole database, byte for byte, and is the
// right artefact for restoring this server after a disk fails. It is the wrong
// artefact for the other thing the operator needs, which is carrying the
// conversation history forward into a rebuilt or replaced installation years
// from now: it carries every unrelated module along with it, it cannot be
// merged into an existing database, and reading it requires knowing the whole
// schema rather than the part that matters.
//
// So this is the second leg. JSON Lines, one record per line, one file per
// table, with a manifest describing what is in the directory and a checksum
// for each file. Both formats are, as it happens, on the Library of Congress's
// list of recommended storage formats for datasets — SQLite and JSON — which
// is the closest thing there is to an assurance that something will still be
// readable in twenty years.
//
// The content is exported in the neutral role/content form it is stored in,
// never as a rendered prompt with some model's chat template applied. That is
// the property that lets this history survive a model swap, and it is the one
// thing an export could quietly destroy.

// exportFormatVersion versions the export's own layout. A reader checks this
// before interpreting anything else.
const exportFormatVersion = 1

const (
	exportClients  = "clients.jsonl"
	exportSessions = "sessions.jsonl"
	exportMessages = "messages.jsonl"
	exportMuninn   = "muninn.jsonl"
)

// ExportManifest describes an export directory.
type ExportManifest struct {
	FormatVersion int       `json:"format_version"`
	Application   string    `json:"application"`
	CreatedAt     time.Time `json:"created_at"`
	SchemaVersion string    `json:"schema_version"`
	// Files maps each data file to its checksum and size.
	Files []BackupFile `json:"files"`
	// Counts records how many records each file holds, so a truncated export
	// is detectable without re-reading every line.
	Counts map[string]int64 `json:"counts"`
	// Embedder records which embedding model the vectors in this export were
	// produced by, when there are any. It is empty until the index exists;
	// vectors are re-derivable from the text and are therefore not carried,
	// but which model produced them is not re-derivable and is.
	Embedder string `json:"embedder,omitempty"`
	// EmbedderDim is that model's vector width.
	EmbedderDim int `json:"embedder_dim,omitempty"`
}

// ExportResult summarises a completed export.
type ExportResult struct {
	Destination string           `json:"destination"`
	Counts      map[string]int64 `json:"counts"`
	Files       []BackupFile     `json:"files"`
	CreatedAt   time.Time        `json:"created_at"`
}

// ImportResult summarises what an import actually changed.
type ImportResult struct {
	Source string `json:"source"`
	// Inserted counts rows written; Skipped counts rows already present.
	// Re-importing the same export is a no-op, and the two numbers are how the
	// operator sees that rather than having to trust it.
	Inserted map[string]int64 `json:"inserted"`
	Skipped  map[string]int64 `json:"skipped"`
}

// ---- export ----------------------------------------------------------------

// ExportMemory writes the conversation history to a timestamped directory of
// JSON Lines files under destDir.
//
// Sessions reference their owning client by name rather than by row id,
// because ids are local to one database and the whole point of this format is
// to be read by another one.
//
// Client authentication tokens are not exported. They are credentials, and an
// archive that is going to be copied to a NAS and kept for years is the last
// place they should be; on import, clients are recreated without a usable
// token and the operator re-issues one with -add-client.
func (s *Service) ExportMemory(ctx context.Context, destDir string) (ExportResult, error) {
	if !filepath.IsAbs(destDir) {
		return ExportResult{}, ErrInvalidDestination
	}
	stamp := time.Now().UTC().Format("2006-01-02T15-04-05")
	outDir := filepath.Join(destDir, "wintermute-memory-"+stamp)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return ExportResult{}, fmt.Errorf("utilities: create export directory: %w", err)
	}

	counts := map[string]int64{}
	var files []BackupFile

	type job struct {
		file  string
		query string
		scan  func(*sql.Rows) (any, error)
	}
	jobs := []job{
		{exportClients, `SELECT name, kind, created_at FROM clients ORDER BY id`,
			func(r *sql.Rows) (any, error) {
				var rec exportedClient
				err := r.Scan(&rec.Name, &rec.Kind, &rec.CreatedAt)
				return rec, err
			}},
		{exportSessions, `SELECT s.id, c.name, s.title, s.backend, s.model, s.agent_id,
			s.record, s.recall, s.created_at, s.updated_at
			FROM sessions s JOIN clients c ON c.id = s.client_id ORDER BY s.created_at`,
			func(r *sql.Rows) (any, error) {
				var rec exportedSession
				err := r.Scan(&rec.ID, &rec.ClientName, &rec.Title, &rec.Backend, &rec.Model,
					&rec.AgentID, &rec.Record, &rec.Recall, &rec.CreatedAt, &rec.UpdatedAt)
				return rec, err
			}},
		{exportMessages, `SELECT session_id, seq, role, content, tool_calls, tool_call_id,
			is_error, thinking, backend, model, token_count, created_at
			FROM messages ORDER BY session_id, seq`,
			func(r *sql.Rows) (any, error) {
				var rec exportedMessage
				err := r.Scan(&rec.SessionID, &rec.Seq, &rec.Role, &rec.Content, &rec.ToolCalls,
					&rec.ToolCallID, &rec.IsError, &rec.Thinking, &rec.Backend, &rec.Model,
					&rec.TokenCount, &rec.CreatedAt)
				return rec, err
			}},
		{exportMuninn, `SELECT session_id, call_id, tool_name, side, risk, input,
			decision, outcome, is_error, created_at
			FROM muninn ORDER BY id`,
			func(r *sql.Rows) (any, error) {
				var rec exportedAudit
				err := r.Scan(&rec.SessionID, &rec.CallID, &rec.ToolName, &rec.Side, &rec.Risk,
					&rec.Input, &rec.Decision, &rec.Outcome, &rec.IsError, &rec.CreatedAt)
				return rec, err
			}},
	}

	for _, j := range jobs {
		n, err := s.writeJSONL(ctx, filepath.Join(outDir, j.file), j.query, j.scan)
		if err != nil {
			os.RemoveAll(outDir)
			return ExportResult{}, err
		}
		counts[j.file] = n

		path := filepath.Join(outDir, j.file)
		fi, err := os.Stat(path)
		if err != nil {
			return ExportResult{}, fmt.Errorf("utilities: stat export file: %w", err)
		}
		sum, err := sha256File(path)
		if err != nil {
			return ExportResult{}, err
		}
		files = append(files, BackupFile{Name: j.file, Size: fi.Size(), SHA256: sum})
	}

	var schemaVersion string
	if err := s.repo.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(name), '') FROM schema_migrations`).Scan(&schemaVersion); err != nil {
		return ExportResult{}, fmt.Errorf("utilities: read schema version: %w", err)
	}

	manifest := ExportManifest{
		FormatVersion: exportFormatVersion,
		Application:   "wintermute",
		CreatedAt:     time.Now().UTC(),
		SchemaVersion: schemaVersion,
		Files:         files,
		Counts:        counts,
	}
	buf, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ExportResult{}, fmt.Errorf("utilities: encode export manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), append(buf, '\n'), 0o640); err != nil {
		return ExportResult{}, fmt.Errorf("utilities: write export manifest: %w", err)
	}

	return ExportResult{
		Destination: outDir, Counts: counts, Files: files, CreatedAt: manifest.CreatedAt,
	}, nil
}

// writeJSONL streams one query to one file, one JSON object per line.
//
// Streaming rather than building a slice matters: this runs against a history
// that is meant to grow for years, and an exporter that has to hold the whole
// transcript in memory would start failing exactly when the archive became
// worth having.
func (s *Service) writeJSONL(ctx context.Context, path, query string, scan func(*sql.Rows) (any, error)) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, fmt.Errorf("utilities: create %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()

	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)

	rows, err := s.repo.db.QueryContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("utilities: read for export: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var n int64
	for rows.Next() {
		rec, err := scan(rows)
		if err != nil {
			return 0, fmt.Errorf("utilities: scan for export: %w", err)
		}
		if err := enc.Encode(rec); err != nil {
			return 0, fmt.Errorf("utilities: encode export record: %w", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("utilities: read for export: %w", err)
	}
	if err := w.Flush(); err != nil {
		return 0, fmt.Errorf("utilities: flush export: %w", err)
	}
	// Durability matters more than speed here: an export the operator is about
	// to copy to another machine should be on the disk, not in a page cache.
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("utilities: sync export: %w", err)
	}
	return n, nil
}

// ---- record shapes ---------------------------------------------------------
//
// These are the archive's public shape and the reason it is worth having.
// They are deliberately flat, self-describing and independent of the Go types
// elsewhere in this program, so that changing an internal struct cannot
// silently change what a decade of archives mean.

type exportedClient struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

type exportedSession struct {
	ID         string    `json:"id"`
	ClientName string    `json:"client_name"`
	Title      string    `json:"title"`
	Backend    string    `json:"backend"`
	Model      string    `json:"model"`
	AgentID    string    `json:"agent_id"`
	Record     bool      `json:"record"`
	Recall     bool      `json:"recall"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type exportedMessage struct {
	SessionID  string    `json:"session_id"`
	Seq        int       `json:"seq"`
	Role       string    `json:"role"`
	Content    string    `json:"content"`
	ToolCalls  string    `json:"tool_calls,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
	IsError    bool      `json:"is_error,omitempty"`
	Thinking   string    `json:"thinking,omitempty"`
	Backend    string    `json:"backend,omitempty"`
	Model      string    `json:"model,omitempty"`
	TokenCount int       `json:"token_count,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type exportedAudit struct {
	SessionID string    `json:"session_id"`
	CallID    string    `json:"call_id"`
	ToolName  string    `json:"tool_name"`
	Side      string    `json:"side"`
	Risk      string    `json:"risk"`
	Input     string    `json:"input,omitempty"`
	Decision  string    `json:"decision"`
	Outcome   string    `json:"outcome,omitempty"`
	IsError   bool      `json:"is_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ---- import ----------------------------------------------------------------

// ImportMemory merges an export directory into this database.
//
// It is idempotent. Sessions key on their id and messages on
// (session_id, seq) — a uniqueness constraint the schema already had — so
// importing the same archive twice inserts nothing the second time. That
// matters because the realistic use is not one clean restore into an empty
// database but repeated merges: an archive from the old machine, then a newer
// one, then the same one again because nobody remembered whether it had been
// done.
//
// Nothing is ever overwritten. Where a record already exists it is left alone
// and counted as skipped, because the row already in the database is the one
// this server has been serving and an archive is not entitled to silently
// replace it.
func (s *Service) ImportMemory(ctx context.Context, srcDir string) (ImportResult, error) {
	if !filepath.IsAbs(srcDir) {
		return ImportResult{}, ErrInvalidDestination
	}

	raw, err := os.ReadFile(filepath.Join(srcDir, "manifest.json"))
	if err != nil {
		return ImportResult{}, fmt.Errorf("utilities: read export manifest: %w", err)
	}
	var manifest ExportManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return ImportResult{}, fmt.Errorf("utilities: decode export manifest: %w", err)
	}
	if manifest.FormatVersion != exportFormatVersion {
		return ImportResult{}, fmt.Errorf(
			"utilities: export format version %d, this build reads version %d",
			manifest.FormatVersion, exportFormatVersion)
	}

	// Verify each file against the manifest before writing a single row. An
	// archive that has rotted on disk should be refused, not half-imported.
	for _, f := range manifest.Files {
		path := filepath.Join(srcDir, f.Name)
		sum, err := sha256File(path)
		if err != nil {
			return ImportResult{}, err
		}
		if f.SHA256 != "" && sum != f.SHA256 {
			return ImportResult{}, fmt.Errorf(
				"utilities: %s does not match its checksum (archive is damaged or was modified)", f.Name)
		}
	}

	res := ImportResult{
		Source:   srcDir,
		Inserted: map[string]int64{},
		Skipped:  map[string]int64{},
	}

	tx, err := s.repo.db.BeginTx(ctx, nil)
	if err != nil {
		return ImportResult{}, fmt.Errorf("utilities: begin import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	clientIDs, err := importClients(ctx, tx, srcDir, &res)
	if err != nil {
		return ImportResult{}, err
	}
	if err := importSessions(ctx, tx, srcDir, clientIDs, &res); err != nil {
		return ImportResult{}, err
	}
	if err := importMessages(ctx, tx, srcDir, &res); err != nil {
		return ImportResult{}, err
	}
	if err := importAudit(ctx, tx, srcDir, &res); err != nil {
		return ImportResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return ImportResult{}, fmt.Errorf("utilities: commit import: %w", err)
	}
	return res, nil
}

// importClients recreates clients by name and returns name→id for the session
// pass. A client that already exists keeps its row and its token.
func importClients(ctx context.Context, tx *sql.Tx, srcDir string, res *ImportResult) (map[string]int64, error) {
	ids := map[string]int64{}
	err := readJSONL(filepath.Join(srcDir, exportClients), func(line []byte) error {
		var rec exportedClient
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM clients WHERE name = ?`, rec.Name).Scan(&id)
		if err == nil {
			ids[rec.Name] = id
			res.Skipped[exportClients]++
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// An imported client gets a token hash nothing can hash to, so the row
		// carries ownership without carrying a credential. The operator issues
		// a real token with -add-client when they want that machine to connect.
		placeholder, err := unusableTokenHash()
		if err != nil {
			return err
		}
		out, err := tx.ExecContext(ctx,
			`INSERT INTO clients (name, token_hash, kind, created_at) VALUES (?, ?, ?, ?)`,
			rec.Name, placeholder, rec.Kind, rec.CreatedAt)
		if err != nil {
			return err
		}
		newID, err := out.LastInsertId()
		if err != nil {
			return err
		}
		ids[rec.Name] = newID
		res.Inserted[exportClients]++
		return nil
	})
	return ids, err
}

func importSessions(ctx context.Context, tx *sql.Tx, srcDir string, clientIDs map[string]int64, res *ImportResult) error {
	return readJSONL(filepath.Join(srcDir, exportSessions), func(line []byte) error {
		var rec exportedSession
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		clientID, ok := clientIDs[rec.ClientName]
		if !ok {
			return fmt.Errorf("session %s names client %q, which is not in the archive",
				rec.ID, rec.ClientName)
		}
		out, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO sessions
			 (id, client_id, title, backend, model, agent_id, record, recall, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, clientID, rec.Title, rec.Backend, rec.Model, rec.AgentID,
			rec.Record, rec.Recall, rec.CreatedAt, rec.UpdatedAt)
		if err != nil {
			return err
		}
		countInsert(out, exportSessions, res)
		return nil
	})
}

func importMessages(ctx context.Context, tx *sql.Tx, srcDir string, res *ImportResult) error {
	return readJSONL(filepath.Join(srcDir, exportMessages), func(line []byte) error {
		var rec exportedMessage
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		// UNIQUE (session_id, seq) is what makes the re-import case free.
		out, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO messages
			 (session_id, seq, role, content, tool_calls, tool_call_id, is_error,
			  thinking, backend, model, token_count, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.SessionID, rec.Seq, rec.Role, rec.Content, rec.ToolCalls, rec.ToolCallID,
			rec.IsError, rec.Thinking, rec.Backend, rec.Model, rec.TokenCount, rec.CreatedAt)
		if err != nil {
			return err
		}
		countInsert(out, exportMessages, res)
		return nil
	})
}

// importAudit merges the episodic record. muninn has no natural key — its id
// is an autoincrement local to whichever database wrote it — so a row is
// treated as already present when the same call, in the same session, at the
// same instant is already there.
func importAudit(ctx context.Context, tx *sql.Tx, srcDir string, res *ImportResult) error {
	return readJSONL(filepath.Join(srcDir, exportMuninn), func(line []byte) error {
		var rec exportedAudit
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		var exists int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM muninn WHERE session_id = ? AND call_id = ? AND tool_name = ?
			 AND created_at = ? LIMIT 1`,
			rec.SessionID, rec.CallID, rec.ToolName, rec.CreatedAt).Scan(&exists)
		if err == nil {
			res.Skipped[exportMuninn]++
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO muninn (session_id, call_id, tool_name, side, risk, input,
			 decision, outcome, is_error, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.SessionID, rec.CallID, rec.ToolName, rec.Side, rec.Risk, rec.Input,
			rec.Decision, rec.Outcome, rec.IsError, rec.CreatedAt); err != nil {
			return err
		}
		res.Inserted[exportMuninn]++
		return nil
	})
}

func countInsert(out sql.Result, file string, res *ImportResult) {
	if n, err := out.RowsAffected(); err == nil && n > 0 {
		res.Inserted[file] += n
		return
	}
	res.Skipped[file]++
}

// readJSONL streams a JSON Lines file, calling fn for each non-empty line.
// A bad line is reported with its number: the format's whole advantage is that
// one damaged record does not make the rest unreadable, so the error says
// which one it was.
func readJSONL(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// An archive from an older export need not have every file.
			return nil
		}
		return fmt.Errorf("utilities: open %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// Transcripts contain long messages; the default 64KB line cap is not
	// enough for a model's reply with its thinking attached.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var lineNo int
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return fmt.Errorf("utilities: %s line %d: %w", filepath.Base(path), lineNo, err)
		}
	}
	return scanner.Err()
}

// unusableTokenHash returns a value the token hash column will accept and no
// token can ever produce. Real hashes are hex-encoded SHA-256, so a value with
// a non-hex prefix cannot collide with one.
func unusableTokenHash() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("utilities: generate placeholder: %w", err)
	}
	return "imported-no-token-" + hex.EncodeToString(buf), nil
}
