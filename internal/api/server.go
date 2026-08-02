// Package api exposes the server's JSON HTTP interface and serves the
// embedded browser UI. Handlers are thin: decode, delegate to the agent or
// store, encode.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"wintermute/internal/agent"
	"wintermute/internal/store"
	"wintermute/internal/tool"
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
	log         *slog.Logger
	llmName     string
}

// New builds a Server.
func New(a *agent.Agent, s *store.Store, serverTools *tool.Registry, log *slog.Logger, llmName string) *Server {
	return &Server{agent: a, store: s, serverTools: serverTools, log: log, llmName: llmName}
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
	writeJSON(w, http.StatusOK, map[string]any{
		"name":  c.Name,
		"kind":  c.Kind,
		"model": s.llmName,
	})
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tools": s.serverTools.Definitions()})
}

type createSessionRequest struct {
	Title string `json:"title"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if !decode(w, r, &req) {
		return
	}
	c := clientFrom(r.Context())
	sess, err := s.store.CreateSession(r.Context(), c.ID, req.Title)
	if err != nil {
		s.fail(w, "create session", err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
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
