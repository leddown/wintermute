package api

// twire's HTTP surface: the canary tripwire's control panel.
//
// The routes are morpheus's, rebased onto /api/v1 and behind the same bearer
// token as everything else. Nothing had to be unscoped on the way across —
// twire was already global there, with every signed-in user managing the same
// canaries — so what was one shared resource behind a login is now one shared
// resource behind a token.
//
// The one route that is new is the secret_configured flag on the alert config:
// morpheus always had a key to encrypt an SMTP password with, and this server
// only has one if WINTERMUTE_SECRET is set. The UI needs to say so before the
// operator types a credential into a form that will refuse it.

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"wintermute/internal/twire"
)

func (s *Server) registerTwireRoutes(authed func(string, http.HandlerFunc)) {
	if s.twire == nil {
		return
	}
	authed("GET /api/v1/twire/status", s.handleTwireStatus)
	authed("POST /api/v1/twire/canaries", s.handleCreateCanary)
	authed("DELETE /api/v1/twire/canaries/{key}", s.handleDeleteCanary)
	authed("POST /api/v1/twire/canaries/{key}/enable", s.handleEnableCanary)
	authed("POST /api/v1/twire/canaries/{key}/disable", s.handleDisableCanary)
	authed("GET /api/v1/twire/events", s.handleTwireEvents)
	authed("GET /api/v1/twire/alert-config", s.handleGetTwireAlertConfig)
	authed("PUT /api/v1/twire/alert-config", s.handleSetTwireAlertConfig)
	authed("POST /api/v1/twire/alert-config/test", s.handleTestTwireAlert)
}

type twireStatusResponse struct {
	Canaries []twire.CanaryStatus `json:"canaries"`
	// BinaryPath is what the UI puts in the setcap command it offers when a
	// canary below port 1024 could not bind. Guessing the path there and
	// getting it wrong produces a command that silently fixes nothing.
	BinaryPath string `json:"binary_path,omitempty"`
}

