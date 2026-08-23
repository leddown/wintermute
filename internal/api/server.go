// Package api exposes the server's JSON HTTP interface and serves the
// embedded browser UI. Handlers are thin: decode, delegate to the agent or
// store, encode.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"wintermute/internal/agent"
	"wintermute/internal/knowledge"
	"wintermute/internal/llm"
	"wintermute/internal/modelrepo"
	"wintermute/internal/models"
	"wintermute/internal/node"
	"wintermute/internal/recall"
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
	// memory backs the shared-memory admin endpoints. Nil when no embedder is
	// configured, and the endpoints say so rather than pretending.
	memory *recall.Store
	// nodes is the fleet telemetry store. Nil when it is not enabled.
	nodes *node.Store
	// modelRepo is the library of weights on the operator's own disk. Nil
	// leaves its routes unregistered, the same way an absent CRM does — the
	// server simply has no repository rather than having an empty one.
	modelRepo *modelrepo.Repo
	// rawWindow is how long full-resolution samples survive, which is what
	// decides whether a requested span can be answered from them.
	rawWindow time.Duration
	// memoryIndexer rebuilds the index on request. Nil for the same reason.
	memoryIndexer *recall.Indexer
	// reloadBackends re-resolves the backend set and swaps it into the router
	// and catalog. Nil leaves the backend-management routes unregistered, so a
	// server assembled without it is read-only about its backends rather than
	// accepting writes it could not apply.
	reloadBackends func(context.Context) error
	info           ServerInfo
	log            *slog.Logger
	// errors keeps the detail behind the generic 5xx bodies this server
	// returns, so an operator can read it in the browser rather than over SSH.
	errors *errorLog
}

