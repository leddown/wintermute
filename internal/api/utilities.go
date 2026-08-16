package api

// The housekeeping surface: backups, diagnostics, live resource rates,
// maintenance and pruning.
//
// These are morpheus's routes rebased onto /api/v1 and behind the same bearer
// token as everything else. Morpheus checked for a signed-in user in every one
// of these handlers; here the token at the edge is that check, so the handlers
// are only the decode/delegate/encode the rest of this package is.
//
// Everything here is destructive or expensive to some degree, and none of it is
// exposed to the model as a tool. A backup writes to an operator-chosen path, a
// vacuum rewrites the database, and a prune deletes rows for good — the sort of
// thing a person triggers deliberately, not something an agent talks itself
// into. That line is the same one twire draws around its canary switches.

import (
	"errors"
	"net/http"
	"strings"

	"wintermute/internal/utilities"
)

func (s *Server) registerUtilitiesRoutes(authed func(string, http.HandlerFunc)) {
	if s.utilities == nil {
		return
	}
	authed("GET /api/v1/utilities/system-info", s.handleSystemInfo)
	authed("GET /api/v1/utilities/resources", s.handleResources)
	authed("GET /api/v1/utilities/api-usage", s.handleUtilitiesAPIUsage)
	authed("POST /api/v1/utilities/backup", s.handleBackup)
	authed("POST /api/v1/utilities/vacuum", s.handleVacuum)
	authed("POST /api/v1/utilities/prune", s.handlePrune)
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	info, err := s.utilities.SystemInfo(r.Context())
	if err != nil {
		s.fail(w, "system info", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleResources reports live CPU, network and disk throughput. Polled while a
// view is open, so it is deliberately cheap: a few /proc reads, no database
// work at all.
func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.utilities.Resources())
}

func (s *Server) handleUtilitiesAPIUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.utilities.APIUsage(r.Context())
	if err != nil {
		s.fail(w, "api usage", err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

type backupRequest struct {
	Destination string `json:"destination"`
}

// handleBackup writes a copy of the database into a timestamped subdirectory of
// the requested path.
//
// The destination is a server-side path chosen by the operator, which is worth
// saying plainly: this writes wherever the server process can write, and the
// bearer token is the only thing standing in front of it. That was true in
// morpheus too, and is the reason the route is not a tool.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	var req backupRequest
	if !decode(w, r, &req) {
		return
	}
	req.Destination = strings.TrimSpace(req.Destination)
	if req.Destination == "" {
		writeError(w, http.StatusBadRequest, "destination is required")
		return
	}

	result, err := s.utilities.Backup(r.Context(), req.Destination)
	if errors.Is(err, utilities.ErrInvalidDestination) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		// The underlying message is the useful half of a failed backup —
		// "permission denied", "no such file or directory", "database is
		// locked" — and each names a fix. A bare "internal error" names none.
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleVacuum(w http.ResponseWriter, r *http.Request) {
	result, err := s.utilities.Vacuum(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type pruneRequest struct {
	Target        string `json:"target"`
	OlderThanDays int    `json:"older_than_days"`
}

func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	var req pruneRequest
	if !decode(w, r, &req) {
		return
	}

	result, err := s.utilities.Prune(r.Context(), req.Target, req.OlderThanDays)
	switch {
	case errors.Is(err, utilities.ErrInvalidPruneTarget):
		writeError(w, http.StatusBadRequest, "unknown prune target")
		return
	case errors.Is(err, utilities.ErrInvalidDays):
		writeError(w, http.StatusBadRequest, "older_than_days must be at least 1")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
