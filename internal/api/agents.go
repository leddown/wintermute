package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"wintermute/internal/knowledge"
)

// This file exposes agent profiles and their document libraries.
//
// An agent is a named configuration of the one assistant: a prompt, a model
// pin, the sources it may consult, and the documents uploaded to it. The point
// is separation — one agent for a GRC engagement, another for a different
// client, another for the firm's own finances — so that a conversation reaches
// the material it should and not the rest.
//
// These routes are also what the GRC application talks to: it lists agents so
// an operator can choose one in its settings, names that agent when it opens a
// session, and links here for document upload.

func (s *Server) registerAgentRoutes(authed func(string, http.HandlerFunc)) {
	if s.knowledge == nil {
		return
	}
	authed("GET /api/v1/agents", s.handleListAgents)
	authed("POST /api/v1/agents", s.handleCreateAgent)
	authed("GET /api/v1/agents/{id}", s.handleGetAgent)
	authed("PUT /api/v1/agents/{id}", s.handleUpdateAgent)
	authed("DELETE /api/v1/agents/{id}", s.handleDeleteAgent)
	authed("GET /api/v1/agents/{id}/documents", s.handleListDocuments)
	authed("POST /api/v1/agents/{id}/documents", s.handleUploadDocument)
	authed("DELETE /api/v1/agents/{id}/documents/{docID}", s.handleDeleteDocument)
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.knowledge.Agents(r.Context())
	if err != nil {
		s.fail(w, "list agents", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":  agents,
		"sources": knowledge.Sources(),
		// What the server can actually back each source with. An agent may
		// declare the grc source on a server with no GRC installation
		// configured; saying so here is better than a tool that is silently
		// never offered.
		"available": map[string]bool{
			knowledge.SourceDocuments: true,
			knowledge.SourceGRC:       s.grcConfigured,
			knowledge.SourceWeb:       s.webConfigured,
		},
	})
}

type agentRequest struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	SystemPrompt string   `json:"system_prompt"`
	Backend      string   `json:"backend"`
	Model        string   `json:"model"`
	Sources      []string `json:"sources"`
}

func (r agentRequest) toAgent(id string) *knowledge.Agent {
	agent := &knowledge.Agent{
		ID:           strings.TrimSpace(firstNonEmpty(id, r.ID)),
		Name:         r.Name,
		Description:  r.Description,
		SystemPrompt: r.SystemPrompt,
		Backend:      r.Backend,
		Model:        r.Model,
		Sources:      r.Sources,
	}
	return agent
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req agentRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.checkBackend(req.Backend); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	agent, err := s.knowledge.CreateAgent(r.Context(), req.toAgent(""))
	if err != nil {
		s.failKnowledge(w, "create agent", err)
		return
	}
	writeJSON(w, http.StatusCreated, agent)
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	agent, err := s.knowledge.Agent(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failKnowledge(w, "get agent", err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	var req agentRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.checkBackend(req.Backend); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	agent, err := s.knowledge.UpdateAgent(r.Context(), req.toAgent(r.PathValue("id")))
	if err != nil {
		s.failKnowledge(w, "update agent", err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.knowledge.DeleteAgent(r.Context(), r.PathValue("id")); err != nil {
		s.failKnowledge(w, "delete agent", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	docs, err := s.knowledge.Documents(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failKnowledge(w, "list documents", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// handleUploadDocument accepts a multipart upload and adds it to the agent's
// library. This is the page the GRC application links to when someone wants a
// document the assistant can consult.
func (s *Server) handleUploadDocument(w http.ResponseWriter, r *http.Request) {
	// Bounded before ParseMultipartForm reads through it, so an oversized
	// upload is refused rather than buffered first.
	r.Body = http.MaxBytesReader(w, r.Body, knowledge.MaxDocumentBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "expected a multipart upload with a file field")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "a file is required")
		return
	}
	defer func() { _ = file.Close() }()

	body, err := io.ReadAll(io.LimitReader(file, knowledge.MaxDocumentBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the uploaded file")
		return
	}
	if len(body) > knowledge.MaxDocumentBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("the document exceeds the %s limit", knowledge.HumanBytes(knowledge.MaxDocumentBytes)))
		return
	}

	doc, err := s.knowledge.Upload(r.Context(), knowledge.UploadInput{
		AgentID:   r.PathValue("id"),
		Title:     r.FormValue("title"),
		Filename:  header.Filename,
		MediaType: header.Header.Get("Content-Type"),
		SourceURL: r.FormValue("source_url"),
		Body:      body,
	})
	if err != nil {
		s.failKnowledge(w, "upload document", err)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("docID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid document id")
		return
	}
	if err := s.knowledge.DeleteDocument(r.Context(), r.PathValue("id"), id); err != nil {
		s.failKnowledge(w, "delete document", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveAgent validates the agent named on a new session and applies its model
// pin when the request did not set one. An agent's backend is a default, not a
// constraint: a caller that asks for a specific model still gets it.
func (s *Server) resolveAgent(ctx context.Context, agentID, backend, model string) (string, string, string, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" || s.knowledge == nil {
		return "", backend, model, nil
	}
	agent, err := s.knowledge.Agent(ctx, agentID)
	if err != nil {
		var notFound knowledge.ErrNotFound
		if errors.As(err, &notFound) {
			return "", "", "", fmt.Errorf("unknown agent %q", agentID)
		}
		return "", "", "", err
	}
	if backend == "" {
		backend = agent.Backend
	}
	if model == "" {
		model = agent.Model
	}
	if err := s.checkBackend(backend); err != nil {
		return "", "", "", err
	}
	return agent.ID, backend, model, nil
}

// failKnowledge maps the knowledge package's error vocabulary onto statuses.
func (s *Server) failKnowledge(w http.ResponseWriter, what string, err error) {
	var notFound knowledge.ErrNotFound
	var invalid knowledge.ErrInvalid
	switch {
	case errors.As(err, &notFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.fail(w, what, err)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
