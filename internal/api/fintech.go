package api

// The investment ledger's HTTP surface.
//
// The routes are morpheus's, minus the ones that no longer mean anything here:
// there is no per-user scoping to enforce (the token at the edge is the
// boundary), and the market-data provider is configured from the environment
// rather than typed into a settings page, so its config route is a read-only
// status instead of a write.

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wintermute/internal/fintech"
)

func (s *Server) registerFintechRoutes(authed func(string, http.HandlerFunc)) {
	if s.workspace.Fintech == nil {
		return
	}
	authed("GET /api/v1/fintech/status", s.handleFintechStatus)
	authed("GET /api/v1/fintech/holdings", s.handleListHoldings)
	authed("GET /api/v1/fintech/portfolio", s.handlePortfolioSummary)
	authed("GET /api/v1/fintech/trades", s.handleListTrades)
	authed("POST /api/v1/fintech/trades", s.handleRecordTrade)
	authed("POST /api/v1/fintech/orders/simulated", s.handlePlaceSimulatedOrder)
	authed("POST /api/v1/fintech/kraken/sync", s.handleKrakenSync)
	authed("POST /api/v1/fintech/import/preview", s.handleImportPreview)
	authed("POST /api/v1/fintech/import/confirm", s.handleImportConfirm)

	authed("GET /api/v1/fintech/forecasts", s.handleListForecasts)
	authed("POST /api/v1/fintech/forecasts", s.handleCreateForecast)
	authed("POST /api/v1/fintech/forecasts/preview", s.handlePreviewForecast)
	authed("POST /api/v1/fintech/forecasts/commit", s.handleCommitForecast)
	authed("GET /api/v1/fintech/forecasts/{id}", s.handleGetForecast)
	authed("DELETE /api/v1/fintech/forecasts/{id}", s.handleDeleteForecast)
	authed("POST /api/v1/fintech/forecasts/{id}/evaluate", s.handleEvaluateForecast)
	authed("POST /api/v1/fintech/forecasts/{id}/enrich", s.handleEnrichForecast)
	authed("GET /api/v1/fintech/forecast-prompt", s.handleGetForecastPrompt)
	authed("PUT /api/v1/fintech/forecast-prompt", s.handleSetForecastPrompt)

	authed("GET /api/v1/fintech/watchlist", s.handleListWatchlist)
	authed("POST /api/v1/fintech/watchlist", s.handleAddWatchlist)
	authed("DELETE /api/v1/fintech/watchlist/{symbol}", s.handleRemoveWatchlist)

	authed("GET /api/v1/fintech/reviews", s.handleListReviews)
	authed("POST /api/v1/fintech/reviews/run", s.handleRunReviews)
	authed("GET /api/v1/fintech/review-config", s.handleGetReviewConfig)
	authed("PUT /api/v1/fintech/review-config", s.handleSetReviewConfig)

	authed("GET /api/v1/fintech/ai-usage", s.handleFintechAIUsage)
}

// failFintech maps the module's sentinel errors onto status codes. Validation
// and duplication are the caller's problem; everything else is the server's.
func (s *Server) failFintech(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, fintech.ErrValidation):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, fintech.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, fintech.ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, fintech.ErrMarketDataNotConfigured),
		errors.Is(err, fintech.ErrBrokerNotConfigured),
		errors.Is(err, fintech.ErrNoForecaster):
		// Not a failure so much as a missing prerequisite, and one the operator
		// fixes in the environment rather than in the request.
		writeError(w, http.StatusPreconditionFailed, err.Error())
	default:
		s.fail(w, what, err)
	}
}

// handleFintechStatus reports what the portfolio can currently do: which
// outside services are configured, and how much is in the ledger. It is what
// the UI reads first, so a page can say "no market data provider configured"
// instead of showing zeroes.
func (s *Server) handleFintechStatus(w http.ResponseWriter, r *http.Request) {
	svc := s.workspace.Fintech
	summary, err := svc.GetPortfolioSummary(r.Context())
	if err != nil {
		s.failFintech(w, "portfolio status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"market_data_provider":   svc.MarketDataProviderName(),
		"market_data_configured": summary.MarketDataConfigured,
		"kraken_configured":      svc.KrakenConfigured(),
		"broker_configured":      svc.BrokerConfigured(),
		"forecasting_configured": svc.ForecastingConfigured(),
		"holdings":               len(summary.Holdings),
		"total_cost_cents":       summary.TotalCostCents,
		"total_value_cents":      summary.TotalValueCents,
		"realized_pl_cents":      summary.RealizedPLCents,
	})
}

