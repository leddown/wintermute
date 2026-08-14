package fintech

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// symbolPattern bounds ticker symbols to a safe charset (uppercase
// letters, digits, dot, hyphen — covering tickers like BRK.B). Validating
// server-side keeps junk/markup out of the ledger and out of anything that
// later renders a symbol.
var symbolPattern = regexp.MustCompile(`^[A-Z0-9.\-]{1,20}$`)

// normalizeSymbol upper-cases and trims raw, then rejects it unless it
// matches symbolPattern.
func normalizeSymbol(raw string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if !symbolPattern.MatchString(s) {
		return "", fmt.Errorf("%w: symbol must be 1-20 characters of letters, digits, '.' or '-'", ErrValidation)
	}
	return s, nil
}

// Service implements ledger recording and derives holdings/cost-basis/P&L
// from that ledger on every read — there is no separately stored,
// mutable balance to drift out of sync.
type Service struct {
	repo        *Repository
	marketData  *switchableMarketData // swappable so the provider can be reconfigured in-app
	paperBroker PaperBroker
	kraken      *KrakenSync // nil or unconfigured until KRAKEN_API_KEY/KRAKEN_API_SECRET are set
	forecaster  Forecaster  // nil until a model backend is wired in
	alerter     Alerter     // nil when nothing delivers the review digest; reviews are still stored
	importer    *importerState
}

// NewService builds a Service backed by repo.
//
// marketData, paperBroker and kraken may be unconfigured stubs — every method
// that needs one checks Configured() rather than assuming it is there, which is
// what lets the portfolio work with no API keys at all. forecaster may be nil,
// and CreateForecast then says so instead of inventing a price. alerter may be
// nil, in which case reviews are generated and stored but nothing mails them.
//
// The provider name is passed in rather than inferred: unlike morpheus, which
// only ever built Finnhub from the environment, this takes whichever provider
// the configuration named.
func NewService(repo *Repository, providerName string, marketData MarketDataProvider, paperBroker PaperBroker, kraken *KrakenSync, forecaster Forecaster, alerter Alerter) *Service {
	if !marketData.Configured() {
		providerName = ""
	}
	return &Service{
		repo:        repo,
		marketData:  newSwitchableMarketData(providerName, marketData),
		paperBroker: paperBroker,
		kraken:      kraken,
		forecaster:  forecaster,
		alerter:     alerter,
		importer:    newImporterState(),
	}
}

// MarketDataProviderName returns the active provider's key, or "" when none is
// configured.
func (s *Service) MarketDataProviderName() string {
	return s.marketData.Name()
}

// The three below say whether an outside connection is present, so a page can
// state that a feature is unavailable rather than offering a control that can
// only fail.

// KrakenConfigured reports whether the read-only Kraken sync can run.
func (s *Service) KrakenConfigured() bool {
	return s.kraken != nil && s.kraken.Configured()
}

// BrokerConfigured reports whether simulated orders can be placed.
func (s *Service) BrokerConfigured() bool {
	return s.paperBroker != nil && s.paperBroker.Configured()
}

// ForecastingConfigured reports whether a model backend is wired in.
func (s *Service) ForecastingConfigured() bool {
	return s.forecaster != nil
}

// RecordTradeInput is the input to RecordTrade.
type RecordTradeInput struct {
	Symbol        string
	AssetClass    AssetClass // used only when the instrument doesn't exist yet; defaults to AssetClassEquity
	Side          Side
	Quantity      string // decimal string, e.g. "1.5"
	PriceCents    int64  // per-unit price in USD cents
	FeeCents      int64
	ExecutedAt    time.Time // zero value defaults to time.Now()
	Source        Source    // defaults to SourceManual
	Notes         string
	BrokerOrderID string
	ExternalID    string
}

