package api

// The model repository: weights the operator keeps on a disk the server owns.
//
// Like the utilities and twire surfaces, none of this is exposed to the model
// as a tool. Downloading gigabytes over somebody's connection and deleting
// files off their drive are operator decisions, and the read-only-by-
// construction rule that lets the agent loop auto-approve server tools does not
// stretch to cover either. The line is the same one drawn around load and
// unload in models.go.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"wintermute/internal/modelrepo"
	"wintermute/internal/store"
)

// deleteConfirmation is what the operator must type to delete weights.
//
// Server-side, like the memory wipe's confirmation, because a check that lives
// only in the browser is not a check. A model file is tens of gigabytes and an
// hour of downloading, and a misplaced click should not be enough to lose it.
const deleteConfirmation = "delete"

func (s *Server) registerModelRepoRoutes(authed func(string, http.HandlerFunc)) {
	if s.modelRepo == nil {
		return
	}
	authed("GET /api/v1/repo", s.handleRepoList)
	authed("POST /api/v1/repo/init", s.handleRepoInit)
	authed("POST /api/v1/repo/download", s.handleRepoDownload)
	authed("GET /api/v1/repo/jobs", s.handleRepoJobs)
	authed("POST /api/v1/repo/jobs/{id}/cancel", s.handleRepoCancel)
	authed("POST /api/v1/repo/delete", s.handleRepoDelete)
	// Serving weights to fleet nodes. A GET, cacheable and resumable, because
	// the thing on the other end is an agent fetching gigabytes over a home
	// network — see handleRepoFile.
	authed("GET /api/v1/repo/file/{path...}", s.handleRepoFile)
	authed("POST /api/v1/repo/tags", s.handleRepoAddTag)
	authed("POST /api/v1/repo/tags/remove", s.handleRepoRemoveTag)
}

// handleRepoList reports the repository's state and its contents in one read,
// which is what the page needs to draw itself and is one request rather than
// three.
func (s *Server) handleRepoList(w http.ResponseWriter, r *http.Request) {
	status := s.modelRepo.Status(r.Context())
	out := map[string]any{"status": status, "files": []modelrepo.Entry{}, "tags": []string{}}

	if status.Available {
		// Fit is graded against the server's own hardware. That is the right
		// answer only when the server is also what runs the models; the
		// hardware panel already says which, so the number is offered rather
		// than hidden.
		files, err := s.modelRepo.List(r.Context(), s.catalog.Hosts(r.Context()))
		if err != nil {
			s.fail(w, "list model repository", err)
			return
		}
		out["files"] = files

		// The tag vocabulary in use, so the UI can offer what already exists
		// instead of inviting a fresh synonym every time.
		seen := map[string]bool{}
		vocab := []string{}
		for _, f := range files {
			for _, t := range f.Tags {
				if !seen[t] {
					seen[t] = true
					vocab = append(vocab, t)
				}
			}
		}
		out["tags"] = vocab
	}
	out["jobs"] = s.modelRepo.Jobs().List()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRepoInit(w http.ResponseWriter, r *http.Request) {
	if err := s.modelRepo.Initialise(); err != nil {
		s.failRepo(w, "initialise model repository", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": s.modelRepo.Status(r.Context())})
}

func (s *Server) handleRepoDownload(w http.ResponseWriter, r *http.Request) {
	var req modelrepo.Request
	if !decode(w, r, &req) {
		return
	}
	job, err := s.modelRepo.Downloader().Start(r.Context(), req)
	if err != nil {
		s.failRepo(w, "start model download", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

// handleRepoJobs is polled while a download runs, so it does no database work
// and no filesystem walk — just the in-memory registry.
func (s *Server) handleRepoJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.modelRepo.Jobs().List()})
}

func (s *Server) handleRepoCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.modelRepo.Jobs().Cancel(r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.modelRepo.Jobs().List()})
}

type repoDeleteRequest struct {
	RelPath string `json:"rel_path"`
	Confirm string `json:"confirm"`
}

// handleRepoDelete removes weights from the drive. POST rather than DELETE
// because it carries a confirmation body, and a DELETE with a meaningful body
// is a request several intermediaries feel free to strip.
func (s *Server) handleRepoDelete(w http.ResponseWriter, r *http.Request) {
	var req repoDeleteRequest
	if !decode(w, r, &req) {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(req.Confirm), deleteConfirmation) {
		writeError(w, http.StatusBadRequest,
			"type "+deleteConfirmation+" to confirm — this erases the weights from the drive "+
				"and they would have to be downloaded again")
		return
	}
	if err := s.modelRepo.Delete(r.Context(), req.RelPath); err != nil {
		s.failRepo(w, "delete model", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": store.RepoKey(req.RelPath)})
}

// handleRepoFile streams one set of weights to a fleet node.
//
// http.ServeContent rather than io.Copy, for one reason that matters: it
// implements Range. A node fetching twelve gigabytes over a domestic network
// will be interrupted, and its agent resumes with a Range request exactly as
// this server's own downloader does against Hugging Face. Serving the whole
// file from zero on every attempt would make a flaky link into a model that
// never arrives.
//
// It is a plain authenticated GET. Any client with a token may read the
// repository, which is the same reach the Repository screen already has; the
// node-only check that guards reporting is about *writing* telemetry
// attributed to a machine, and does not apply to reading a file.
func (s *Server) handleRepoFile(w http.ResponseWriter, r *http.Request) {
	path, info, err := s.modelRepo.Open(r.PathValue("path"))
	if err != nil {
		s.failRepo(w, "open repository file", err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.fail(w, "open repository file", err)
		return
	}
	defer func() { _ = f.Close() }()

	// Named as the operator's repository names it, so a file fetched by hand
	// with curl -O lands with a sensible name.
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

type repoTagRequest struct {
	RelPath string `json:"rel_path"`
	Tag     string `json:"tag"`
}

func (s *Server) handleRepoAddTag(w http.ResponseWriter, r *http.Request) {
	var req repoTagRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.store.AddTag(r.Context(), req.RelPath, req.Tag); err != nil {
		s.failRepo(w, "add tag", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tag": store.TagKey(req.Tag)})
}

func (s *Server) handleRepoRemoveTag(w http.ResponseWriter, r *http.Request) {
	var req repoTagRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.store.RemoveTag(r.Context(), req.RelPath, req.Tag); err != nil {
		s.failRepo(w, "remove tag", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": store.TagKey(req.Tag)})
}

// failRepo distinguishes the operator's problem from the server's.
//
// An unplugged drive, a directory with no marker, a gated repository, a full
// disk: every one of those is a fact about the world the operator can act on,
// and answering all of them with "internal error" — which is the right answer
// for a genuine fault — would make the whole feature undiagnosable.
func (s *Server) failRepo(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, modelrepo.ErrNotConfigured),
		errors.Is(err, modelrepo.ErrUnavailable),
		errors.Is(err, modelrepo.ErrOutsideRepo),
		errors.Is(err, modelrepo.ErrInvalidRequest),
		errors.Is(err, modelrepo.ErrNotWritable):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var running modelrepo.ErrAlreadyRunning
	if errors.As(err, &running) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.fail(w, what, err)
}
