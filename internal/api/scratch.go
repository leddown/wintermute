package api

import (
	"errors"
	"net/http"

	"wintermute/internal/scratch"
)

// The scratch pad's routes. Five of them, and no verbs beyond the CRUD: the
// server stores the text and hands it back, and everything that makes it an
// editor happens in the browser.
func (s *Server) registerScratchRoutes(authed func(string, http.HandlerFunc)) {
	if s.workspace.Scratch == nil {
		return
	}
	authed("GET /api/v1/scratch", s.handleListScratch)
	authed("POST /api/v1/scratch", s.handleCreateScratch)
	authed("GET /api/v1/scratch/{id}", s.handleGetScratch)
	authed("PUT /api/v1/scratch/{id}", s.handleUpdateScratch)
	authed("DELETE /api/v1/scratch/{id}", s.handleDeleteScratch)
}

func (s *Server) handleListScratch(w http.ResponseWriter, r *http.Request) {
	docs, err := s.workspace.Scratch.List()
	if err != nil {
		s.fail(w, "list scratch documents", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"docs": docs, "max_body": scratch.MaxBodyLen})
}

func (s *Server) handleGetScratch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	doc, err := s.workspace.Scratch.Get(id)
	if err != nil {
		s.scratchError(w, "get scratch document", err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleCreateScratch(w http.ResponseWriter, r *http.Request) {
	var in scratch.Doc
	if !decode(w, r, &in) {
		return
	}
	created, err := s.workspace.Scratch.Create(in)
	if err != nil {
		s.scratchError(w, "create scratch document", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateScratch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in scratch.Doc
	if !decode(w, r, &in) {
		return
	}
	updated, err := s.workspace.Scratch.Update(id, in)
	if err != nil {
		s.scratchError(w, "update scratch document", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteScratch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.workspace.Scratch.Delete(id); err != nil {
		s.scratchError(w, "delete scratch document", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// scratchError separates "there is no such document" from "what you sent is
// not a document". Everything the service rejects is one of the two; a storage
// failure arrives wrapped and reads as a bad request, which is the same
// bargain the task routes make.
func (s *Server) scratchError(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, scratch.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}
