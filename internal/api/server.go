// Package api exposes the server's JSON HTTP interface and serves the
// embedded browser UI. Handlers are thin: decode, delegate to the agent or
// store, encode.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"wintermute/internal/agent"
	"wintermute/internal/knowledge"
	"wintermute/internal/models"
	"wintermute/internal/store"
	"wintermute/internal/tool"
	"wintermute/internal/twire"
	"wintermute/internal/utilities"
	"wintermute/internal/web"
)

// maxBodyBytes bounds request bodies. Tool results can carry a directory
// listing, so it is generous, but not unbounded.
const maxBodyBytes = 8 << 20

// Server holds the API's dependencies.
type Server struct {
	agent       *agent.Agent
	store       *store.Store
	serverTools *tool.Registry
	catalog     *models.Catalog
	workspace   Workspace
	knowledge   *knowledge.Service
	// grcConfigured and webConfigured report whether the server can actually
	// back those sources, so the UI can say "declared but not configured"
	// rather than leaving an agent quietly toothless.
	grcConfigured bool
	webConfigured bool
	// twire is the canary tripwire. Nil leaves its routes unregistered, the
	// same way an absent CRM does.
	twire *twire.Service
	// utilities is the housekeeping surface: backups, diagnostics, vacuum and
	// pruning. Nil likewise leaves its routes off.
	utilities *utilities.Service
	info      ServerInfo
	log       *slog.Logger
}

// New builds a Server. A zero Workspace disables those routes rather than
// registering handlers that would nil-panic on the first request.
func New(a *agent.Agent, s *store.Store, serverTools *tool.Registry, cat *models.Catalog, ws Workspace, info ServerInfo, log *slog.Logger) *Server {
	return &Server{agent: a, store: s, serverTools: serverTools, catalog: cat,
		workspace: ws, info: info, log: log}
}

// WithKnowledge attaches agent profiles and their libraries. Without it the
// agent routes are not registered and every session is the unscoped assistant,
// which is what this server was before agents existed.
//
// grcAvailable and webAvailable report whether those sources have been
// configured on this server, so the UI can distinguish "this agent does not use
// the web" from "this server cannot".
func (s *Server) WithKnowledge(svc *knowledge.Service, grcAvailable, webAvailable bool) *Server {
	s.knowledge = svc
	s.grcConfigured = grcAvailable
	s.webConfigured = webAvailable
	return s
}

// WithTwire attaches the canary tripwire. Without it the twire routes are not
// registered and the UI's twire view reports the module as unavailable.
func (s *Server) WithTwire(svc *twire.Service) *Server {
	s.twire = svc
	return s
}

// WithUtilities attaches the housekeeping operations.
func (s *Server) WithUtilities(svc *utilities.Service) *Server {
	s.utilities = svc
	return s
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: liveness only. It reports no configuration details.
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	authed := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.authenticate(h))
	}
	authed("GET /api/v1/me", s.handleMe)
	authed("GET /api/v1/tools", s.handleTools)
	authed("POST /api/v1/sessions", s.handleCreateSession)
	authed("GET /api/v1/sessions", s.handleListSessions)
	authed("GET /api/v1/sessions/{id}/messages", s.handleMessages)
	authed("POST /api/v1/sessions/{id}/messages", s.handlePostMessage)
	authed("POST /api/v1/sessions/{id}/tool_results", s.handleToolResults)
	authed("GET /api/v1/sessions/{id}/audit", s.handleAudit)
	authed("PATCH /api/v1/sessions/{id}/model", s.handleSetSessionModel)

	// Model awareness: hardware, backends, catalog, discovery and planning.
	authed("GET /api/v1/system", s.handleSystem)
	authed("GET /api/v1/backends", s.handleBackends)
	authed("POST /api/v1/backends/refresh", s.handleRefreshBackends)
	// Send one prompt to one backend and see what comes back — see
	// backendtest.go for why it neither falls back nor keeps a transcript.
	authed("POST /api/v1/backends/{name}/test", s.handleTestBackend)
	authed("GET /api/v1/models", s.handleModels)
	authed("GET /api/v1/models/search", s.handleModelSearch)
	// The Hub id contains a slash ("author/name"), so it needs a trailing
	// wildcard rather than a single path segment.
	authed("GET /api/v1/models/detail/{id...}", s.handleModelDetail)
	authed("POST /api/v1/models/plan", s.handlePlan)
	authed("POST /api/v1/models/fit", s.handleFit)
	authed("GET /api/v1/tasks", s.handleTasks)

	// Model Context Protocol. Same bearer token as everything else, so an MCP
	// client is registered with `wintermuted -add-client` like any other.
	s.registerAdminRoutes(authed)

	authed("POST /mcp", s.handleMCP)

	// Company profile, CRM and tasks — see workspace.go.
	s.registerWorkspaceRoutes(authed)

	// Agent profiles and their document libraries — see agents.go.
	s.registerAgentRoutes(authed)

	// The canary tripwire — see twire.go.
	s.registerTwireRoutes(authed)

	// Backups, diagnostics and maintenance — see utilities.go.
	s.registerUtilitiesRoutes(authed)

	mux.Handle("/", web.Handler())

	return s.recoverPanic(s.logRequests(mux))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DB().PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	c := clientFrom(r.Context())
	router := s.agent.Router()
	writeJSON(w, http.StatusOK, map[string]any{
		"name":            c.Name,
		"kind":            c.Kind,
		"backends":        router.Names(),
		"default_backend": router.Default(),
		"fallback":        router.Fallback(),
	})
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tools": s.serverTools.Definitions()})
}

