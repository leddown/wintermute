package fintech

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// ValidHorizons are the only forecast horizons (in days) the module
// supports — matching the fintech_forecast_horizons CHECK constraint.
var ValidHorizons = []int{3, 5, 10, 14, 21, 30, 60, 90}

// Forecast is one on-demand forecast request: a ticker plus one row per
// requested horizon.
type Forecast struct {
	ID                  int64
	InstrumentID        int64
	Symbol              string
	RequestedAt         time.Time
	ReferencePriceCents int64
	ModelName           string
	Rationale           string
	Horizons            []ForecastHorizon
	Enrichment          *ForecastEnrichment
	EnrichedAt          *time.Time
}

// ForecastHorizon is the prediction for one horizon, plus (once its
// target date has passed and EvaluateForecast has run) the scored
// actual outcome.
type ForecastHorizon struct {
	ID                   int64
	ForecastID           int64
	HorizonDays          int
	TargetDate           time.Time
	PredictedDirection   string
	PredictedLowCents    int64
	PredictedHighCents   int64
	Confidence           float64
	ActualPriceCents     *int64
	ActualDirection      *string
	WithinPredictedRange *bool
	EvaluatedAt          *time.Time
}

// ForecastEnrichment is deeper, on-demand qualitative analysis the
// assistant adds to an already-generated forecast via EnrichForecast. It
// is nil until a user explicitly asks for it.
type ForecastEnrichment struct {
	Summary             string   `json:"summary"`
	PriceRangeChallenge string   `json:"price_range_challenge"`
	Catalysts           []string `json:"catalysts"`
	Risks               []string `json:"risks"`
	SupportingSignals   []string `json:"supporting_signals"`
	ConflictingSignals  []string `json:"conflicting_signals"`
}

