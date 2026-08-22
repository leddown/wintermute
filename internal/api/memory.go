package api

import (
	"net/http"

	"wintermute/internal/recall"
)

// The memory admin surface: one switch, and two ways to throw things away.
//
// These live under /admin rather than beside the per-session endpoints because
// they are server-wide. The per-session switches in server.go decide how one
// conversation behaves; these decide whether the feature is operating at all,
// and what the store holds.

// WithMemory attaches the memory store, enabling the admin endpoints below. A
// server without an embedder configured never gets one, and the endpoints
// report that rather than pretending to work.
func (s *Server) WithMemory(store *recall.Store) *Server {
	s.memory = store
	return s
}

// memoryUnavailable answers when no embedder is configured. It is a 200 with
// an explicit "off" rather than a 404, because "this server has no memory" is
// a real answer to "what is the state of memory" and the admin screen should
// be able to render it.
func (s *Server) memoryUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": false,
		"reason":     "no embedder configured; set WINTERMUTE_EMBED_URL and WINTERMUTE_EMBED_MODEL",
	})
}

func (s *Server) handleMemoryStatus(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		s.memoryUnavailable(w)
		return
	}
	stats, err := s.memory.Stats(r.Context())
	if err != nil {
		s.fail(w, "read memory status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"enabled":    stats.Enabled,
		"indexed":    stats.Indexed,
		"queued":     stats.Queued,
		"messages":   stats.Messages,
		"sessions":   stats.Sessions,
		"embedder":   stats.Embedder,
		"dimension":  stats.Dimension,
	})
}

type setMemoryEnabledRequest struct {
	// Required rather than optional, for the same reason the per-session
	// switches are: this is a setting an operator must be able to be certain
	// about, and a request whose omitted field silently kept the old value is
	// the wrong shape for that.
	Enabled *bool `json:"enabled"`
}

// handleSetMemoryEnabled is the master switch: recall on or off for every
// conversation on the server.
func (s *Server) handleSetMemoryEnabled(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		s.memoryUnavailable(w)
		return
	}
	var req setMemoryEnabledRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	if err := s.memory.SetEnabled(r.Context(), *req.Enabled); err != nil {
		s.fail(w, "set memory switch", err)
		return
	}
	// Worth a log line at info: someone turning shared memory off is a fact
	// that explains later behaviour, and the alternative is wondering for a
	// week why nothing is being recalled.
	s.log.Info("shared memory switched", "enabled", *req.Enabled)
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "enabled": *req.Enabled})
}

// handleClearMemoryIndex throws away the retrieval index and leaves every
// conversation intact.
//
// This is the reversible one. The index is derived from `messages`, so
// -backfill-memory rebuilds it exactly; it is what to reach for when
// retrieval is behaving oddly and the question is whether the index is at
// fault.
func (s *Server) handleClearMemoryIndex(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		s.memoryUnavailable(w)
		return
	}
	if err := s.memory.ClearIndex(r.Context()); err != nil {
		s.fail(w, "clear memory index", err)
		return
	}
	s.log.Warn("memory index cleared; conversations kept, rebuild with -backfill-memory")
	writeJSON(w, http.StatusOK, map[string]any{
		"cleared": "index",
		"note":    "conversations kept; rebuild the index with: wintermuted -backfill-memory",
	})
}

type forgetEverythingRequest struct {
	// Confirm must be the exact word, which is what keeps this endpoint from
	// being reachable by an accidental POST or a stray click. It is checked
	// server-side rather than only in the browser, because a destructive
	// endpoint that trusts its caller to have asked first is one curl away
	// from wiping the store.
	Confirm string `json:"confirm"`
}

// forgetConfirmation is what handleForgetEverything requires in the body.
const forgetConfirmation = "delete everything"

// handleForgetEverything deletes every conversation on the server.
//
// This is the irreversible one, and it exists for exactly one situation: a new
// installation full of test conversations that are worth nothing and would
// otherwise be recalled forever as though they meant something.
//
// It takes everything with it — messages, vectors, lexical entries and the
// audit trail — because a fortnight of testing against fake data is not
// evidence of anything, and leaving the audit rows behind would leave the
// bulkiest half of the rubbish in place.
func (s *Server) handleForgetEverything(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		s.memoryUnavailable(w)
		return
	}
	var req forgetEverythingRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Confirm != forgetConfirmation {
		writeError(w, http.StatusBadRequest,
			`this deletes every conversation on the server and cannot be undone; `+
				`send {"confirm": "`+forgetConfirmation+`"} if that is what you want`)
		return
	}

	deleted, err := s.memory.ForgetEverything(r.Context())
	if err != nil {
		s.fail(w, "forget everything", err)
		return
	}
	s.log.Warn("every conversation deleted from the store", "sessions", deleted)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted_sessions": deleted,
		"note":             "conversations, messages, vectors and audit rows are gone; snapshots taken before now still hold them",
	})
}