type createSessionRequest struct {
	Title string `json:"title"`
	// Backend and Model pin this conversation to a model. Empty means the
	// server default.
	Backend string `json:"backend"`
	Model   string `json:"model"`
	// Agent names an agent profile, which decides the documents and external
	// sources this conversation may reach. Empty is the unscoped assistant.
	Agent string `json:"agent"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.checkBackend(req.Backend); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	agentID, backend, model, err := s.resolveAgent(r.Context(), req.Agent, req.Backend, req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	c := clientFrom(r.Context())
	sess, err := s.store.CreateSession(r.Context(), c.ID, req.Title, backend, model, agentID)
	if err != nil {
		s.fail(w, "create session", err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

type setSessionModelRequest struct {
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

// handleSetSessionModel repoints an existing conversation at another model.
// The transcript is kept — switching mid-conversation is deliberate, and is
// how a stuck local turn gets escalated to a stronger model.
func (s *Server) handleSetSessionModel(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var req setSessionModelRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.checkBackend(req.Backend); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SetSessionModel(r.Context(), sess.ID, req.Backend, req.Model); err != nil {
		s.fail(w, "set session model", err)
		return
	}
	sess.Backend, sess.Model = req.Backend, req.Model
	writeJSON(w, http.StatusOK, sess)
}

// checkBackend rejects an unknown backend name up front, so the failure lands
// on the request that chose it rather than on the next turn.
func (s *Server) checkBackend(name string) error {
	if name == "" {
		return nil
	}
	if _, ok := s.agent.Router().Backend(name); !ok {
		return fmt.Errorf("unknown backend %q", name)
	}
	return nil
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	c := clientFrom(r.Context())
	sessions, err := s.store.ListSessions(r.Context(), c.ID, 50)
	if err != nil {
		s.fail(w, "list sessions", err)
		return
	}
	if sessions == nil {
		sessions = []store.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	msgs, err := s.store.Messages(r.Context(), sess.ID)
	if err != nil {
		s.fail(w, "list messages", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	entries, err := s.store.AuditForSession(r.Context(), sess.ID, 200)
	if err != nil {
		s.fail(w, "list audit", err)
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// session resolves the {id} path value, scoped to the authenticated client.
func (s *Server) session(w http.ResponseWriter, r *http.Request) (*store.Session, bool) {
	c := clientFrom(r.Context())
	sess, err := s.store.Session(r.Context(), r.PathValue("id"), c.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return nil, false
	}
	if err != nil {
		s.fail(w, "lookup session", err)
		return nil, false
	}
	return sess, true
}

// fail logs the underlying error and returns a generic message, so internal
// details never reach the client.
func (s *Server) fail(w http.ResponseWriter, op string, err error) {
	s.log.Error(op+" failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already written; nothing useful is left to do.
		return
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
