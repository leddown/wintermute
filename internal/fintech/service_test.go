package fintech

import (
	"context"
	"testing"
	"time"
)

func testService(t *testing.T) *Service {
	svc, _ := newTestService(t, nil, nil, nil)
	return svc
}

func TestServiceHoldingsAndRealizedPL(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Buy 10 AAPL @ $100.00 + $1 fee.
	if _, err := svc.RecordTrade(ctx, RecordTradeInput{
		Symbol: "AAPL", Side: SideBuy, Quantity: "10", PriceCents: 10000, FeeCents: 100, ExecutedAt: base,
	}); err != nil {
		t.Fatalf("RecordTrade(buy 1) error: %v", err)
	}

	// Buy 10 more AAPL @ $120.00 + $1 fee -> moving average cost basis.
	if _, err := svc.RecordTrade(ctx, RecordTradeInput{
		Symbol: "AAPL", Side: SideBuy, Quantity: "10", PriceCents: 12000, FeeCents: 100, ExecutedAt: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("RecordTrade(buy 2) error: %v", err)
	}

	// Total cost basis: (10*10000+100) + (10*12000+100) = 100100 + 120100 = 220200 cents for 20 shares.
	// Avg cost per share = 11010 cents.

	// Sell 5 AAPL @ $150.00 - $1 fee.
	if _, err := svc.RecordTrade(ctx, RecordTradeInput{
		Symbol: "AAPL", Side: SideSell, Quantity: "5", PriceCents: 15000, FeeCents: 100, ExecutedAt: base.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("RecordTrade(sell) error: %v", err)
	}
	// Proceeds: 5*15000-100 = 74900. Cost removed: 5 * 11010 = 55050. Realized P&L = 19850.

	holdings, err := svc.ListHoldings(ctx)
	if err != nil {
		t.Fatalf("ListHoldings() error: %v", err)
	}
	if len(holdings) != 1 {
		t.Fatalf("ListHoldings() returned %d holdings, want 1", len(holdings))
	}
	h := holdings[0]
	if h.Symbol != "AAPL" {
		t.Errorf("Symbol = %q, want AAPL", h.Symbol)
	}
	if h.Quantity != "15.0000000000" {
		t.Errorf("Quantity = %q, want 15.0000000000", h.Quantity)
	}
	wantCostCents := int64(220200 - 55050)
	if h.TotalCostCents != wantCostCents {
		t.Errorf("TotalCostCents = %d, want %d", h.TotalCostCents, wantCostCents)
	}

	summary, err := svc.GetPortfolioSummary(ctx)
	if err != nil {
		t.Fatalf("GetPortfolioSummary() error: %v", err)
	}
	if summary.RealizedPLCents != 19850 {
		t.Errorf("RealizedPLCents = %d, want 19850", summary.RealizedPLCents)
	}
	if summary.TotalCostCents != wantCostCents {
		t.Errorf("TotalCostCents = %d, want %d", summary.TotalCostCents, wantCostCents)
	}

	t.Run("duplicate trade is rejected", func(t *testing.T) {
		_, err := svc.RecordTrade(ctx, RecordTradeInput{
			Symbol: "AAPL", Side: SideBuy, Quantity: "10", PriceCents: 10000, FeeCents: 100, ExecutedAt: base,
		})
		if err != ErrDuplicate {
			t.Errorf("RecordTrade(dup) error = %v, want ErrDuplicate", err)
		}
	})

	t.Run("fully closed position is excluded from holdings", func(t *testing.T) {
		if _, err := svc.RecordTrade(ctx, RecordTradeInput{
			Symbol: "AAPL", Side: SideSell, Quantity: "15", PriceCents: 16000, FeeCents: 100, ExecutedAt: base.Add(3 * time.Hour),
		}); err != nil {
			t.Fatalf("RecordTrade(close out) error: %v", err)
		}
		holdings, err := svc.ListHoldings(ctx)
		if err != nil {
			t.Fatalf("ListHoldings() error: %v", err)
		}
		if len(holdings) != 0 {
			t.Errorf("ListHoldings() returned %d holdings, want 0 after fully closing the position", len(holdings))
		}
	})
}
