package fintech

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"wintermute/internal/tool"
)

// Register exposes the portfolio to the assistant.
//
// This is what morpheus's own assistant page did, arriving here as tools on the
// agent that already exists — the same move the task and accounting modules
// made when they came across. Each handler is a thin adapter over the Service
// methods the HTTP layer calls, so a trade the model records goes through the
// same validation, the same dedupe hash and the same ledger as one typed in.
//
// CSV import and Kraken sync are deliberately absent. Those are credential and
// file-upload flows; they stay in the API where a person drives them, out of
// the model's reach.
//
// Morpheus scoped every one of these to the authenticated user. There is one
// portfolio here, so what used to be an ownership check is now the token check
// at the API edge — see the note in repository.go.
func Register(reg *tool.Registry, svc *Service) error {
	for _, r := range []registration{
		recordTradeTool(svc),
		listHoldingsTool(svc),
		portfolioSummaryTool(svc),
		createForecastTool(svc),
		listForecastsTool(svc),
		getForecastTool(svc),
		evaluateForecastTool(svc),
		enrichForecastTool(svc),
		placeSimulatedOrderTool(svc),
		addToWatchlistTool(svc),
		listWatchlistTool(svc),
		removeFromWatchlistTool(svc),
		listPositionReviewsTool(svc),
	} {
		if err := reg.Register(r.def, r.handler); err != nil {
			return err
		}
	}
	return nil
}

// registration pairs a tool with the handler that runs it.
type registration struct {
	def     tool.Definition
	handler tool.Handler
}

// jsonHandler adapts a handler that returns a value to the registry's, which
// returns the string the model sees. Marshalling in one place is what keeps
// every tool's result shaped the same way.
func jsonHandler(fn func(ctx context.Context, raw json.RawMessage) (any, error)) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		v, err := fn(ctx, raw)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("encode tool result: %w", err)
		}
		return string(data), nil
	}
}

func recordTradeTool(svc *Service) registration {
	type input struct {
		Symbol     string `json:"symbol"`
		AssetClass string `json:"asset_class"`
		Side       string `json:"side"`
		Quantity   string `json:"quantity"`
		PriceCents int64  `json:"price_cents"`
		FeeCents   int64  `json:"fee_cents"`
		ExecutedAt string `json:"executed_at"`
	}
	return registration{
		def: tool.Definition{
			Name:        "record_trade",
			Risk:        tool.RiskWrite,
			Description: "Record a stock/ETF/crypto trade the user made (manually, not executed by morpheus). Prices and fees are in USD cents.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"symbol": {"type": "string", "description": "Ticker symbol, e.g. AAPL or BTC."},
					"asset_class": {"type": "string", "enum": ["equity", "etf", "crypto"], "description": "Defaults to equity if omitted."},
					"side": {"type": "string", "enum": ["buy", "sell"]},
					"quantity": {"type": "string", "description": "Number of shares/units as a decimal string, e.g. \"1.5\"."},
					"price_cents": {"type": "integer", "description": "Per-unit price in USD cents."},
					"fee_cents": {"type": "integer", "description": "Fee in USD cents. Optional, defaults to 0."},
					"executed_at": {"type": "string", "description": "RFC3339 timestamp. Optional, defaults to now."}
				},
				"required": ["symbol", "side", "quantity", "price_cents"]
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			rt := RecordTradeInput{
				Symbol:     in.Symbol,
				AssetClass: AssetClass(in.AssetClass),
				Side:       Side(in.Side),
				Quantity:   in.Quantity,
				PriceCents: in.PriceCents,
				FeeCents:   in.FeeCents,
			}
			if in.ExecutedAt != "" {
				t, err := time.Parse(time.RFC3339, in.ExecutedAt)
				if err != nil {
					return nil, fmt.Errorf("executed_at must be RFC3339: %w", err)
				}
				rt.ExecutedAt = t
			}
			txn, err := svc.RecordTrade(ctx, rt)
			if err != nil {
				return nil, err
			}
			return map[string]any{"id": txn.ID, "symbol": txn.Symbol, "side": txn.Side, "quantity": txn.Quantity, "total_cents": txn.TotalCents}, nil
		}),
	}
}

func listHoldingsTool(svc *Service) registration {
	return registration{
		def: tool.Definition{
			Name:        "list_holdings",
			Risk:        tool.RiskRead,
			Description: "List the open stock/ETF/crypto positions with cost basis (and current value/P&L if market data is configured).",
			Parameters:  json.RawMessage(`{"type": "object", "properties": {}}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			holdings, err := svc.ListHoldings(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, len(holdings))
			for i, h := range holdings {
				out[i] = map[string]any{
					"symbol": h.Symbol, "asset_class": h.AssetClass, "quantity": h.Quantity,
					"total_cost_cents": h.TotalCostCents, "avg_cost_cents": h.AvgCostCents,
					"current_value_cents": h.CurrentValueCents, "unrealized_pl_cents": h.UnrealizedPLCents,
				}
			}
			return out, nil
		}),
	}
}

func portfolioSummaryTool(svc *Service) registration {
	return registration{
		def: tool.Definition{
			Name:        "get_portfolio_summary",
			Risk:        tool.RiskRead,
			Description: "Get the total cost basis, current value, and realized/unrealized P&L across all holdings.",
			Parameters:  json.RawMessage(`{"type": "object", "properties": {}}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			s, err := svc.GetPortfolioSummary(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"total_cost_cents": s.TotalCostCents, "total_value_cents": s.TotalValueCents,
				"realized_pl_cents": s.RealizedPLCents, "market_data_configured": s.MarketDataConfigured,
				"holding_count": len(s.Holdings),
			}, nil
		}),
	}
}