func (s *Server) handleListHoldings(w http.ResponseWriter, r *http.Request) {
	holdings, err := s.workspace.Fintech.ListHoldings(r.Context())
	if err != nil {
		s.failFintech(w, "list holdings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"holdings": holdings})
}

func (s *Server) handlePortfolioSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.workspace.Fintech.GetPortfolioSummary(r.Context())
	if err != nil {
		s.failFintech(w, "portfolio summary", err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleListTrades(w http.ResponseWriter, r *http.Request) {
	trades, err := s.workspace.Fintech.ListTransactions(r.Context())
	if err != nil {
		s.failFintech(w, "list trades", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trades": trades})
}

type recordTradeRequest struct {
	Symbol     string `json:"symbol"`
	AssetClass string `json:"asset_class"`
	Side       string `json:"side"`
	Quantity   string `json:"quantity"`
	PriceCents int64  `json:"price_cents"`
	FeeCents   int64  `json:"fee_cents"`
	ExecutedAt string `json:"executed_at"`
	Notes      string `json:"notes"`
}

func (s *Server) handleRecordTrade(w http.ResponseWriter, r *http.Request) {
	var req recordTradeRequest
	if !decode(w, r, &req) {
		return
	}
	in := fintech.RecordTradeInput{
		Symbol:     req.Symbol,
		AssetClass: fintech.AssetClass(req.AssetClass),
		Side:       fintech.Side(req.Side),
		Quantity:   req.Quantity,
		PriceCents: req.PriceCents,
		FeeCents:   req.FeeCents,
		Notes:      req.Notes,
	}
	if req.ExecutedAt != "" {
		t, err := time.Parse(time.RFC3339, req.ExecutedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "executed_at must be an RFC3339 timestamp")
			return
		}
		in.ExecutedAt = t
	}
	txn, err := s.workspace.Fintech.RecordTrade(r.Context(), in)
	if err != nil {
		s.failFintech(w, "record trade", err)
		return
	}
	writeJSON(w, http.StatusCreated, txn)
}

type simulatedOrderRequest struct {
	Symbol   string `json:"symbol"`
	Side     string `json:"side"`
	Quantity string `json:"quantity"`
}

func (s *Server) handlePlaceSimulatedOrder(w http.ResponseWriter, r *http.Request) {
	var req simulatedOrderRequest
	if !decode(w, r, &req) {
		return
	}
	txn, err := s.workspace.Fintech.PlaceSimulatedOrder(r.Context(), req.Symbol, fintech.Side(req.Side), req.Quantity)
	if err != nil {
		s.failFintech(w, "place simulated order", err)
		return
	}
	writeJSON(w, http.StatusCreated, txn)
}

func (s *Server) handleKrakenSync(w http.ResponseWriter, r *http.Request) {
	imported, err := s.workspace.Fintech.SyncKraken(r.Context())
	if err != nil {
		s.failFintech(w, "kraken sync", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": imported})
}

// handleImportPreview takes a CSV upload and returns the parsed preview plus an
// upload id, which handleImportConfirm then commits. The two-step shape is what
// lets a person see how their broker's columns were read before any of it
// reaches the ledger.
func (s *Server) handleImportPreview(w http.ResponseWriter, r *http.Request) {
	// Bounded before anything reads it: a CSV of trades is small, and something
	// this size is not one.
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "expected a multipart upload with a file field")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "a file field is required")
		return
	}
	defer file.Close()

	content, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the uploaded file")
		return
	}

	preview, err := s.workspace.Fintech.PreviewImport(r.Context(), header.Filename, content)
	if err != nil {
		s.failFintech(w, "import preview", err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

type importConfirmRequest struct {
	UploadID string                `json:"upload_id"`
	Mapping  fintech.ColumnMapping `json:"mapping"`
}

func (s *Server) handleImportConfirm(w http.ResponseWriter, r *http.Request) {
	var req importConfirmRequest
	if !decode(w, r, &req) {
		return
	}
	result, err := s.workspace.Fintech.ConfirmImport(r.Context(), req.UploadID, req.Mapping)
	if err != nil {
		s.failFintech(w, "import confirm", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type forecastRequest struct {
	Symbol      string `json:"symbol"`
	HorizonDays []int  `json:"horizon_days"`
	Context     string `json:"context"`
}

func (s *Server) handleCreateForecast(w http.ResponseWriter, r *http.Request) {
	var req forecastRequest
	if !decode(w, r, &req) {
		return
	}
	f, err := s.workspace.Fintech.CreateForecast(r.Context(), req.Symbol, req.HorizonDays, req.Context)
	if err != nil {
		s.failFintech(w, "create forecast", err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

// handlePreviewForecast generates a forecast without storing it, so it can be
// read and thrown away. The commit route is what writes one.
func (s *Server) handlePreviewForecast(w http.ResponseWriter, r *http.Request) {
	var req forecastRequest
	if !decode(w, r, &req) {
		return
	}
	f, err := s.workspace.Fintech.PreviewForecast(r.Context(), req.Symbol, req.HorizonDays, req.Context)
	if err != nil {
		s.failFintech(w, "preview forecast", err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleCommitForecast(w http.ResponseWriter, r *http.Request) {
	var f fintech.Forecast
	if !decode(w, r, &f) {
		return
	}
	saved, err := s.workspace.Fintech.CommitForecast(r.Context(), f)
	if err != nil {
		s.failFintech(w, "commit forecast", err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) handleListForecasts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	forecasts, err := s.workspace.Fintech.ListForecasts(r.Context(), strings.ToUpper(r.URL.Query().Get("symbol")), limit)
	if err != nil {
		s.failFintech(w, "list forecasts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"forecasts": forecasts})
}

// fintechID reads the {id} path value, writing the error itself when it is not
// a number.
func fintechID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return 0, false
	}
	return id, true
}

func (s *Server) handleGetForecast(w http.ResponseWriter, r *http.Request) {
	id, ok := fintechID(w, r)
	if !ok {
		return
	}
	f, err := s.workspace.Fintech.GetForecast(r.Context(), id)
	if err != nil {
		s.failFintech(w, "get forecast", err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleDeleteForecast(w http.ResponseWriter, r *http.Request) {
	id, ok := fintechID(w, r)
	if !ok {
		return
	}
	if err := s.workspace.Fintech.DeleteForecast(r.Context(), id); err != nil {
		s.failFintech(w, "delete forecast", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEvaluateForecast(w http.ResponseWriter, r *http.Request) {
	id, ok := fintechID(w, r)
	if !ok {
		return
	}
	f, err := s.workspace.Fintech.EvaluateForecast(r.Context(), id)
	if err != nil {
		s.failFintech(w, "evaluate forecast", err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleEnrichForecast(w http.ResponseWriter, r *http.Request) {
	id, ok := fintechID(w, r)
	if !ok {
		return
	}
	f, err := s.workspace.Fintech.EnrichForecast(r.Context(), id)
	if err != nil {
		s.failFintech(w, "enrich forecast", err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleGetForecastPrompt(w http.ResponseWriter, r *http.Request) {
	prompt, err := s.workspace.Fintech.GetForecastPrompt(r.Context())
	if err != nil {
		s.failFintech(w, "get forecast prompt", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prompt": prompt})
}

func (s *Server) handleSetForecastPrompt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt string `json:"prompt"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.workspace.Fintech.SetForecastPrompt(r.Context(), req.Prompt); err != nil {
		s.failFintech(w, "set forecast prompt", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListWatchlist(w http.ResponseWriter, r *http.Request) {
	entries, err := s.workspace.Fintech.ListWatchlist(r.Context())
	if err != nil {
		s.failFintech(w, "list watchlist", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"watchlist": entries})
}

type watchlistRequest struct {
	Symbol      string `json:"symbol"`
	AssetClass  string `json:"asset_class"`
	HorizonDays []int  `json:"horizon_days"`
}

func (s *Server) handleAddWatchlist(w http.ResponseWriter, r *http.Request) {
	var req watchlistRequest
	if !decode(w, r, &req) {
		return
	}
	entry, err := s.workspace.Fintech.AddToWatchlist(r.Context(), req.Symbol, req.HorizonDays, fintech.AssetClass(req.AssetClass))
	if err != nil {
		s.failFintech(w, "add to watchlist", err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) handleRemoveWatchlist(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")
	if err := s.workspace.Fintech.RemoveFromWatchlist(r.Context(), symbol); err != nil {
		s.failFintech(w, "remove from watchlist", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListReviews(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	reviews, err := s.workspace.Fintech.ListReviews(r.Context(), limit)
	if err != nil {
		s.failFintech(w, "list reviews", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": reviews})
}

// handleRunReviews runs a review cycle now rather than waiting for the
// scheduler. It is deliberately synchronous: it costs one model call per
// symbol, and a caller that started it should be the one waiting for it.
func (s *Server) handleRunReviews(w http.ResponseWriter, r *http.Request) {
	reviewed, err := s.workspace.Fintech.RunReviewCycle(r.Context())
	if err != nil {
		s.failFintech(w, "run reviews", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviewed": reviewed})
}

func (s *Server) handleGetReviewConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.workspace.Fintech.GetReviewConfig(r.Context())
	if err != nil {
		s.failFintech(w, "get review config", err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleSetReviewConfig(w http.ResponseWriter, r *http.Request) {
	var cfg fintech.ReviewConfig
	if !decode(w, r, &cfg) {
		return
	}
	if err := s.workspace.Fintech.SetReviewConfig(r.Context(), cfg); err != nil {
		s.failFintech(w, "set review config", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFintechAIUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.workspace.Fintech.GetAIUsageSummary(r.Context())
	if err != nil {
		s.failFintech(w, "fintech ai usage", err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}
