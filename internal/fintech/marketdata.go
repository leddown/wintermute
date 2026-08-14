package fintech

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Canonical provider keys persisted in fintech_market_data_config.provider
// and sent by the settings UI. Only ProviderFinnhub is wired up so far.
const (
	ProviderFinnhub      = "finnhub"
	ProviderAlphaVantage = "alphavantage"
	ProviderAlpaca       = "alpaca"
	ProviderTwelveData   = "twelvedata"
)

// NewMarketDataProvider constructs the provider named by the configuration.
// It validates both the name and that a key was actually supplied, so the
// caller can report a misconfiguration at startup rather than at the first
// quote.
func NewMarketDataProvider(provider, apiKey string) (MarketDataProvider, error) {
	if apiKey == "" {
		return nil, errors.New("fintech: an API key is required")
	}
	switch provider {
	case ProviderFinnhub:
		return NewFinnhubProvider(apiKey), nil
	case ProviderAlphaVantage:
		return NewAlphaVantageProvider(apiKey), nil
	default:
		return nil, fmt.Errorf("fintech: market data provider %q is not yet supported", provider)
	}
}

// ErrMarketDataNotConfigured is returned by every MarketDataProvider
// method until a real provider is wired up. Forecasting and live
// valuation deliberately refuse to proceed without one rather than
// letting the assistant guess a price from stale training data.
var ErrMarketDataNotConfigured = errors.New("fintech: no market data provider configured")

// Quote is a point-in-time price snapshot for one instrument.
type Quote struct {
	Symbol     string
	PriceCents int64
	AsOf       time.Time
}

// NewsItem is one headline relevant to an instrument.
type NewsItem struct {
	Headline    string
	Source      string
	URL         string
	PublishedAt time.Time
}

// MarketDataProvider is the seam for quotes and news. Implementations
// must report Configured() so callers (and the UI) can show a clear
// "not configured" state instead of failing opaquely.
//
// Candidate providers to wire up later (none implemented yet — this is
// the reminder the plan called for):
//   - Finnhub (recommended default): https://finnhub.io — 60 req/min free
//     tier, real-time-ish quotes (20-min delayed on free) plus a company
//     news endpoint.
//   - Alpha Vantage: https://www.alphavantage.co — only 25 req/day free
//     (5/min), but 20+ years of historical EOD data; better for backfill
//     than on-demand lookups.
//   - Alpaca Market Data: https://alpaca.markets — free with a paper
//     trading account (see broker.go), real-time US equity data, no news.
//   - Twelve Data: https://twelvedata.com — ~800 req/day free tier, broad
//     asset coverage including crypto.
type MarketDataProvider interface {
	Configured() bool
	Quote(ctx context.Context, symbol string) (Quote, error)
	News(ctx context.Context, symbol string, limit int) ([]NewsItem, error)
}

// notConfiguredProvider is the default MarketDataProvider until a real
// one is wired up via config. Every method returns ErrMarketDataNotConfigured.
type notConfiguredProvider struct{}

// NewNotConfiguredProvider returns a MarketDataProvider stub that reports
// itself as unconfigured and errors clearly on every call.
func NewNotConfiguredProvider() MarketDataProvider { return notConfiguredProvider{} }

func (notConfiguredProvider) Configured() bool { return false }

func (notConfiguredProvider) Quote(ctx context.Context, symbol string) (Quote, error) {
	return Quote{}, ErrMarketDataNotConfigured
}

func (notConfiguredProvider) News(ctx context.Context, symbol string, limit int) ([]NewsItem, error) {
	return nil, ErrMarketDataNotConfigured
}

// MarketDataProviderCandidate describes one of the not-yet-wired-up data
// providers listed in MarketDataProvider's doc comment, for surfacing in
// the UI's "not configured" reminder banner.
type MarketDataProviderCandidate struct {
	// Key is the canonical provider id the settings UI sends back and that
	// is persisted in fintech_market_data_config.provider.
	Key       string `json:"key"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	FreeTier  string `json:"free_tier"`
	Recommend bool   `json:"recommended"`
	// Implemented reports whether a provider implementation exists yet. The
	// UI offers only implemented providers as selectable; the rest are
	// shown for context but cannot be configured.
	Implemented bool `json:"implemented"`
}

// MarketDataProviderCandidates is the single source of truth for the
// candidate-provider reminder shown by GET /fintech/providers/status —
// keep this in sync with the doc comment above instead of duplicating
// the list as a hardcoded string in the frontend.
var MarketDataProviderCandidates = []MarketDataProviderCandidate{
	{Key: ProviderFinnhub, Name: "Finnhub", URL: "https://finnhub.io", FreeTier: "60 req/min, quotes + company news", Recommend: true, Implemented: true},
	{Key: ProviderAlphaVantage, Name: "Alpha Vantage", URL: "https://www.alphavantage.co", FreeTier: "25 req/day, 20+ years of historical EOD data", Implemented: true},
	{Key: ProviderAlpaca, Name: "Alpaca Market Data", URL: "https://alpaca.markets", FreeTier: "free with a paper trading account, real-time US equities"},
	{Key: ProviderTwelveData, Name: "Twelve Data", URL: "https://twelvedata.com", FreeTier: "~800 req/day, broad asset coverage including crypto"},
}

// switchableMarketData is a MarketDataProvider whose underlying
// implementation can be swapped at runtime (under an RWMutex) when the user
// saves a new configuration through the settings UI. The fintech service
// holds one of these as its provider, so every call site keeps working
// against a stable reference while the active provider changes underneath.
type switchableMarketData struct {
	mu       sync.RWMutex
	name     string
	provider MarketDataProvider
}

func newSwitchableMarketData(name string, p MarketDataProvider) *switchableMarketData {
	return &switchableMarketData{name: name, provider: p}
}

// set atomically replaces the active provider and its name.
func (s *switchableMarketData) set(name string, p MarketDataProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
	s.provider = p
}

// Name returns the active provider's canonical key (empty when none is
// configured).
func (s *switchableMarketData) Name() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name
}

func (s *switchableMarketData) Configured() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.provider.Configured()
}

func (s *switchableMarketData) Quote(ctx context.Context, symbol string) (Quote, error) {
	s.mu.RLock()
	p := s.provider
	s.mu.RUnlock()
	return p.Quote(ctx, symbol)
}

func (s *switchableMarketData) News(ctx context.Context, symbol string, limit int) ([]NewsItem, error) {
	s.mu.RLock()
	p := s.provider
	s.mu.RUnlock()
	return p.News(ctx, symbol, limit)
}
