// Package fintech tracks stock/ETF/crypto holdings from a single transaction
// ledger — the only source of truth, with holdings and cost basis always
// derived on read rather than stored as a mutable running balance — and has
// the assistant produce price-direction forecasts that are later scored
// against what actually happened.
//
// It never places a real-money trade. It records trades made elsewhere (typed
// in, imported from CSV, or read from a Kraken account read-only) and places
// simulated orders through a paper-broker seam.
//
// Moved here from morpheus, which had signed-in users and scoped every row to
// one. Wintermute authenticates clients by token and holds one portfolio, so
// the owner column and the userID parameter that went with it are gone rather
// than stubbed — see internal/store/migrations/0008_fintech.sql.
package fintech

import (
	"errors"
	"time"
)

// ErrNotFound is returned when a requested record does not exist or does
// not belong to the requesting user.
var ErrNotFound = errors.New("fintech: not found")

// ErrDuplicate is returned by InsertTransaction when a transaction with
// the same dedupe hash already exists for the user — this is how CSV
// re-imports and Kraken re-syncs stay idempotent.
var ErrDuplicate = errors.New("fintech: duplicate transaction")

// ErrValidation is returned for invalid input to a Service method.
var ErrValidation = errors.New("fintech: validation error")

// AssetClass categorizes an Instrument.
type AssetClass string

const (
	AssetClassEquity AssetClass = "equity"
	AssetClassETF    AssetClass = "etf"
	AssetClassCrypto AssetClass = "crypto"
)

// Instrument is a tradable symbol (e.g. "AAPL", "VTI", "BTC").
type Instrument struct {
	ID         int64
	Symbol     string
	Name       string
	AssetClass AssetClass
	CreatedAt  time.Time
}

// Side is which direction a Transaction moves a position.
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// Source records how a Transaction entered the ledger.
type Source string

const (
	SourceManual     Source = "manual"
	SourceCSVImport  Source = "csv_import"
	SourcePaper      Source = "paper"
	SourceKrakenSync Source = "kraken_sync"
)

// Transaction is one entry in the ledger — the only source of truth for
// holdings, cost basis, and realized/unrealized P&L, all of which are
// derived on read rather than stored separately.
type Transaction struct {
	ID            int64
	InstrumentID  int64
	Symbol        string     // populated on read via a join; not its own column
	AssetClass    AssetClass // likewise joined on read, so replaying the ledger needs no extra query
	Side          Side
	Quantity      string // decimal string — never float64, summed with math/big
	PriceCents    int64
	FeeCents      int64
	TotalCents    int64
	Source        Source
	ExecutedAt    time.Time
	BrokerOrderID string
	ExternalID    string
	DedupeHash    string
	Notes         string
	CreatedAt     time.Time
}

// Holding is a derived, point-in-time view of one open position. It is
// never stored — ListHoldings recomputes it from the transaction ledger
// on every call.
type Holding struct {
	InstrumentID      int64
	Symbol            string
	AssetClass        AssetClass
	Quantity          string // decimal string, summed exactly in Go (see repository.go)
	TotalCostCents    int64
	AvgCostCents      int64 // TotalCostCents / quantity, in cents per unit
	CurrentValueCents int64 // 0 if no market data provider is configured
	UnrealizedPLCents int64 // 0 if no market data provider is configured
}

// PortfolioSummary aggregates every open holding.
type PortfolioSummary struct {
	Holdings             []Holding
	TotalCostCents       int64
	TotalValueCents      int64 // equals TotalCostCents if no market data is available
	RealizedPLCents      int64
	MarketDataConfigured bool
}

// AIUsage is what the forecasting and review calls have cost, all time and
// today. Kept because a local backend's tokens are free and a cloud backend's
// are not, and that difference is the reason to run one rather than the other.
type AIUsage struct {
	Calls             int64
	InputTokens       int64
	OutputTokens      int64
	CallsToday        int64
	InputTokensToday  int64
	OutputTokensToday int64
}