// RecordTrade validates in and appends one row to the ledger. It is the
// single insert path used by manual entry, CSV import, paper fills, and
// Kraken sync alike.
func (s *Service) RecordTrade(ctx context.Context, in RecordTradeInput) (Transaction, error) {
	symbol, err := normalizeSymbol(in.Symbol)
	if err != nil {
		return Transaction{}, err
	}
	if in.Side != SideBuy && in.Side != SideSell {
		return Transaction{}, fmt.Errorf("%w: side must be %q or %q", ErrValidation, SideBuy, SideSell)
	}
	qty, ok := new(big.Rat).SetString(strings.TrimSpace(in.Quantity))
	if !ok || qty.Sign() <= 0 {
		return Transaction{}, fmt.Errorf("%w: quantity must be a positive decimal number", ErrValidation)
	}
	if in.PriceCents < 0 || in.FeeCents < 0 {
		return Transaction{}, fmt.Errorf("%w: price_cents and fee_cents must not be negative", ErrValidation)
	}

	assetClass := in.AssetClass
	if assetClass == "" {
		assetClass = AssetClassEquity
	}
	instrument, err := s.repo.UpsertInstrument(ctx, symbol, "", assetClass)
	if err != nil {
		return Transaction{}, err
	}

	executedAt := in.ExecutedAt
	if executedAt.IsZero() {
		executedAt = time.Now()
	}
	source := in.Source
	if source == "" {
		source = SourceManual
	}

	gross := ratToCents(new(big.Rat).Mul(qty, big.NewRat(in.PriceCents, 1)))
	var totalCents int64
	switch in.Side {
	case SideBuy:
		totalCents = -(gross + in.FeeCents)
	case SideSell:
		totalCents = gross - in.FeeCents
	}

	txn := Transaction{
		InstrumentID:  instrument.ID,
		Side:          in.Side,
		Quantity:      qty.FloatString(10),
		PriceCents:    in.PriceCents,
		FeeCents:      in.FeeCents,
		TotalCents:    totalCents,
		Source:        source,
		ExecutedAt:    executedAt,
		BrokerOrderID: in.BrokerOrderID,
		ExternalID:    in.ExternalID,
		Notes:         in.Notes,
		DedupeHash:    computeDedupeHash(symbol, in.Side, qty.FloatString(10), in.PriceCents, in.FeeCents, executedAt, source, in.ExternalID),
	}
	out, err := s.repo.InsertTransaction(ctx, txn)
	if err != nil {
		return Transaction{}, err
	}
	out.Symbol = symbol // InsertTransaction's RETURNING doesn't join fintech_instruments
	return out, nil
}

