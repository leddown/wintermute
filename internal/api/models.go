package api

import (
	"net/http"
	"strconv"

	"wintermute/internal/models"
)

// defaultPlanContext is the context length assumed when a caller does not say.
// It matches the default in the guide's llama-server invocation.
const defaultPlanContext = 8192

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.catalog.Hardware(r.Context()))
}

func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	health, err := s.catalog.BackendHealth(r.Context())
	if err != nil {
		s.fail(w, "list backends", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backends": health,
		"default":  s.agent.Router().Default(),
		"fallback": s.agent.Router().Fallback(),
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
