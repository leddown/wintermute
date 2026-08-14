package fintech

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var alphaVantageBaseURL = "https://www.alphavantage.co/query"

// AlphaVantageProvider implements MarketDataProvider against the Alpha Vantage
// free tier (https://www.alphavantage.co). Free accounts are limited to 25
// requests/day and 5 req/min.
type AlphaVantageProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewAlphaVantageProvider builds an AlphaVantageProvider. apiKey may be empty,
// in which case Configured() reports false.
func NewAlphaVantageProvider(apiKey string) *AlphaVantageProvider {
	return &AlphaVantageProvider{apiKey: apiKey, httpClient: &http.Client{Timeout: 10 * time.Second}}
}

func (p *AlphaVantageProvider) Configured() bool { return p.apiKey != "" }

// Quote fetches the current price for symbol using the GLOBAL_QUOTE function.
func (p *AlphaVantageProvider) Quote(ctx context.Context, symbol string) (Quote, error) {
	if !p.Configured() {
		return Quote{}, ErrMarketDataNotConfigured
	}

	params := url.Values{
		"function": {"GLOBAL_QUOTE"},
		"symbol":   {symbol},
		"apikey":   {p.apiKey},
	}
	var body struct {
		GlobalQuote struct {
			Price            string `json:"05. price"`
			LatestTradingDay string `json:"07. latest trading day"`
		} `json:"Global Quote"`
	}
	if err := p.get(ctx, params, &body); err != nil {
		return Quote{}, err
	}
	if body.GlobalQuote.Price == "" {
		return Quote{}, fmt.Errorf("fintech: alpha vantage returned no price for %q (unknown symbol or rate limited)", symbol)
	}

	priceCents, err := parseUnsignedCents(body.GlobalQuote.Price)
	if err != nil {
		return Quote{}, fmt.Errorf("fintech: parsing alpha vantage price %q: %w", body.GlobalQuote.Price, err)
	}

	asOf := time.Now()
	if t, err := time.Parse("2006-01-02", body.GlobalQuote.LatestTradingDay); err == nil {
		asOf = t.UTC()
	}
	return Quote{Symbol: symbol, PriceCents: priceCents, AsOf: asOf}, nil
}

// News fetches recent news for symbol using the NEWS_SENTIMENT function,
// capped at limit items (limit <= 0 means a default of 10).
func (p *AlphaVantageProvider) News(ctx context.Context, symbol string, limit int) ([]NewsItem, error) {
	if !p.Configured() {
		return nil, ErrMarketDataNotConfigured
	}
	if limit <= 0 {
		limit = 10
	}

	params := url.Values{
		"function": {"NEWS_SENTIMENT"},
		"tickers":  {symbol},
		"limit":    {fmt.Sprintf("%d", limit)},
		"apikey":   {p.apiKey},
	}
	var body struct {
		Feed []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Source        string `json:"source"`
			TimePublished string `json:"time_published"`
		} `json:"feed"`
	}
	if err := p.get(ctx, params, &body); err != nil {
		return nil, err
	}

	out := make([]NewsItem, 0, len(body.Feed))
	for _, n := range body.Feed {
		t, _ := time.Parse("20060102T150405", n.TimePublished)
		out = append(out, NewsItem{
			Headline:    n.Title,
			Source:      n.Source,
			URL:         n.URL,
			PublishedAt: t.UTC(),
		})
	}
	return out, nil
}

func (p *AlphaVantageProvider) get(ctx context.Context, params url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, alphaVantageBaseURL+"?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("fintech: building alpha vantage request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fintech: alpha vantage request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return fmt.Errorf("fintech: reading alpha vantage response: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("fintech: alpha vantage rate limit exceeded (free tier is 25 req/day, 5 req/min)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fintech: unexpected alpha vantage HTTP status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("fintech: decoding alpha vantage response: %w", err)
	}
	return nil
}