// PlaceSimulatedOrder places a paper/simulated order through the
// configured broker (never real money) and records the resulting fill in
// the ledger as source=paper. It errors clearly if no paper broker is
// configured rather than silently doing nothing.
func (s *Service) PlaceSimulatedOrder(ctx context.Context, symbol string, side Side, quantity string) (Transaction, error) {
	if !s.paperBroker.Configured() {
		return Transaction{}, ErrBrokerNotConfigured
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	result, err := s.paperBroker.PlaceOrder(ctx, PlaceOrderInput{Symbol: symbol, Side: side, Quantity: quantity})
	if err != nil {
		return Transaction{}, err
	}
	return s.RecordTrade(ctx, RecordTradeInput{
		Symbol:        symbol,
		Side:          side,
		Quantity:      result.FilledQuantity,
		PriceCents:    result.FillPriceCents,
		ExecutedAt:    result.FilledAt,
		Source:        SourcePaper,
		BrokerOrderID: result.BrokerOrderID,
	})
}

// ListTransactions returns the full ledger, most recent first.
func (s *Service) ListTransactions(ctx context.Context) ([]Transaction, error) {
	txns, err := s.repo.ListTransactions(ctx)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(txns)-1; i < j; i, j = i+1, j-1 {
		txns[i], txns[j] = txns[j], txns[i]
	}
	return txns, nil
}

// ListHoldings returns the currently open positions, derived by
// replaying the full transaction ledger with the moving-average cost
// method (no separate holdings table — see package doc).
func (s *Service) ListHoldings(ctx context.Context) ([]Holding, error) {
	holdings, _, err := s.computeHoldings(ctx)
	return holdings, err
}

// GetPortfolioSummary aggregates ListHoldings into totals plus realized
// P&L from the ledger replay. CurrentValueCents/UnrealizedPLCents fall
// back to cost-basis-only values (zero unrealized P&L) until a
// MarketDataProvider is configured — see marketdata.go.
func (s *Service) GetPortfolioSummary(ctx context.Context) (PortfolioSummary, error) {
	holdings, realizedPLCents, err := s.computeHoldings(ctx)
	if err != nil {
		return PortfolioSummary{}, err
	}

	configured := s.marketData.Configured()
	if configured {
		for i := range holdings {
			quote, err := s.marketData.Quote(ctx, holdings[i].Symbol)
			if err != nil {
				continue // leave this holding's value at cost-basis fallback rather than failing the whole summary
			}
			qty, ok := new(big.Rat).SetString(holdings[i].Quantity)
			if !ok {
				continue
			}
			currentValue := ratToCents(new(big.Rat).Mul(qty, big.NewRat(quote.PriceCents, 1)))
			holdings[i].CurrentValueCents = currentValue
			holdings[i].UnrealizedPLCents = currentValue - holdings[i].TotalCostCents
		}
	}

	summary := PortfolioSummary{Holdings: holdings, RealizedPLCents: realizedPLCents, MarketDataConfigured: configured}
	for _, h := range holdings {
		summary.TotalCostCents += h.TotalCostCents
		summary.TotalValueCents += h.CurrentValueCents
	}
	return summary, nil
}

// computeHoldings replays the ledger in execution order using the
// moving-average cost method: each buy adds to a running quantity/cost
// for its instrument, each sell removes cost proportional to the average
// cost per unit at that point in the replay and books the difference as
// realized P&L. All intermediate arithmetic uses big.Rat — quantities and
// cents are never represented as float64 until the final rounding to a
// storable int64-cents value.
func (s *Service) computeHoldings(ctx context.Context) ([]Holding, int64, error) {
	txns, err := s.repo.ListTransactions(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.holdingsFromLedger(txns)
}

// ListHeldPositions derives every open position, for the review cycle.
//
// Morpheus replayed each user's ledger separately here and returned the owner
// with every position. There is one ledger now, so this is holdingsFromLedger
// over all of it — kept as its own method because the review cycle asks a
// different question ("what is held, at all") than the portfolio page does.
func (s *Service) ListHeldPositions(ctx context.Context) ([]HeldPosition, error) {
	txns, err := s.repo.ListTransactions(ctx)
	if err != nil {
		return nil, err
	}
	holdings, _, err := s.holdingsFromLedger(txns)
	if err != nil {
		return nil, err
	}
	out := make([]HeldPosition, 0, len(holdings))
	for _, h := range holdings {
		out = append(out, HeldPosition{Holding: h})
	}
	return out, nil
}

// holdingsFromLedger replays transactions (in execution order) into open
// positions plus realized P&L. It is shared by the per-user and cross-user
// paths so there is exactly one implementation of the moving-average cost
// method, and it makes no database calls of its own — every field it needs,
// including the instrument's asset class, is joined onto the ledger rows.
func (s *Service) holdingsFromLedger(txns []Transaction) ([]Holding, int64, error) {
	type position struct {
		instrumentID int64
		symbol       string
		assetClass   AssetClass
		qty          *big.Rat
		costCents    *big.Rat
	}
	order := make([]int64, 0)
	positions := make(map[int64]*position)
	realizedPLCents := int64(0)

	for _, t := range txns {
		p, ok := positions[t.InstrumentID]
		if !ok {
			p = &position{
				instrumentID: t.InstrumentID, symbol: t.Symbol, assetClass: t.AssetClass,
				qty: new(big.Rat), costCents: new(big.Rat),
			}
			positions[t.InstrumentID] = p
			order = append(order, t.InstrumentID)
		}

		qty, ok := new(big.Rat).SetString(t.Quantity)
		if !ok {
			return nil, 0, fmt.Errorf("fintech: transaction %d has unparseable quantity %q", t.ID, t.Quantity)
		}

		switch t.Side {
		case SideBuy:
			p.qty.Add(p.qty, qty)
			p.costCents.Add(p.costCents, big.NewRat(-t.TotalCents, 1)) // buy total_cents is negative cash outflow
		case SideSell:
			if p.qty.Sign() > 0 {
				avgCostPerUnit := new(big.Rat).Quo(p.costCents, p.qty)
				costRemoved := new(big.Rat).Mul(avgCostPerUnit, qty)
				p.costCents.Sub(p.costCents, costRemoved)
				realizedPLCents += t.TotalCents - ratToCents(costRemoved)
			} else {
				// Selling without a recorded prior position (e.g. ledger
				// seeded mid-history): treat the entire proceeds as
				// realized gain rather than computing a basis we don't have.
				realizedPLCents += t.TotalCents
			}
			p.qty.Sub(p.qty, qty)
		}
	}

	holdings := make([]Holding, 0, len(order))
	for _, id := range order {
		p := positions[id]
		if p.qty.Sign() <= 0 {
			continue // fully closed position
		}
		totalCostCents := ratToCents(p.costCents)
		qtyFloat, _ := p.qty.Float64()
		avgCostCents := int64(0)
		if qtyFloat != 0 {
			avgCostCents = int64(math.Round(float64(totalCostCents) / qtyFloat))
		}
		holdings = append(holdings, Holding{
			InstrumentID:      p.instrumentID,
			Symbol:            p.symbol,
			AssetClass:        p.assetClass,
			Quantity:          p.qty.FloatString(10),
			TotalCostCents:    totalCostCents,
			AvgCostCents:      avgCostCents,
			CurrentValueCents: totalCostCents, // no market data configured yet — see marketdata.go
			UnrealizedPLCents: 0,
		})
	}
	return holdings, realizedPLCents, nil
}

// ratToCents rounds r (an exact rational number of cents) to the nearest
// integer cent. Converting to float64 only at this final boundary is safe:
// float64's 53-bit mantissa carries far more precision than any realistic
// dollar amount needs for round-to-nearest-cent.
func ratToCents(r *big.Rat) int64 {
	f, _ := r.Float64()
	return int64(math.Round(f))
}

// computeDedupeHash derives a stable hash so CSV re-imports and Kraken
// re-syncs of the same trade are idempotent (enforced by the unique index on
// dedupe_hash, not by a pre-check that two concurrent imports would both pass).
func computeDedupeHash(symbol string, side Side, quantity string, priceCents, feeCents int64, executedAt time.Time, source Source, externalID string) string {
	h := sha256.New()
	h.Write([]byte(symbol))
	h.Write([]byte(side))
	h.Write([]byte(quantity))
	h.Write([]byte(strconv.FormatInt(priceCents, 10)))
	h.Write([]byte(strconv.FormatInt(feeCents, 10)))
	h.Write([]byte(executedAt.UTC().Format(time.RFC3339)))
	h.Write([]byte(source))
	h.Write([]byte(externalID))
	return hex.EncodeToString(h.Sum(nil))
}