// New builds a Server. A zero Workspace disables those routes rather than
// registering handlers that would nil-panic on the first request.
func New(a *agent.Agent, s *store.Store, serverTools *tool.Registry, cat *models.Catalog, ws Workspace, info ServerInfo, log *slog.Logger) *Server {
	return &Server{agent: a, store: s, serverTools: serverTools, catalog: cat,
		workspace: ws, info: info, log: log, errors: newErrorLog()}
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

// WithModelRepo attaches the model repository. Without it the repository
// routes are not registered and the UI's Repository tab reports the feature as
// not configured.
func (s *Server) WithModelRepo(repo *modelrepo.Repo) *Server {
	s.modelRepo = repo
	return s
}

// WithUtilities attaches the housekeeping operations.
func (s *Server) WithUtilities(svc *utilities.Service) *Server {
	s.utilities = svc
	return s
}

// WithBackendAdmin enables declaring backends through the API. reload rebuilds
// the live backend set from the config file plus the stored declarations and
// applies it, so a change takes effect without a restart.
func (s *Server) WithBackendAdmin(reload func(context.Context) error) *Server {
	s.reloadBackends = reload
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
	authed("GET /api/v1/sessions/{id}/progress", s.handleTurnProgress)
	authed("POST /api/v1/sessions/{id}/messages", s.handlePostMessage)
	authed("POST /api/v1/sessions/{id}/tool_results", s.handleToolResults)
	authed("GET /api/v1/sessions/{id}/audit", s.handleAudit)
	authed("DELETE /api/v1/sessions/{id}", s.handleDeleteSession)
	authed("PATCH /api/v1/sessions/{id}/model", s.handleSetSessionModel)
	authed("PATCH /api/v1/sessions/{id}/memory", s.handleSetSessionMemory)
	authed("DELETE /api/v1/sessions/{id}/messages/{messageID}", s.handleDeleteMessage)

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
	// What the operator thinks of a model, as opposed to what it reports about
	// itself — see models.go.
	authed("POST /api/v1/models/note", s.handleSetModelNote)
	authed("GET /api/v1/models/champions", s.handleChampions)
	authed("POST /api/v1/models/champions", s.handleSetChampion)
	// Putting a model in memory on a backend, and taking it out again.
	authed("GET /api/v1/models/resident", s.handleResident)
	authed("GET /api/v1/models/performance", s.handleModelPerformance)
	authed("POST /api/v1/models/load", s.handleLoadModel)
	authed("POST /api/v1/models/unload", s.handleUnloadModel)

	// Model Context Protocol. Same bearer token as everything else, so an MCP
	// client is registered with `wintermuted -add-client` like any other.
	s.registerAdminRoutes(authed)

	// Remote hosts reporting what they are doing — see nodes.go.
	s.registerNodeRoutes(authed)

	authed("POST /mcp", s.handleMCP)

	// Company profile, CRM and tasks — see workspace.go.
	s.registerWorkspaceRoutes(authed)

	// Agent profiles and their document libraries — see agents.go.
	s.registerAgentRoutes(authed)

	// The canary tripwire — see twire.go.
	s.registerTwireRoutes(authed)
	s.registerBackendAdminRoutes(authed)

	// Backups, diagnostics and maintenance — see utilities.go.
	s.registerUtilitiesRoutes(authed)

	// Weights the operator keeps on their own disk — see modelrepo.go.
	s.registerModelRepoRoutes(authed)

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

// setSessionMemoryRequest carries the two switches. Both are required rather
// than optional: this endpoint sets the conversation's memory state, and a
// partial update whose omitted field silently keeps its old value is the wrong
// shape for a setting the operator must be able to be certain about.
type setSessionMemoryRequest struct {
	Record *bool `json:"record"`
	Recall *bool `json:"recall"`
}

// handleSetSessionMemory turns recording and recall on or off for one
// conversation.
//
// Turning recording off deletes the turns already written for this session, so
// this is not a reversible preference toggle — it destroys data on purpose,
// which is the whole point of it. The conversation itself keeps working from
// memory.
func (s *Server) handleSetSessionMemory(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var req setSessionMemoryRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Record == nil || req.Recall == nil {
		writeError(w, http.StatusBadRequest,
			"both record and recall are required, so the resulting state is never inferred")
		return
	}

	if err := s.agent.SetMemory(r.Context(), sess, *req.Record, *req.Recall); err != nil {
		s.fail(w, "set session memory", err)
		return
	}
	sess.Record, sess.Recall = *req.Record, *req.Recall
	writeJSON(w, http.StatusOK, sess)
}

// handleDeleteMessage removes one message and everything derived from it.
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("messageID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "message id must be a number")
		return
	}
	if err := s.store.DeleteMessage(r.Context(), sess.ID, sess.ClientID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such message in this conversation")
			return
		}
		s.fail(w, "delete message", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// handleDeleteSession discards a conversation, taking its messages and audit
// rows with it. This is a user action only — it is never offered to the model
// as a tool, for the same reason the utilities purges are not: the assistant
// has no business deciding that a record of what it did should stop existing.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	c := clientFrom(r.Context())
	err := s.store.DeleteSession(r.Context(), r.PathValue("id"), c.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		s.fail(w, "delete session", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	// A conversation that is off the record has no rows; its transcript lives
	// in memory for as long as the process does, and the browser still has to
	// be able to display what is being said.
	if !sess.Record {
		msgs, err := s.agent.EphemeralMessages(r.Context(), sess.ID)
		if err != nil {
			s.fail(w, "list messages", err)
			return
		}
		if msgs == nil {
			msgs = []llm.Message{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
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
//
// The detail is also kept in a bounded ring the admin API can read back — see
// errorlog.go. The response is unchanged: what a failing caller learns stays
// exactly as opaque as it was, and the operator gains a way to find out what
// happened without leaving the browser.
func (s *Server) fail(w http.ResponseWriter, op string, err error) {
	s.log.Error(op+" failed", "error", err)
	if s.errors != nil {
		s.errors.record(ServerError{Op: op, Err: err.Error()})
	}
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