// emitForecastSchema is the JSON Schema for the synthetic "emit_forecast"
// tool that forces Claude into schema-validated structured output.
var emitForecastSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"horizons": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"horizon_days": {"type": "integer", "description": "One of 3,5,10,14,21,30,60,90."},
					"direction": {"type": "string", "enum": ["up", "down", "flat"], "description": "Predicted price direction relative to the reference price."},
					"predicted_low_cents": {"type": "integer", "description": "Low end of the predicted price range, in EUR cents."},
					"predicted_high_cents": {"type": "integer", "description": "High end of the predicted price range, in EUR cents."},
					"confidence": {"type": "number", "description": "Confidence in this horizon's prediction, 0.0 to 1.0."},
					"rationale": {"type": "string", "description": "Brief reasoning for this horizon."}
				},
				"required": ["horizon_days", "direction", "predicted_low_cents", "predicted_high_cents", "confidence"]
			}
		},
		"overall_rationale": {"type": "string", "description": "Overall reasoning across all horizons, citing the news considered."}
	},
	"required": ["horizons"]
}`)

type forecastToolOutput struct {
	Horizons []struct {
		HorizonDays        int     `json:"horizon_days"`
		Direction          string  `json:"direction"`
		PredictedLowCents  int64   `json:"predicted_low_cents"`
		PredictedHighCents int64   `json:"predicted_high_cents"`
		Confidence         float64 `json:"confidence"`
		Rationale          string  `json:"rationale"`
	} `json:"horizons"`
	OverallRationale string `json:"overall_rationale"`
}

// GetForecastPrompt returns the saved global forecast-prompt addition (empty
// string when nothing has been saved yet).
func (s *Service) GetForecastPrompt(ctx context.Context) (string, error) {
	return s.repo.GetForecastPrompt(ctx)
}

// SetForecastPrompt persists prompt as the global forecast-prompt addition.
func (s *Service) SetForecastPrompt(ctx context.Context, prompt string) error {
	return s.repo.SetForecastPrompt(ctx, prompt)
}

// generateForecast does everything short of persisting to the database:
// validation, market-data fetch, AI call, usage recording, and assembling
// the Forecast struct. The ID and InstrumentID fields are left zero; callers
// that want persistence must UpsertInstrument and InsertForecast themselves.
func (s *Service) generateForecast(ctx context.Context, symbol string, horizonDays []int, extraContext string) (Forecast, error) {
	symbol, err := normalizeSymbol(symbol)
	if err != nil {
		return Forecast{}, err
	}
	if len(horizonDays) == 0 {
		return Forecast{}, fmt.Errorf("%w: at least one horizon is required", ErrValidation)
	}
	for _, h := range horizonDays {
		if !validHorizon(h) {
			return Forecast{}, fmt.Errorf("%w: %d is not a valid horizon (allowed: %v)", ErrValidation, h, ValidHorizons)
		}
	}
	if s.forecaster == nil {
		return Forecast{}, fmt.Errorf("fintech: assistant is not configured; cannot generate forecasts")
	}
	if !s.marketData.Configured() {
		return Forecast{}, ErrMarketDataNotConfigured
	}

	quote, err := s.marketData.Quote(ctx, symbol)
	if err != nil {
		return Forecast{}, fmt.Errorf("fintech: fetching reference quote: %w", err)
	}
	news, err := s.marketData.News(ctx, symbol, 10)
	if err != nil {
		news = nil // news is best-effort; a forecast without it is still allowed
	}

	globalPrompt, err := s.repo.GetForecastPrompt(ctx)
	if err != nil {
		globalPrompt = "" // non-fatal; proceed without the saved addition
	}
	combinedContext := joinContext(globalPrompt, extraContext)
	prompt := buildForecastPrompt(symbol, quote, news, horizonDays, combinedContext)
	raw, usage, err := s.forecaster.CreateStructuredMessage(ctx, StructuredRequest{
		System:       forecastSystemPrompt,
		Prompt:       prompt,
		OutputName:   "emit_forecast",
		OutputSchema: emitForecastSchema,
	})
	if recErr := s.repo.RecordAIUsage(ctx, "forecast", usage); recErr != nil {
		log.Printf("fintech: failed to record forecast AI usage: %v", recErr)
	}
	if err != nil {
		return Forecast{}, fmt.Errorf("fintech: generating forecast: %w", err)
	}

	var out forecastToolOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return Forecast{}, fmt.Errorf("fintech: parsing forecast output: %w", err)
	}

	requested := strings.Join(intsToStrings(horizonDays), ",")
	now := time.Now()
	forecast := Forecast{
		Symbol:              symbol,
		RequestedAt:         now,
		ReferencePriceCents: quote.PriceCents,
		ModelName:           usage.Model,
		Rationale:           out.OverallRationale,
	}
	wanted := make(map[int]bool, len(horizonDays))
	for _, h := range horizonDays {
		wanted[h] = true
	}
	seen := make(map[int]bool)
	for _, h := range out.Horizons {
		if !wanted[h.HorizonDays] || seen[h.HorizonDays] {
			continue
		}
		seen[h.HorizonDays] = true
		forecast.Horizons = append(forecast.Horizons, ForecastHorizon{
			HorizonDays:        h.HorizonDays,
			TargetDate:         now.AddDate(0, 0, h.HorizonDays),
			PredictedDirection: normalizeDirection(h.Direction),
			PredictedLowCents:  h.PredictedLowCents,
			PredictedHighCents: h.PredictedHighCents,
			Confidence:         clampConfidence(h.Confidence),
		})
	}
	if len(forecast.Horizons) == 0 {
		return Forecast{}, fmt.Errorf("fintech: model returned no usable horizons for requested %s", requested)
	}
	return forecast, nil
}

// CreateForecast generates and persists a price-direction forecast. It is the
// normal path: call generateForecast then insert the result into the database.
// extraContext is appended to the prompt for this run only; the saved global
// prompt (see SetForecastPrompt) is fetched and prepended automatically.
func (s *Service) CreateForecast(ctx context.Context, symbol string, horizonDays []int, extraContext string) (Forecast, error) {
	forecast, err := s.generateForecast(ctx, symbol, horizonDays, extraContext)
	if err != nil {
		return Forecast{}, err
	}
	instrument, err := s.repo.UpsertInstrument(ctx, forecast.Symbol, "", AssetClassEquity)
	if err != nil {
		return Forecast{}, err
	}
	forecast.InstrumentID = instrument.ID
	return s.repo.InsertForecast(ctx, forecast)
}

// PreviewForecast generates a forecast without saving it to the database.
// The returned Forecast has ID 0. AI usage is still recorded.
func (s *Service) PreviewForecast(ctx context.Context, symbol string, horizonDays []int, extraContext string) (Forecast, error) {
	return s.generateForecast(ctx, symbol, horizonDays, extraContext)
}

// CommitForecast persists a previously-previewed Forecast (ID == 0) to the
// database. It does not call the AI again; the caller supplies the struct
// returned by PreviewForecast. The forecast is always re-derived from the
// request context, not from the supplied struct.
func (s *Service) CommitForecast(ctx context.Context, f Forecast) (Forecast, error) {
	if f.Symbol == "" {
		return Forecast{}, fmt.Errorf("%w: symbol is required", ErrValidation)
	}
	if len(f.Horizons) == 0 {
		return Forecast{}, fmt.Errorf("%w: at least one horizon is required", ErrValidation)
	}
	instrument, err := s.repo.UpsertInstrument(ctx, f.Symbol, "", AssetClassEquity)
	if err != nil {
		return Forecast{}, err
	}
	f.InstrumentID = instrument.ID
	return s.repo.InsertForecast(ctx, f)
}

// emitForecastEnrichmentSchema is the JSON Schema for the synthetic
// "emit_forecast_enrichment" tool used by EnrichForecast.
var emitForecastEnrichmentSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "description": "A few sentences of deeper written analysis expanding on the existing forecast."},
		"price_range_challenge": {"type": "string", "description": "Critically assess the predicted price range for each horizon. State whether you agree with the range or challenge it — if you disagree, explain whether the range should be wider, narrower, higher, or lower and why, citing specific evidence from the news or market context. Be direct: do not simply restate the range."},
		"catalysts": {"type": "array", "items": {"type": "string"}, "description": "Specific upcoming events or catalysts that could move the price within the forecast horizons."},
		"risks": {"type": "array", "items": {"type": "string"}, "description": "Key risks that could invalidate the forecast."},
		"supporting_signals": {"type": "array", "items": {"type": "string"}, "description": "Evidence or reasoning that supports the predicted direction(s)."},
		"conflicting_signals": {"type": "array", "items": {"type": "string"}, "description": "Evidence or reasoning that conflicts with the predicted direction(s)."}
	},
	"required": ["summary", "price_range_challenge"]
}`)