func (s *Server) handleTwireStatus(w http.ResponseWriter, r *http.Request) {
	canaries, err := s.twire.Status(r.Context())
	if err != nil {
		s.fail(w, "twire status", err)
		return
	}
	if canaries == nil {
		canaries = []twire.CanaryStatus{}
	}
	resp := twireStatusResponse{Canaries: canaries}
	if exe, err := os.Executable(); err == nil {
		resp.BinaryPath = exe
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleEnableCanary(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := s.twire.Enable(r.Context(), key); err != nil {
		s.failTwire(w, "enable canary", err)
		return
	}
	s.writeOneCanary(w, r, key)
}

func (s *Server) handleDisableCanary(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := s.twire.Disable(r.Context(), key); err != nil {
		s.failTwire(w, "disable canary", err)
		return
	}
	s.writeOneCanary(w, r, key)
}

// writeOneCanary responds with the current status of a single canary, so the UI
// can reflect listening or bind-error state immediately after a toggle. A bind
// failure is not an error on the toggle — enabling a port already in use is
// allowed, and shows up here as the canary's status.
func (s *Server) writeOneCanary(w http.ResponseWriter, r *http.Request, key string) {
	canaries, err := s.twire.Status(r.Context())
	if err != nil {
		s.fail(w, "twire status", err)
		return
	}
	for _, c := range canaries {
		if c.Key == key {
			writeJSON(w, http.StatusOK, c)
			return
		}
	}
	writeError(w, http.StatusNotFound, "unknown canary")
}

type createCanaryRequest struct {
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Description string `json:"description"`
	Banner      string `json:"banner"`
}

// handleCreateCanary adds an operator-defined canary on a port the built-in
// catalog does not cover. The new canary starts disabled.
func (s *Server) handleCreateCanary(w http.ResponseWriter, r *http.Request) {
	var req createCanaryRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		writeError(w, http.StatusBadRequest, "port must be between 1 and 65535")
		return
	}

	status, err := s.twire.CreateCustom(r.Context(), req.Name, req.Port, req.Description, req.Banner)
	if err != nil {
		s.failTwire(w, "create canary", err)
		return
	}
	writeJSON(w, http.StatusCreated, status)
}

// handleDeleteCanary removes an operator-defined canary. Built-in ones cannot
// be deleted, only disabled.
func (s *Server) handleDeleteCanary(w http.ResponseWriter, r *http.Request) {
	if err := s.twire.DeleteCustom(r.Context(), r.PathValue("key")); err != nil {
		s.failTwire(w, "delete canary", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTwireEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.twire.ListEvents(r.Context(), limit)
	if err != nil {
		s.fail(w, "list twire events", err)
		return
	}
	if events == nil {
		events = []twire.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

type alertConfigResponse struct {
	Enabled      bool     `json:"enabled"`
	SMTPUsername string   `json:"smtp_username"`
	From         string   `json:"from"`
	Recipients   []string `json:"recipients"`
	// PasswordSet reports only that a password is held, never what it is.
	PasswordSet bool `json:"password_set"`
	// SecretConfigured is false when WINTERMUTE_SECRET is unset, in which case
	// a password cannot be saved at all.
	SecretConfigured bool `json:"secret_configured"`
}

// handleGetTwireAlertConfig returns the effective alert configuration. The App
// Password is never returned.
func (s *Server) handleGetTwireAlertConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.twire.GetAlertConfig(r.Context())
	if err != nil {
		s.fail(w, "get twire alert config", err)
		return
	}
	recipients := cfg.Recipients
	if recipients == nil {
		recipients = []string{}
	}
	writeJSON(w, http.StatusOK, alertConfigResponse{
		Enabled:          cfg.Enabled,
		SMTPUsername:     cfg.SMTPUsername,
		From:             cfg.From,
		Recipients:       recipients,
		PasswordSet:      cfg.SMTPPassword != "",
		SecretConfigured: s.twire.SecretConfigured(),
	})
}

type alertConfigRequest struct {
	Enabled      bool     `json:"enabled"`
	SMTPUsername string   `json:"smtp_username"`
	SMTPPassword *string  `json:"smtp_password"` // omitted/null = keep existing
	From         string   `json:"from"`
	Recipients   []string `json:"recipients"`
}

func (s *Server) handleSetTwireAlertConfig(w http.ResponseWriter, r *http.Request) {
	var req alertConfigRequest
	if !decode(w, r, &req) {
		return
	}

	cfg := twire.AlertConfig{
		Enabled:      req.Enabled,
		SMTPUsername: strings.TrimSpace(req.SMTPUsername),
		From:         strings.TrimSpace(req.From),
		Recipients:   cleanRecipients(req.Recipients),
	}
	passwordSupplied := req.SMTPPassword != nil
	if passwordSupplied {
		cfg.SMTPPassword = *req.SMTPPassword
	}
	if cfg.Enabled && (cfg.From == "" || len(cfg.Recipients) == 0) {
		writeError(w, http.StatusBadRequest,
			"from address and at least one recipient are required to enable alerts")
		return
	}

	if err := s.twire.SetAlertConfig(r.Context(), cfg, passwordSupplied); err != nil {
		s.failTwire(w, "set twire alert config", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestTwireAlert(w http.ResponseWriter, r *http.Request) {
	err := s.twire.SendTestAlert(r.Context())
	if errors.Is(err, twire.ErrValidation) {
		writeError(w, http.StatusBadRequest, "email alerting is not configured and enabled")
		return
	}
	if err != nil {
		// The relay's own complaint is the useful half of a failed test, and a
		// test button that says only "failed" is a button worth nothing.
		writeError(w, http.StatusBadGateway, "failed to send test email: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// failTwire maps the module's sentinel errors onto status codes, the way
// failFintech does for the ledger.
func (s *Server) failTwire(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, twire.ErrUnknownCanary):
		writeError(w, http.StatusNotFound, "unknown canary")
	case errors.Is(err, twire.ErrNotCustom):
		writeError(w, http.StatusBadRequest, "built-in canaries cannot be deleted")
	case errors.Is(err, twire.ErrValidation):
		writeError(w, http.StatusBadRequest, "name and a valid port are required")
	case errors.Is(err, twire.ErrPortReserved):
		writeError(w, http.StatusConflict, "that port is used by a built-in canary; enable it instead")
	case errors.Is(err, twire.ErrPortTaken):
		writeError(w, http.StatusConflict, "a custom canary already uses that port")
	case errors.Is(err, twire.ErrNoSecret):
		// A missing prerequisite the operator fixes in the environment rather
		// than in the request — the same reading fintech gives its unconfigured
		// providers.
		writeError(w, http.StatusPreconditionFailed,
			"WINTERMUTE_SECRET is not set, so the SMTP password cannot be stored encrypted")
	default:
		s.fail(w, what, err)
	}
}

func cleanRecipients(in []string) []string {
	var out []string
	for _, r := range in {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}
