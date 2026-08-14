package fintech

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestCreateForecast_RequiresMarketData(t *testing.T) {
	output := json.RawMessage(`{"horizons":[{"horizon_days":3,"direction":"up","predicted_low_cents":1,"predicted_high_cents":2,"confidence":0.5}]}`)
	svc, _ := testForecastService(t, NewNotConfiguredProvider(), fakeForecaster{output: output})
	_, err := svc.CreateForecast(context.Background(), "AAPL", []int{3}, "")
	if err != ErrMarketDataNotConfigured {
		t.Errorf("CreateForecast without market data: error = %v, want ErrMarketDataNotConfigured", err)
	}
}

// fakeForecaster returns a canned emit_forecast tool output without
// hitting the Anthropic API.
type fakeForecaster struct {
	output json.RawMessage
	usage  Usage
	err    error
}

func (f fakeForecaster) CreateStructuredMessage(ctx context.Context, req StructuredRequest) (json.RawMessage, Usage, error) {
	return f.output, f.usage, f.err
}

// fakeMarketData returns a fixed quote and no news.
type fakeMarketData struct {
	priceCents int64
}

func (m fakeMarketData) Configured() bool { return true }
func (m fakeMarketData) Quote(ctx context.Context, symbol string) (Quote, error) {
	return Quote{Symbol: symbol, PriceCents: m.priceCents, AsOf: time.Now()}, nil
}
func (m fakeMarketData) News(ctx context.Context, symbol string, limit int) ([]NewsItem, error) {
	return nil, nil
}

func testForecastService(t *testing.T, marketData MarketDataProvider, forecaster Forecaster) (*Service, *Repository) {
	return newTestService(t, marketData, forecaster, nil)
}