type forecastEnrichmentToolOutput struct {
	Summary             string   `json:"summary"`
	PriceRangeChallenge string   `json:"price_range_challenge"`
	Catalysts           []string `json:"catalysts"`
	Risks               []string `json:"risks"`
	SupportingSignals   []string `json:"supporting_signals"`
	ConflictingSignals  []string `json:"conflicting_signals"`
}

const forecastEnrichmentSystemPrompt = `You are a financial analyst adding deeper qualitative analysis to a price-direction forecast that has already been made. You are given the forecast's symbol, reference price, predicted horizons (each with a predicted direction and price range), and any recent news. Your job has two parts: (1) critically evaluate the predicted price range for each horizon — either confirm it with evidence or challenge it by arguing the range should be wider, narrower, higher, or lower, citing specific news or market context; (2) identify concrete catalysts, risks, and signals that support or conflict with the predicted direction(s). Be direct and specific. These are speculative estimates, not financial advice. Respond only by calling the emit_forecast_enrichment tool.`

// EnrichForecast asks the assistant to add deeper qualitative analysis —
// catalysts, risks, and supporting/conflicting signals — to an existing
// forecast, on top of the direction/range/confidence numbers CreateForecast
// already produced. It re-fetches news best-effort for fresher context but,
// unlike CreateForecast, does not require a market data provider: the
// existing forecast's own data is enough to enrich.
func (s *Service) EnrichForecast(ctx context.Context, forecastID int64) (Forecast, error) {
	forecast, err := s.repo.GetForecast(ctx, forecastID)
	if err != nil {
		return Forecast{}, err
	}
	if s.forecaster == nil {
		return Forecast{}, fmt.Errorf("fintech: assistant is not configured; cannot enrich forecasts")
	}

	var news []NewsItem
	if s.marketData.Configured() {
		news, _ = s.marketData.News(ctx, forecast.Symbol, 10) // best-effort, as in CreateForecast
	}

	prompt := buildForecastEnrichmentPrompt(forecast, news)
	raw, usage, err := s.forecaster.CreateStructuredMessage(ctx, StructuredRequest{
		System:       forecastEnrichmentSystemPrompt,
		Prompt:       prompt,
		OutputName:   "emit_forecast_enrichment",
		OutputSchema: emitForecastEnrichmentSchema,
	})
	if recErr := s.repo.RecordAIUsage(ctx, "enrichment", usage); recErr != nil {
		log.Printf("fintech: failed to record enrichment AI usage: %v", recErr)
	}
	if err != nil {
		return Forecast{}, fmt.Errorf("fintech: enriching forecast: %w", err)
	}

	var out forecastEnrichmentToolOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return Forecast{}, fmt.Errorf("fintech: parsing forecast enrichment: %w", err)
	}

	enrichment := ForecastEnrichment(out)
	enrichedAt := time.Now()
	if err := s.repo.UpdateForecastEnrichment(ctx, forecast.ID, enrichment, enrichedAt); err != nil {
		return Forecast{}, err
	}
	forecast.Enrichment = &enrichment
	forecast.EnrichedAt = &enrichedAt
	return forecast, nil
}

