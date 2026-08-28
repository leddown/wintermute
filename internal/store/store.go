// Package store persists sessions, transcripts and the tool audit trail in
// SQLite. The driver is pure Go (modernc.org/sqlite) so the server needs no
// cgo toolchain and the database is a single file on the host.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// ErrDuplicate is returned when a uniqueness constraint would be violated.
var ErrDuplicate = errors.New("already exists")

// Store owns the database handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies any
// pending migrations. Use ":memory:" in tests.
func Open(path string) (*Store, error) {
	// WAL keeps the browser UI's reads from blocking the harness's writes;
	// busy_timeout avoids spurious SQLITE_BUSY under concurrent turns.
	//
	// _txlock=immediate is what makes that busy_timeout worth anything. Every
	// transaction in this package writes, and several of them read first —
	// AppendMessages takes the next seq before inserting. A deferred
	// transaction that reads and then writes has to *upgrade* its lock, and
	// SQLite will not wait for an upgrade: two connections both holding a read
	// lock and both wanting to write would deadlock, so it returns
	// SQLITE_BUSY at once and busy_timeout never comes into it. Two turns
	// landing together then failed one of them about a tenth of a second in,
	// which reads as a question that got no answer.
	//
	// Taking the write lock at BEGIN costs nothing here because there are no
	// read-only transactions to serialise — plain queries run outside one — and
	// a writer that waits is a writer busy_timeout can actually help.
	//
	// secure_delete makes a DELETE overwrite the row's content with zeros
	// instead of only unlinking it. It is off by default in SQLite, which
	// means deleted records survive in freeblocks and are routinely recovered
	// forensically — so without it, "delete this conversation" would leave the
	// text sitting in the file, and an off-the-record conversation whose
	// already-written turns are deleted would not actually be off the record.
	// It costs a little write throughput, which is not the scarce resource on
	// a single-operator server.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)&_pragma=secure_delete(ON)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// SQLite tolerates one writer; the pool is kept small deliberately.
	db.SetMaxOpenConns(4)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for callers that need it (health checks, tests).
func (s *Store) DB() *sql.DB { return s.db }

// migrate applies every embedded migration that has not run yet, in filename
// order, recording each in schema_migrations.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int
		err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&applied)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}

		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// Counts returns row counts for the tables an operator asks about. A table that
// fails to count is reported as absent rather than failing the whole call: the
// caller is an admin screen, and one broken count should not blank the page.
func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	var firstErr error
	for _, table := range []string{"sessions", "messages", "muninn", "clients"} {
		var n int
		// The table name is from this fixed list, never from a request.
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out[table] = n
	}
	return out, firstErr
}
