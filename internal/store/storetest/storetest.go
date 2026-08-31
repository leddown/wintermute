// Package storetest opens migrated databases for tests without paying for the
// migrations every time.
//
// store.Open applies twenty-odd migrations in as many transactions, and in WAL
// mode every one of those commits is an fsync. That is around three hundred
// milliseconds per call on a fast disk and considerably worse on the small host
// this server actually runs on — and the suite opens a fresh database in nearly
// every test, so the deploy gate spent most of its time migrating empty
// databases over and over.
//
// The schema is identical every time, so it is built once per test binary and
// kept in memory as bytes. Each test gets a byte-for-byte copy of that file,
// which is a single write rather than twenty-two flushes. The store is then
// opened for real on that copy — it finds every migration already recorded and
// applies none — so tests exercise the opening path itself.
//
// The copy is opened with store.OpenUnsynced, so the writes a test then makes
// do not each wait on the disk either. A database in a temp directory that is
// deleted when the test ends has nothing to be durable for.
package storetest

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"wintermute/internal/store"
)

var (
	seedOnce sync.Once
	seedData []byte
	seedErr  error
)

// seed returns the bytes of an empty, fully migrated database file.
func seed() ([]byte, error) {
	seedOnce.Do(func() {
		dir, err := os.MkdirTemp("", "wintermute-storetest")
		if err != nil {
			seedErr = err
			return
		}
		defer os.RemoveAll(dir)

		path := filepath.Join(dir, "seed.db")
		st, err := store.OpenUnsynced(path)
		if err != nil {
			seedErr = err
			return
		}
		// Closing the last handle checkpoints the WAL into the main file and
		// removes the -wal and -shm sidecars, so the single file left behind is
		// the whole database.
		if err := st.Close(); err != nil {
			seedErr = err
			return
		}
		seedData, seedErr = os.ReadFile(path)
	})
	return seedData, seedErr
}

// New opens a fresh migrated database private to tb, closed when tb finishes.
func New(tb testing.TB) *store.Store {
	tb.Helper()
	return NewAt(tb, filepath.Join(tb.TempDir(), "test.db"))
}

// NewAt is New for tests that need the database's path as well as the handle —
// backup, export and vacuum all read the file itself.
func NewAt(tb testing.TB, path string) *store.Store {
	tb.Helper()
	data, err := seed()
	if err != nil {
		tb.Fatalf("build seed database: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		tb.Fatalf("write seed database: %v", err)
	}
	st, err := store.OpenUnsynced(path)
	if err != nil {
		tb.Fatalf("store.OpenUnsynced error: %v", err)
	}
	tb.Cleanup(func() { st.Close() })
	return st
}
