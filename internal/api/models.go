package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"wintermute/internal/models"
	"wintermute/internal/store"
)

// defaultPlanContext is the context length assumed when a caller does not say.
// It matches the default in the guide's llama-server invocation.
const defaultPlanContext = 8192

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.catalog.Hardware(r.Context()))
}

// registerBackendAdminRoutes exposes declaring backends at runtime. Without a
// reload hook the routes are left off entirely, the same way an absent twire
// leaves its own unregistered.
func (s *Server) registerBackendAdminRoutes(authed func(string, http.HandlerFunc)) {
	if s.reloadBackends == nil {
		return
	}
	authed("POST /api/v1/backends", s.handleSaveBackend)
	authed("DELETE /api/v1/backends/{name}", s.handleDeleteBackend)
}

// backendInput is a backend declared through the UI.
//
// There is no api_key field, only api_key_env. A key reaches this process
// through its environment; accepting one here would put a credential in a
// browser POST, the request log and the database, which is exactly what
// backends.json refuses to do.
type backendInput struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
}

// backendNamePattern keeps a name usable as a path segment and recognisable in
// a session row, and stops one that only differs by whitespace or case-tricks.
var backendNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)

func (s *Server) handleSaveBackend(w http.ResponseWriter, r *http.Request) {
	var req backendInput
	if !decode(w, r, &req) {
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	req.Model = strings.TrimSpace(req.Model)
	req.APIKeyEnv = strings.TrimSpace(req.APIKeyEnv)

	if !backendNamePattern.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest,
			"name must be 1-32 characters of letters, digits, dash or underscore, starting with a letter or digit")
		return
	}
	kind := models.Kind(req.Kind)
	if !kind.Valid() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown kind %q", req.Kind))
		return
	}

	// The same rules config.resolve applies to the file, so a backend declared
	// here cannot be one the file would have rejected at startup.
	if kind == models.KindAnthropic {
		if req.APIKeyEnv == "" {
			req.APIKeyEnv = "ANTHROPIC_API_KEY"
		}
		// Checked now rather than at first use: a backend whose key is missing
		// answers nothing, and finding that out here names the fix, where
		// finding it out mid-conversation just looks like the model is broken.
		if os.Getenv(req.APIKeyEnv) == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"%s is not set on the server, so this backend could not authenticate. "+
					"Add it to the environment and restart before declaring the backend.", req.APIKeyEnv))
			return
		}
	} else if req.BaseURL == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("base_url is required for kind %q", kind))
		return
	}
	if req.BaseURL != "" {
		u, err := url.Parse(req.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeError(w, http.StatusBadRequest, "base_url must be an absolute http:// or https:// URL")
			return
		}
	}

	// A name from backends.json is not ours to redefine: the file wins at
	// resolve time, so accepting the write would store a row that never takes
	// effect and report success for a change nobody made.
	if s.fileBackend(r.Context(), req.Name) {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"%q is declared in backends.json; edit that file instead", req.Name))
		return
	}

	if err := s.store.SaveBackendConfig(r.Context(), store.BackendConfig{
		Name: req.Name, Kind: string(kind), BaseURL: req.BaseURL,
		Model: req.Model, APIKeyEnv: req.APIKeyEnv,
	}); err != nil {
		s.fail(w, "save backend", err)
		return
	}
	if err := s.reloadBackends(r.Context()); err != nil {
		s.fail(w, "reload backends", err)
		return
	}
	// Probe it straight away, so the row the UI draws next carries a real
	// verdict rather than "unknown" until the next sweep.
	if err := s.catalog.Refresh(r.Context()); err != nil {
		s.log.Warn("probe after declaring backend failed", "backend", req.Name, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": req.Name})
}

func (s *Server) handleDeleteBackend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.fileBackend(r.Context(), name) {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"%q is declared in backends.json; remove it from that file instead", name))
		return
	}
	err := s.store.DeleteBackendConfig(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "backend not found")
		return
	}
	if err != nil {
		s.fail(w, "delete backend", err)
		return
	}
	if err := s.reloadBackends(r.Context()); err != nil {
		s.fail(w, "reload backends", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fileBackend reports whether a name came from backends.json rather than the
// database. It is the difference between the two lists, which is cheaper and
// less brittle than threading the parsed config down here.
func (s *Server) fileBackend(ctx context.Context, name string) bool {
	if name == "" {
		return false
	}
	declared, err := s.store.BackendConfigs(ctx)
	if err != nil {
		return false
	}
	for _, d := range declared {
		if d.Name == name {
			return false
		}
	}
	_, live := s.agent.Router().Backend(name)
	return live
}

// backendView is a health row plus what the configuration says about the
// backend. Declared memory lives in backends.json and never reaches the
// database, so it is joined on the way out rather than persisted — which also
// means changing it is a file edit and a reload, not a migration.
type backendView struct {
	store.BackendRow
	Memory      string `json:"memory,omitempty"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"`
}

// withMemory attaches each backend's declared size to its health row. A
// backend the configuration does not mention — one declared in the database
// through the admin UI — simply has no memory, which readers must treat as
// unknown rather than as zero.
func withMemory(rows []store.BackendRow, configured []models.Backend) []backendView {
	declared := make(map[string]models.Backend, len(configured))
	for _, b := range configured {
		declared[b.Name] = b
	}
	out := make([]backendView, 0, len(rows))
	for _, row := range rows {
		view := backendView{BackendRow: row}
		if b, ok := declared[row.Name]; ok {
			view.Memory, view.MemoryBytes = b.Memory, b.MemoryBytes
		}
		out = append(out, view)
	}
	return out
}

func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	health, err := s.catalog.BackendHealth(r.Context())
	if err != nil {
		s.fail(w, "list backends", err)
		return
	}
	// declared names the backends that came from the database, so the UI can
	// offer edit and delete on those and leave the file-declared ones alone
	// rather than presenting controls that would 409.
	declared, err := s.store.BackendConfigs(r.Context())
	if err != nil {
		s.fail(w, "list declared backends", err)
		return
	}
	names := make([]string, 0, len(declared))
	for _, d := range declared {
		names = append(names, d.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backends": withMemory(health, s.catalog.Backends()),
		"default":  s.agent.Router().Default(),
		"fallback": s.agent.Router().Fallback(),
		"declared": names,
		"editable": s.reloadBackends != nil,
	})
}

func (s *Server) handleRefreshBackends(w http.ResponseWriter, r *http.Request) {
	if err := s.catalog.Refresh(r.Context()); err != nil {
		s.fail(w, "refresh backends", err)
		return
	}
	health, err := s.catalog.BackendHealth(r.Context())
	if err != nil {
		s.fail(w, "list backends", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": health})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	list, err := s.catalog.Models(r.Context(), queryInt(r, "context", defaultPlanContext))
	if err != nil {
		s.fail(w, "list models", err)
		return
	}
	if list == nil {
		list = []models.Model{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": list})
}

// The operator's own judgements about models.
//
// The model id travels in the body rather than the path. Model ids routinely
// contain both slashes and colons — "qwen3:8b", "meta-llama/Llama-3.1-8B" —
// and putting one in a path segment means every caller has to agree on how it
// was escaped. The body sidesteps it.

type modelNoteRequest struct {
	ModelID string `json:"model_id"`
	Note    string `json:"note"`
}

// handleSetModelNote records what the operator thinks of a model. An empty note
// clears it.
func (s *Server) handleSetModelNote(w http.ResponseWriter, r *http.Request) {
	var req modelNoteRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ModelID) == "" {
		writeError(w, http.StatusBadRequest, "model_id is required")
		return
	}
	const maxNote = 4000
	if len(req.Note) > maxNote {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("a note is at most %d characters", maxNote))
		return
	}
	if err := s.store.SetModelNote(r.Context(), req.ModelID, req.Note); err != nil {
		s.fail(w, "set model note", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model_id": store.NoteKey(req.ModelID),
		"note":     strings.TrimSpace(req.Note),
	})
}

type championRequest struct {
	Task    string `json:"task"`
	ModelID string `json:"model_id"`
}

// handleSetChampion names the model to reach for at one task, replacing
// whatever held the title before. An empty model id clears the task.
func (s *Server) handleSetChampion(w http.ResponseWriter, r *http.Request) {
	var req championRequest
	if !decode(w, r, &req) {
		return
	}
	// The task must be one this server knows, or a typo would create a
	// champion for a task nothing ever asks about — stored, displayed nowhere,
	// and silently useless.
	if !knownTask(req.Task) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("unknown task %q; known tasks are %s", req.Task, taskNames()))
		return
	}
	if err := s.store.SetChampion(r.Context(), req.Task, req.ModelID); err != nil {
		s.fail(w, "set champion", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task":     req.Task,
		"model_id": store.NoteKey(req.ModelID),
	})
}

// handleChampions lists every task's champion.
func (s *Server) handleChampions(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.Champions(r.Context())
	if err != nil {
		s.fail(w, "list champions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"champions": list})
}

func knownTask(task string) bool {
	for _, t := range models.AllTasks {
		if string(t) == task {
			return true
		}
	}
	return false
}

func taskNames() string {
	names := make([]string, 0, len(models.AllTasks))
	for _, t := range models.AllTasks {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}

// handleModelSearch proxies a Hugging Face Hub search.
//
// It is proxied rather than called from the browser because the Hub token, if
// one is configured, must not reach the client — and because the results are
// enriched with a fit verdict that only the server can compute.
func (s *Server) handleModelSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}

	results, err := s.catalog.Hub().Search(r.Context(), models.SearchOptions{
		Query:    query,
		Limit:    queryInt(r, "limit", 20),
		GGUFOnly: r.URL.Query().Get("gguf") != "false",
		Sort:     r.URL.Query().Get("sort"),
	})
	if err != nil {
		// The Hub being unreachable is an upstream problem, not an internal
		// one; say so rather than returning a generic 500.
		s.log.Warn("hub search failed", "error", err)
		writeError(w, http.StatusBadGateway, "could not reach the Hugging Face Hub: "+err.Error())
		return
	}
	if results == nil {
		results = []models.HubModel{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) handleModelDetail(w http.ResponseWriter, r *http.Request) {
	// The Hub id is "author/name", so it arrives as a wildcard path segment.
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "model id is required")
		return
	}

	detail, err := s.catalog.Hub().Detail(r.Context(), id,
		s.catalog.Hardware(r.Context()), queryInt(r, "context", defaultPlanContext))
	if err != nil {
		s.log.Warn("hub detail failed", "model", id, "error", err)
		writeError(w, http.StatusBadGateway, "could not fetch model: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

type planRequest struct {
	Task          string `json:"task"`
	ContextTokens int    `json:"context_tokens"`
	Priority      string `json:"priority"`
	RequireTools  bool   `json:"require_tools"`
	RequireLocal  bool   `json:"require_local"`
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req planRequest
	if !decode(w, r, &req) {
		return
	}

	plan, err := s.catalog.Recommend(r.Context(), models.PlanRequest{
		Task:          models.Task(req.Task),
		ContextTokens: req.ContextTokens,
		Priority:      models.Priority(req.Priority),
		RequireTools:  req.RequireTools,
		RequireLocal:  req.RequireLocal,
	})
	if err != nil {
		s.fail(w, "plan", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

type fitRequest struct {
	ParamsB       float64 `json:"params_b"`
	Quant         string  `json:"quant"`
	ContextTokens int     `json:"context_tokens"`
	KVCacheType   string  `json:"kv_cache_type"`
	ActiveParamsB float64 `json:"active_params_b"`
}

func (s *Server) handleFit(w http.ResponseWriter, r *http.Request) {
	var req fitRequest
	if !decode(w, r, &req) {
		return
	}
	if req.ParamsB <= 0 {
		writeError(w, http.StatusBadRequest, "params_b must be greater than zero")
		return
	}

	fit := models.EstimateFit(models.FitInput{
		ParamsB:       req.ParamsB,
		Quant:         req.Quant,
		ContextTokens: req.ContextTokens,
		KVCacheType:   req.KVCacheType,
		ActiveParamsB: req.ActiveParamsB,
	}, s.catalog.Hardware(r.Context()))
	writeJSON(w, http.StatusOK, fit)
}

// handleTasks lists the planner's task classes so the UI does not hardcode
// them alongside the server's copy.
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tasks": models.TaskCatalog})
}

func queryInt(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
