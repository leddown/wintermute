package fintech

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// schedulerTestService builds a Service with the supplied fakes.
func schedulerTestService(t *testing.T, marketData MarketDataProvider, forecaster Forecaster) (*Service, *Repository) {
	return newTestService(t, marketData, forecaster, nil)
}

func TestSchedulerScanGeneratesAndEvaluates(t *testing.T) {
	output := json.RawMessage(`{"horizons":[{"horizon_days":3,"direction":"up","predicted_low_cents":9000,"predicted_high_cents":11000,"confidence":0.5}]}`)
	svc, repo := schedulerTestService(t, fakeMarketData{priceCents: 10000}, fakeForecaster{output: output})
	ctx := context.Background()

	// Watchlist a symbol; scheduler should generate a forecast for it.
	if _, err := svc.AddToWatchlist(ctx, "AAPL", []int{3}, AssetClassEquity); err != nil {
		t.Fatalf("AddToWatchlist() error: %v", err)
	}

	sched := NewScheduler(svc, time.Hour)
	sched.scanOnce(ctx)

	forecasts, err := svc.ListForecasts(ctx, "AAPL", 0)
	if err != nil {
		t.Fatalf("ListForecasts() error: %v", err)
	}
	if len(forecasts) != 1 {
		t.Fatalf("after scan: got %d forecasts, want 1", len(forecasts))
	}

	// A second immediate scan should NOT generate another forecast
	// (last_forecast_at is within the interval).
	sched.scanOnce(ctx)
	forecasts, _ = svc.ListForecasts(ctx, "AAPL", 0)
	if len(forecasts) != 1 {
		t.Errorf("after second scan: got %d forecasts, want 1 (should be throttled)", len(forecasts))
	}

	// Backdate the horizon so it is due, then scan: scheduler should evaluate it.
	backdateHorizons(t, repo)
	sched.scanOnce(ctx)

	got, err := svc.GetForecast(ctx, forecasts[0].ID)
	if err != nil {
		t.Fatalf("GetForecast() error: %v", err)
	}
	if got.Horizons[0].EvaluatedAt == nil {
		t.Error("expected the matured horizon to be evaluated by the scan")
	}
}

func TestSchedulerSkipsWhenMarketDataNotConfigured(t *testing.T) {
	svc, _ := schedulerTestService(t, NewNotConfiguredProvider(), fakeForecaster{})
	ctx := context.Background()
	if _, err := svc.AddToWatchlist(ctx, "AAPL", []int{3}, AssetClassEquity); err != nil {
		t.Fatalf("AddToWatchlist() error: %v", err)
	}

	NewScheduler(svc, time.Hour).scanOnce(ctx)

	forecasts, _ := svc.ListForecasts(ctx, "AAPL", 0)
	if len(forecasts) != 0 {
		t.Errorf("got %d forecasts, want 0 (scan should skip with no market data)", len(forecasts))
	}
}

func TestWatchlistRoundTrip(t *testing.T) {
	svc, _ := schedulerTestService(t, fakeMarketData{priceCents: 10000}, fakeForecaster{})
	ctx := context.Background()

	if _, err := svc.AddToWatchlist(ctx, "msft", []int{3, 10}, AssetClassEquity); err != nil {
		t.Fatalf("AddToWatchlist() error: %v", err)
	}
	entries, err := svc.ListWatchlist(ctx)
	if err != nil {
		t.Fatalf("ListWatchlist() error: %v", err)
	}
	if len(entries) != 1 || entries[0].Symbol != "MSFT" {
		t.Fatalf("entries = %+v, want one MSFT", entries)
	}
	if len(entries[0].Horizons) != 2 || entries[0].Horizons[0] != 3 || entries[0].Horizons[1] != 10 {
		t.Errorf("horizons = %v, want [3 10]", entries[0].Horizons)
	}

	if err := svc.RemoveFromWatchlist(ctx, "MSFT"); err != nil {
		t.Fatalf("RemoveFromWatchlist() error: %v", err)
	}
	entries, _ = svc.ListWatchlist(ctx)
	if len(entries) != 0 {
		t.Errorf("after remove: got %d entries, want 0", len(entries))
	}
}