// GetAIUsageSummary returns fintech's global (not per-user) Anthropic API
// usage from generating and enriching forecasts.
func (s *Service) GetAIUsageSummary(ctx context.Context) (AIUsage, error) {
	return s.repo.GetAIUsageSummary(ctx)
}

// ListForecasts returns userID's forecasts, most recent first, optionally
// filtered to one symbol.
func (s *Service) ListForecasts(ctx context.Context, symbol string, limit int) ([]Forecast, error) {
	return s.repo.ListForecasts(ctx, strings.ToUpper(strings.TrimSpace(symbol)), limit)
}

// GetForecast returns one forecast (with its horizons) owned by userID.
func (s *Service) GetForecast(ctx context.Context, forecastID int64) (Forecast, error) {
	return s.repo.GetForecast(ctx, forecastID)
}

// DeleteForecast permanently removes a forecast and all its horizons.
func (s *Service) DeleteForecast(ctx context.Context, forecastID int64) error {
	return s.repo.DeleteForecast(ctx, forecastID)
}

// EvaluateForecast scores any of forecastID's horizons whose target date
// has passed and that haven't been evaluated yet, by comparing the
// reference price to the actual current price from the market data
// provider, then returns the updated forecast. Like CreateForecast it
// takes no chat-specific arguments.
func (s *Service) EvaluateForecast(ctx context.Context, forecastID int64) (Forecast, error) {
	forecast, err := s.repo.GetForecast(ctx, forecastID)
	if err != nil {
		return Forecast{}, err
	}
	if !s.marketData.Configured() {
		return Forecast{}, ErrMarketDataNotConfigured
	}

	quote, err := s.marketData.Quote(ctx, forecast.Symbol)
	if err != nil {
		return Forecast{}, fmt.Errorf("fintech: fetching actual price: %w", err)
	}
	now := time.Now()
	for i := range forecast.Horizons {
		h := &forecast.Horizons[i]
		if h.EvaluatedAt != nil || h.TargetDate.After(now) {
			continue // not due yet, or already scored
		}
		actualPrice := quote.PriceCents
		actualDir := directionOf(forecast.ReferencePriceCents, actualPrice)
		within := actualPrice >= h.PredictedLowCents && actualPrice <= h.PredictedHighCents
		if err := s.repo.UpdateHorizonOutcome(ctx, h.ID, actualPrice, actualDir, within, now); err != nil {
			return Forecast{}, err
		}
		h.ActualPriceCents = &actualPrice
		h.ActualDirection = &actualDir
		h.WithinPredictedRange = &within
		h.EvaluatedAt = &now
	}
	return forecast, nil
}