func createForecastTool(svc *Service) registration {
	type input struct {
		Symbol      string `json:"symbol"`
		HorizonDays []int  `json:"horizon_days"`
	}
	return registration{
		def: tool.Definition{
			Name:        "create_forecast",
			Risk:        tool.RiskWrite,
			Description: "Generate a news-informed price-direction forecast for a ticker across one or more horizons (3, 5, 10, 14, 21, 30, 60, or 90 days). Requires a configured market data provider.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"symbol": {"type": "string", "description": "Ticker symbol, e.g. AAPL."},
					"horizon_days": {"type": "array", "items": {"type": "integer", "enum": [3, 5, 10, 14, 21, 30, 60, 90]}, "description": "One or more forecast horizons in days."}
				},
				"required": ["symbol", "horizon_days"]
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			f, err := svc.CreateForecast(ctx, in.Symbol, in.HorizonDays, "")
			if err != nil {
				return nil, err
			}
			return forecastToMap(f), nil
		}),
	}
}

func listForecastsTool(svc *Service) registration {
	type input struct {
		Symbol string `json:"symbol"`
		Limit  int    `json:"limit"`
	}
	return registration{
		def: tool.Definition{
			Name:        "list_forecasts",
			Risk:        tool.RiskRead,
			Description: "List the past forecasts, most recent first, optionally filtered by symbol.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"symbol": {"type": "string", "description": "Optional ticker filter."},
					"limit": {"type": "integer", "description": "Optional maximum number to return."}
				}
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, fmt.Errorf("invalid input: %w", err)
				}
			}
			forecasts, err := svc.ListForecasts(ctx, in.Symbol, in.Limit)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, len(forecasts))
			for i, f := range forecasts {
				out[i] = forecastToMap(f)
			}
			return out, nil
		}),
	}
}

func getForecastTool(svc *Service) registration {
	type input struct {
		ForecastID int64 `json:"forecast_id"`
	}
	return registration{
		def: tool.Definition{
			Name:        "get_forecast",
			Risk:        tool.RiskRead,
			Description: "Get one of the forecasts by ID, including all its horizons and any evaluated outcomes.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"forecast_id": {"type": "integer"}},
				"required": ["forecast_id"]
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			f, err := svc.GetForecast(ctx, in.ForecastID)
			if err != nil {
				return nil, err
			}
			return forecastToMap(f), nil
		}),
	}
}

func evaluateForecastTool(svc *Service) registration {
	type input struct {
		ForecastID int64 `json:"forecast_id"`
	}
	return registration{
		def: tool.Definition{
			Name:        "evaluate_forecast",
			Risk:        tool.RiskWrite,
			Description: "Score a forecast's horizons whose target date has passed against the actual current price, recording the outcomes. Requires a configured market data provider.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"forecast_id": {"type": "integer"}},
				"required": ["forecast_id"]
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			f, err := svc.EvaluateForecast(ctx, in.ForecastID)
			if err != nil {
				return nil, err
			}
			return forecastToMap(f), nil
		}),
	}
}

func enrichForecastTool(svc *Service) registration {
	type input struct {
		ForecastID int64 `json:"forecast_id"`
	}
	return registration{
		def: tool.Definition{
			Name:        "enrich_forecast",
			Risk:        tool.RiskWrite,
			Description: "Add deeper qualitative analysis to an existing forecast: catalysts, risks, and signals supporting or conflicting with the predicted direction(s). Does not change the forecast's numbers.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"forecast_id": {"type": "integer"}},
				"required": ["forecast_id"]
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			f, err := svc.EnrichForecast(ctx, in.ForecastID)
			if err != nil {
				return nil, err
			}
			return forecastToMap(f), nil
		}),
	}
}

func placeSimulatedOrderTool(svc *Service) registration {
	type input struct {
		Symbol   string `json:"symbol"`
		Side     string `json:"side"`
		Quantity string `json:"quantity"`
	}
	return registration{
		def: tool.Definition{
			Name:        "place_simulated_order",
			Risk:        tool.RiskWrite,
			Description: "Place a paper/simulated order through the configured paper-trading broker. This never uses real money. Requires a configured paper broker.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"symbol": {"type": "string"},
					"side": {"type": "string", "enum": ["buy", "sell"]},
					"quantity": {"type": "string", "description": "Number of shares/units as a decimal string."}
				},
				"required": ["symbol", "side", "quantity"]
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			txn, err := svc.PlaceSimulatedOrder(ctx, in.Symbol, Side(in.Side), in.Quantity)
			if err != nil {
				return nil, err
			}
			return map[string]any{"id": txn.ID, "symbol": txn.Symbol, "side": txn.Side, "quantity": txn.Quantity, "price_cents": txn.PriceCents, "source": txn.Source}, nil
		}),
	}
}

