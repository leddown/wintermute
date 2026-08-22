package api

import (
	"net/http"
	"os"
	"time"
)

// The admin surface answers "why is the server behaving like this?" from the
// browser, instead of requiring ssh and a look at the env file.
//
// Nothing here returns a secret. API keys, the Hugging Face token and client
// tokens are reported as present or absent and never by value: this is a page
// that gets left open, screenshotted and pasted into chat, and a config screen
// that prints keys is how they escape. Client tokens could not be shown even if
// that were wanted — the store keeps only their hashes.

// ServerInfo is the effective configuration, assembled by the composition root
// so this package does not have to import config and re-derive it. It is a
// snapshot taken at startup: these values cannot change without a restart, and
// presenting them as live would be a lie.
type ServerInfo struct {
	Addr              string        `json:"addr"`
	DatabasePath      string        `json:"database_path"`
	BackendsPath      string        `json:"backends_path"`
	DefaultBackend    string        `json:"default_backend"`
	FallbackBackend   string        `json:"fallback_backend"`
	PoolBackends      []string      `json:"pool_backends"`
	LLMMaxTokens      int           `json:"llm_max_tokens"`
	LLMTimeout        time.Duration `json:"-"`
	LLMTimeoutLabel   string        `json:"llm_timeout"`
	MaxToolIterations int           `json:"max_tool_iterations"`

	// Presence, never value.
	MetadataProviders   []string `json:"metadata_providers"`
	HasHuggingFaceToken bool     `json:"has_huggingface_token"`

	// Knowledge sources an agent profile can be given, described rather than
	// disclosed: a base URL is configuration worth seeing on the admin screen,
	// the token behind it is not.
	GRC       string `json:"grc"`
	WebSearch string `json:"web_search"`

	GoVersion string    `json:"go_version"`
	StartedAt time.Time `json:"started_at"`
}

func (s *Server) registerAdminRoutes(authed func(string, http.HandlerFunc)) {
	authed("GET /api/v1/admin/config", s.handleAdminConfig)
	authed("GET /api/v1/admin/status", s.handleAdminStatus)
	authed("GET /api/v1/admin/clients", s.handleAdminClients)
	authed("DELETE /api/v1/admin/clients/{name}", s.handleAdminRevokeClient)
	authed("GET /api/v1/admin/tools", s.handleAdminTools)

	// Shared memory: the master switch, and the two ways to throw things
	// away. See memory.go.
	authed("GET /api/v1/admin/memory", s.handleMemoryStatus)
	authed("PATCH /api/v1/admin/memory", s.handleSetMemoryEnabled)
	authed("POST /api/v1/admin/memory/rebuild-index", s.handleRebuildMemoryIndex)
	authed("POST /api/v1/admin/memory/forget-everything", s.handleForgetEverything)
}

func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	info := s.info
	info.LLMTimeoutLabel = info.LLMTimeout.String()
	writeJSON(w, http.StatusOK, info)
}

// AdminStatus is what is true right now, as opposed to what was configured.
type AdminStatus struct {
	UptimeSeconds int64  `json:"uptime_seconds"`
	Uptime        string `json:"uptime"`
	StartedAt     string `json:"started_at"`

	DatabasePath  string `json:"database_path"`
	DatabaseBytes int64  `json:"database_bytes"`
	// A WAL that keeps growing is the usual sign of a reader holding a
	// transaction open, so it is worth surfacing next to the database itself.
	WALBytes int64 `json:"wal_bytes"`

	Sessions int `json:"sessions"`
	Messages int `json:"messages"`
	Muninn   int `json:"muninn"`
	Clients  int `json:"clients"`

	ServerTools int `json:"server_tools"`
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	st := AdminStatus{
		StartedAt:    s.info.StartedAt.UTC().Format(time.RFC3339),
		DatabasePath: s.info.DatabasePath,
		ServerTools:  len(s.serverTools.Definitions()),
	}
	d := time.Since(s.info.StartedAt)
	st.UptimeSeconds = int64(d.Seconds())
	st.Uptime = d.Truncate(time.Second).String()

	if fi, err := os.Stat(s.info.DatabasePath); err == nil {
		st.DatabaseBytes = fi.Size()
	}
	if fi, err := os.Stat(s.info.DatabasePath + "-wal"); err == nil {
		st.WALBytes = fi.Size()
	}

	// Counts come straight off the database. A failure here is reported as a
	// zero rather than a 500: the rest of the page is still useful, and an
	// admin screen that goes blank because one count failed is worse than one
	// that admits it does not know.
	counts, err := s.store.Counts(r.Context())
	if err != nil {
		s.log.Warn("admin status counts failed", "error", err)
	}
	st.Sessions = counts["sessions"]
	st.Messages = counts["messages"]
	st.Muninn = counts["muninn"]
	st.Clients = counts["clients"]

	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleAdminClients(w http.ResponseWriter, r *http.Request) {
	clients, err := s.store.ListClients(r.Context())
	if err != nil {
		s.fail(w, "list clients", err)
		return
	}
	// store.Client carries no token, only its hash, and that is not in the
	// struct either. Nothing needs redacting here; it is worth saying so.
	writeJSON(w, http.StatusOK, map[string]any{"clients": clients})
}

// handleAdminRevokeClient deletes a client, and with it the token's ability to
// authenticate.
//
// There is deliberately no matching create route. `wintermuted -add-client` is
// the only way to mint a token, which keeps token issuance on the machine
// rather than behind a credential that could itself be stolen — a leaked
// browser token that could mint more would turn one compromised session into
// permanent access. Revocation is the opposite: it only ever removes access,
// so it is safe to expose.
func (s *Server) handleAdminRevokeClient(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "a client name is required")
		return
	}
	// Revoking the credential the request arrived on would lock the operator
	// out mid-click, and the page would have no way to tell them why.
	if me := clientFrom(r.Context()); me.Name == name {
		writeError(w, http.StatusConflict,
			"that is the client you are signed in as; revoke it from another session or with wintermuted -revoke-client")
		return
	}
	if err := s.store.DeleteClient(r.Context(), name); err != nil {
		s.fail(w, "revoke client", err)
		return
	}
	s.log.Info("client revoked", "client", name, "by", clientFrom(r.Context()).Name)
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminTools lists what the model can call server-side, with the risk
// level each one declares. Risk levels drive the approval policy, so being able
// to read them without going to the source is the point.
func (s *Server) handleAdminTools(w http.ResponseWriter, r *http.Request) {
	defs := s.serverTools.Definitions()
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		out = append(out, map[string]any{
			"name":        d.Name,
			"risk":        d.Risk,
			"description": d.Description,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}