const forecastSystemPrompt = `You are a financial analyst producing short-term price-direction forecasts for stocks and ETFs. You are given a reference price and recent news headlines. For each requested horizon, predict a direction (up/down/flat relative to the reference price), a plausible price range in EUR cents, and a calibrated confidence between 0 and 1. Base predictions on the supplied news and general market reasoning. These are speculative estimates, not financial advice. Respond only by calling the emit_forecast tool.`

func buildForecastPrompt(symbol string, quote Quote, news []NewsItem, horizonDays []int, extraContext string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Forecast the price direction of %s.\n", symbol)
	fmt.Fprintf(&b, "Reference price: %d cents (as of %s).\n", quote.PriceCents, quote.AsOf.Format(time.RFC3339))
	fmt.Fprintf(&b, "Requested horizons (days): %s.\n", strings.Join(intsToStrings(horizonDays), ", "))
	if len(news) == 0 {
		b.WriteString("No recent news was available.\n")
	} else {
		b.WriteString("Recent news headlines:\n")
		for _, n := range news {
			fmt.Fprintf(&b, "- %s (%s, %s)\n", n.Headline, n.Source, n.PublishedAt.Format("2006-01-02"))
		}
	}
	if extraContext != "" {
		b.WriteString("\nAdditional context:\n")
		b.WriteString(extraContext)
		b.WriteString("\n")
	}
	b.WriteString("Produce one entry per requested horizon.")
	return b.String()
}

// joinContext merges a global saved prompt and a per-run override into one
// string. Either may be empty.
func joinContext(global, perRun string) string {
	global = strings.TrimSpace(global)
	perRun = strings.TrimSpace(perRun)
	switch {
	case global == "":
		return perRun
	case perRun == "":
		return global
	default:
		return global + "\n" + perRun
	}
}

func buildForecastEnrichmentPrompt(f Forecast, news []NewsItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Enrich and critically evaluate this forecast for %s.\n", f.Symbol)
	fmt.Fprintf(&b, "Reference price: %d cents (as of %s).\n", f.ReferencePriceCents, f.RequestedAt.Format(time.RFC3339))
	if f.Rationale != "" {
		fmt.Fprintf(&b, "Original rationale: %s\n", f.Rationale)
	}
	b.WriteString("Predicted horizons (direction, price range in cents, confidence):\n")
	for _, h := range f.Horizons {
		fmt.Fprintf(&b, "- %dd: %s, €%.2f–€%.2f (%d–%d cents), confidence %.0f%%\n",
			h.HorizonDays, h.PredictedDirection,
			float64(h.PredictedLowCents)/100, float64(h.PredictedHighCents)/100,
			h.PredictedLowCents, h.PredictedHighCents,
			h.Confidence*100)
	}
	b.WriteString("For each horizon, state whether you agree with the predicted price range or challenge it. ")
	b.WriteString("If you challenge it, explain whether it should be wider, narrower, higher, or lower and why.\n")
	if len(news) == 0 {
		b.WriteString("No recent news was available.\n")
	} else {
		b.WriteString("Recent news headlines:\n")
		for _, n := range news {
			fmt.Fprintf(&b, "- %s (%s, %s)\n", n.Headline, n.Source, n.PublishedAt.Format("2006-01-02"))
		}
	}
	return b.String()
}

func validHorizon(h int) bool {
	for _, v := range ValidHorizons {
		if v == h {
			return true
		}
	}
	return false
}

func normalizeDirection(d string) string {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "up":
		return "up"
	case "down":
		return "down"
	default:
		return "flat"
	}
}

func directionOf(reference, actual int64) string {
	switch {
	case actual > reference:
		return "up"
	case actual < reference:
		return "down"
	default:
		return "flat"
	}
}

func clampConfidence(c float64) float64 {
	if c < 0 {
		return 0
	}
	if c > 1 {
		return 1
	}
	return c
}

func intsToStrings(ints []int) []string {
	out := make([]string, len(ints))
	for i, v := range ints {
		out[i] = fmt.Sprintf("%d", v)
	}
	return out
}