func TestCreateAndEvaluateForecast(t *testing.T) {
	output := json.RawMessage(`{
		"horizons": [
			{"horizon_days": 3, "direction": "up", "predicted_low_cents": 10000, "predicted_high_cents": 11000, "confidence": 0.6, "rationale": "x"},
			{"horizon_days": 10, "direction": "down", "predicted_low_cents": 9000, "predicted_high_cents": 9500, "confidence": 0.4, "rationale": "y"}
		],
		"overall_rationale": "test reasoning"
	}`)
	svc, repo := testForecastService(t, fakeMarketData{priceCents: 10000}, fakeForecaster{output: output, usage: Usage{InputTokens: 100, OutputTokens: 50}})
	ctx := context.Background()

	forecast, err := svc.CreateForecast(ctx, "aapl", []int{3, 10}, "")
	if err != nil {
		t.Fatalf("CreateForecast() error: %v", err)
	}
	if forecast.Symbol != "AAPL" {
		t.Errorf("Symbol = %q, want AAPL", forecast.Symbol)
	}
	if forecast.ReferencePriceCents != 10000 {
		t.Errorf("ReferencePriceCents = %d, want 10000", forecast.ReferencePriceCents)
	}
	if len(forecast.Horizons) != 2 {
		t.Fatalf("got %d horizons, want 2", len(forecast.Horizons))
	}
	if forecast.Horizons[0].HorizonDays != 3 || forecast.Horizons[0].PredictedDirection != "up" {
		t.Errorf("horizon[0] = %+v, want 3-day up", forecast.Horizons[0])
	}

	t.Run("get and list", func(t *testing.T) {
		got, err := svc.GetForecast(ctx, forecast.ID)
		if err != nil {
			t.Fatalf("GetForecast() error: %v", err)
		}
		if len(got.Horizons) != 2 {
			t.Errorf("GetForecast horizons = %d, want 2", len(got.Horizons))
		}
		list, err := svc.ListForecasts(ctx, "AAPL", 0)
		if err != nil {
			t.Fatalf("ListForecasts() error: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("ListForecasts = %d, want 1", len(list))
		}
	})

	t.Run("enrich forecast", func(t *testing.T) {
		enrichOutput := json.RawMessage(`{
			"summary": "deeper analysis",
			"catalysts": ["earnings call"],
			"risks": ["macro slowdown"],
			"supporting_signals": ["upgraded guidance"],
			"conflicting_signals": ["insider selling"]
		}`)
		enrichSvc := serviceOver(repo, fakeMarketData{priceCents: 10000},
			fakeForecaster{output: enrichOutput, usage: Usage{InputTokens: 200, OutputTokens: 80}}, nil)

		enriched, err := enrichSvc.EnrichForecast(ctx, forecast.ID)
		if err != nil {
			t.Fatalf("EnrichForecast() error: %v", err)
		}
		if enriched.Enrichment == nil {
			t.Fatalf("Enrichment = nil, want non-nil")
		}
		if enriched.Enrichment.Summary != "deeper analysis" {
			t.Errorf("Enrichment.Summary = %q, want %q", enriched.Enrichment.Summary, "deeper analysis")
		}
		if len(enriched.Enrichment.Catalysts) != 1 || enriched.Enrichment.Catalysts[0] != "earnings call" {
			t.Errorf("Enrichment.Catalysts = %v, want [earnings call]", enriched.Enrichment.Catalysts)
		}
		if enriched.EnrichedAt == nil {
			t.Errorf("EnrichedAt = nil, want non-nil")
		}

		got, err := enrichSvc.GetForecast(ctx, forecast.ID)
		if err != nil {
			t.Fatalf("GetForecast() after enrich error: %v", err)
		}
		if got.Enrichment == nil || got.Enrichment.Summary != "deeper analysis" {
			t.Errorf("GetForecast().Enrichment = %+v, want it to round-trip", got.Enrichment)
		}

		t.Run("global AI usage summary aggregates both calls", func(t *testing.T) {
			usage, err := enrichSvc.GetAIUsageSummary(ctx)
			if err != nil {
				t.Fatalf("GetAIUsageSummary() error: %v", err)
			}
			if usage.Calls != 2 {
				t.Errorf("Calls = %d, want 2 (one forecast, one enrichment)", usage.Calls)
			}
			if usage.InputTokens != 300 || usage.OutputTokens != 130 {
				t.Errorf("InputTokens/OutputTokens = %d/%d, want 300/130", usage.InputTokens, usage.OutputTokens)
			}
			if usage.CallsToday != 2 || usage.InputTokensToday != 300 || usage.OutputTokensToday != 130 {
				t.Errorf("today usage = %+v, want RequestCount:2 InputTokens:300 OutputTokens:130", usage)
			}
		})
	})

	t.Run("evaluate matured horizons", func(t *testing.T) {
		// Force both horizons' target dates into the past so evaluation runs.
		backdateHorizons(t, repo)

		// Actual price 10500 > reference 10000 => actual direction "up".
		// 3-day horizon predicted [10000,11000] (within), 10-day predicted [9000,9500] (outside).
		evalSvc := serviceOver(repo, fakeMarketData{priceCents: 10500}, nil, nil)
		evaluated, err := evalSvc.EvaluateForecast(ctx, forecast.ID)
		if err != nil {
			t.Fatalf("EvaluateForecast() error: %v", err)
		}
		for _, h := range evaluated.Horizons {
			if h.ActualPriceCents == nil || *h.ActualPriceCents != 10500 {
				t.Errorf("horizon %d actual price = %v, want 10500", h.HorizonDays, h.ActualPriceCents)
			}
			if h.ActualDirection == nil || *h.ActualDirection != "up" {
				t.Errorf("horizon %d actual direction = %v, want up", h.HorizonDays, h.ActualDirection)
			}
			wantWithin := h.HorizonDays == 3
			if h.WithinPredictedRange == nil || *h.WithinPredictedRange != wantWithin {
				t.Errorf("horizon %d within_range = %v, want %v", h.HorizonDays, h.WithinPredictedRange, wantWithin)
			}
		}
	})
}
