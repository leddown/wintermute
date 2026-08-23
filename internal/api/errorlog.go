package api

// Recent server errors, kept so they can be read in the browser.
//
// Every 5xx this server returns is deliberately opaque: fail() logs the real
// error and answers "internal error", so a stack of internal detail never
// reaches a client. That is the right default and it stays. What it cost was
// diagnosability — the operator sees "internal error" in the UI and the only
// way to learn anything more is to go and read journalctl on the server, which
// is a different machine, over SSH, in another window.
//
// So the detail is also kept here: a small ring of recent failures, readable
// through the admin API by the same authenticated operator who just triggered
// one. The response to the failing request is unchanged — a caller still learns
// nothing from it — and the detail is available to someone who can already read
// this server's configuration, backends and client list.
//
// In memory and bounded. These are a diagnostic aid for something that just
// happened, not a record of anything; muninn is the audit trail and it is in
// the database. A ring that cannot grow also cannot become the reason a server
// that is failing repeatedly runs out of memory.

import (
	"net/http"
	"sync"
	"time"
)

// errorRingSize is how many failures are kept. Enough to cover a burst while
// someone is looking at a screen, small enough that the whole thing is a
// rounding error against a single conversation.
const errorRingSize = 50

// ServerError is one recorded failure.
type ServerError struct {
	ID int64     `json:"id"`
	At time.Time `json:"at"`
	// Op is the operation name fail() was given — "initialise model repository",
	// "list model repository". It is what identifies a failure; the request
	// path would add little beside it, and threading one through all seventy
	// call sites of fail() to find out would be a poor trade.
	Op  string `json:"op"`
	Err string `json:"error"`
}

// errorLog is a bounded ring of recent failures.
type errorLog struct {
	mu      sync.Mutex
	entries []ServerError
	next    int64
}

func newErrorLog() *errorLog {
	return &errorLog{entries: make([]ServerError, 0, errorRingSize)}
}

// record adds a failure, dropping the oldest when full.
func (l *errorLog) record(e ServerError) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	e.ID = l.next
	e.At = time.Now().UTC()
	if len(l.entries) < errorRingSize {
		l.entries = append(l.entries, e)
		return
	}
	// Full: shift down by one rather than growing. At fifty entries this is
	// cheaper than any structure that avoids it, and it keeps the slice in
	// chronological order for the reader.
	copy(l.entries, l.entries[1:])
	l.entries[len(l.entries)-1] = e
}

// list returns the most recent failures, newest first.
func (l *errorLog) list(limit int) []ServerError {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.entries) {
		limit = len(l.entries)
	}
	out := make([]ServerError, 0, limit)
	for i := len(l.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, l.entries[i])
	}
	return out
}

// handleAdminErrors reports recent failures.
//
// The one endpoint in this server whose whole purpose is to say what went
// wrong, which is why it exists separately rather than by making fail() less
// discreet.
func (s *Server) handleAdminErrors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"errors": s.errors.list(queryInt(r, "limit", errorRingSize)),
	})
}