func addToWatchlistTool(svc *Service) registration {
	type input struct {
		Symbol      string `json:"symbol"`
		AssetClass  string `json:"asset_class"`
		HorizonDays []int  `json:"horizon_days"`
	}
	return registration{
		def: tool.Definition{
			Name:        "add_to_watchlist",
			Risk:        tool.RiskWrite,
			Description: "Add a ticker to the watchlist so the background scheduler auto-generates and evaluates forecasts for it. Requires the scheduler to be enabled by the operator.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"symbol": {"type": "string"},
					"asset_class": {"type": "string", "enum": ["equity", "etf", "crypto"], "description": "Defaults to equity."},
					"horizon_days": {"type": "array", "items": {"type": "integer", "enum": [3, 5, 10, 14, 21, 30, 60, 90]}}
				},
				"required": ["symbol", "horizon_days"]
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			e, err := svc.AddToWatchlist(ctx, in.Symbol, in.HorizonDays, AssetClass(in.AssetClass))
			if err != nil {
				return nil, err
			}
			return map[string]any{"symbol": e.Symbol, "horizons": e.Horizons, "enabled": e.Enabled}, nil
		}),
	}
}

func listWatchlistTool(svc *Service) registration {
	return registration{
		def: tool.Definition{
			Name:        "list_watchlist",
			Risk:        tool.RiskRead,
			Description: "List the watchlist symbols and their forecast horizons.",
			Parameters:  json.RawMessage(`{"type": "object", "properties": {}}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			entries, err := svc.ListWatchlist(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, len(entries))
			for i, e := range entries {
				out[i] = map[string]any{"symbol": e.Symbol, "horizons": e.Horizons, "enabled": e.Enabled, "last_forecast_at": e.LastForecastAt}
			}
			return out, nil
		}),
	}
}

func removeFromWatchlistTool(svc *Service) registration {
	type input struct {
		Symbol string `json:"symbol"`
	}
	return registration{
		def: tool.Definition{
			Name:        "remove_from_watchlist",
			Risk:        tool.RiskWrite,
			Description: "Remove a ticker from the watchlist.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"symbol": {"type": "string"}},
				"required": ["symbol"]
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			if err := svc.RemoveFromWatchlist(ctx, in.Symbol); err != nil {
				return nil, err
			}
			return map[string]any{"removed": true, "symbol": in.Symbol}, nil
		}),
	}
}

func listPositionReviewsTool(svc *Service) registration {
	type input struct {
		Limit int `json:"limit"`
	}
	return registration{
		def: tool.Definition{
			Name: "list_position_reviews",
			Risk: tool.RiskRead,
			Description: "List the most recent scheduled position reviews, newest first. " +
				"Each review is one symbol's verdict from a periodic review cycle: max_sell, sell, hold, buy, or max_buy, " +
				"with the reasoning behind it and whether the symbol is held or only watched.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"limit": {"type": "integer", "description": "Maximum reviews to return (default 100)."}}
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, fmt.Errorf("invalid input: %w", err)
				}
			}
			reviews, err := svc.ListReviews(ctx, in.Limit)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, len(reviews))
			for i, rev := range reviews {
				out[i] = map[string]any{
					"symbol": rev.Symbol, "rating": string(rev.Rating), "source": string(rev.Source),
					"rationale": rev.Rationale, "reviewed_at": rev.ReviewedAt.Format(time.RFC3339),
					"forecast_id": rev.ForecastID,
				}
			}
			return out, nil
		}),
	}
}

func forecastToMap(f Forecast) map[string]any {
	horizons := make([]map[string]any, len(f.Horizons))
	for i, h := range f.Horizons {
		m := map[string]any{
			"horizon_days": h.HorizonDays, "target_date": h.TargetDate.Format("2006-01-02"),
			"predicted_direction": h.PredictedDirection, "predicted_low_cents": h.PredictedLowCents,
			"predicted_high_cents": h.PredictedHighCents, "confidence": h.Confidence,
		}
		if h.EvaluatedAt != nil {
			m["actual_price_cents"] = h.ActualPriceCents
			m["actual_direction"] = h.ActualDirection
			m["within_predicted_range"] = h.WithinPredictedRange
		}
		horizons[i] = m
	}
	out := map[string]any{
		"id": f.ID, "symbol": f.Symbol, "requested_at": f.RequestedAt.Format(time.RFC3339),
		"reference_price_cents": f.ReferencePriceCents, "rationale": f.Rationale, "horizons": horizons,
	}
	if f.Enrichment != nil {
		out["enrichment"] = map[string]any{
			"summary": f.Enrichment.Summary, "price_range_challenge": f.Enrichment.PriceRangeChallenge,
			"catalysts": f.Enrichment.Catalysts, "risks": f.Enrichment.Risks,
			"supporting_signals": f.Enrichment.SupportingSignals, "conflicting_signals": f.Enrichment.ConflictingSignals,
		}
	}
	return out
}
