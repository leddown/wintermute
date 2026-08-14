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

// finnhubBaseURL is hardcoded (test-overridable only) so it can never be
// redirected via configuration, mirroring the kraken adapter's stance.
var finnhubBaseURL = "https://finnhub.io/api/v1"

// FinnhubProvider implements MarketDataProvider against Finnhub's free
// tier (https://finnhub.io). It is gated on an API key: when unset,
// Configured() reports false and the not-configured stub should be used
// instead (see app.go wiring).
type FinnhubProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewFinnhubProvider builds a FinnhubProvider. apiKey may be empty, in
// which case Configured() reports false.
func NewFinnhubProvider(apiKey string) *FinnhubProvider {
	return &FinnhubProvider{apiKey: apiKey, httpClient: &http.Client{Timeout: 10 * time.Second}}
}

func (p *FinnhubProvider) Configured() bool { return p.apiKey != "" }

// Quote fetches the current price for symbol. Finnhub returns prices as
// JSON numbers in dollars; we read the raw number as a string and convert
// to integer cents via the same decimal-safe path used for trade prices,
// never going through float64.
func (p *FinnhubProvider) Quote(ctx context.Context, symbol string) (Quote, error) {
	if !p.Configured() {
		return Quote{}, ErrMarketDataNotConfigured
	}

	var body struct {
		Current   json.Number `json:"c"`
		Timestamp int64       `json:"t"`
	}
	if err := p.get(ctx, "/quote", url.Values{"symbol": {symbol}}, &body); err != nil {
		return Quote{}, err
	}
	if body.Current == "" || body.Current == "0" {
		return Quote{}, fmt.Errorf("fintech: finnhub returned no price for %q (unknown symbol or rate limited)", symbol)
	}

	priceCents, err := parseUnsignedCents(body.Current.String())
	if err != nil {
		return Quote{}, fmt.Errorf("fintech: parsing finnhub price %q: %w", body.Current.String(), err)
	}
	asOf := time.Now()
	if body.Timestamp > 0 {
		asOf = time.Unix(body.Timestamp, 0).UTC()
	}
	return Quote{Symbol: symbol, PriceCents: priceCents, AsOf: asOf}, nil
}

// News fetches recent company news for symbol over the trailing week,
// capped at limit items (limit <= 0 means a default of 10).
func (p *FinnhubProvider) News(ctx context.Context, symbol string, limit int) ([]NewsItem, error) {
	if !p.Configured() {
		return nil, ErrMarketDataNotConfigured
	}
	if limit <= 0 {
		limit = 10
	}

	now := time.Now().UTC()
	params := url.Values{
		"symbol": {symbol},
		"from":   {now.AddDate(0, 0, -7).Format("2006-01-02")},
		"to":     {now.Format("2006-01-02")},
	}
	var raw []struct {
		Headline string `json:"headline"`
		Source   string `json:"source"`
		URL      string `json:"url"`
		Datetime int64  `json:"datetime"`
	}
	if err := p.get(ctx, "/company-news", params, &raw); err != nil {
		return nil, err
	}

	out := make([]NewsItem, 0, limit)
	for _, n := range raw {
		if len(out) >= limit {
			break
		}
		out = append(out, NewsItem{
			Headline:    n.Headline,
			Source:      n.Source,
			URL:         n.URL,
			PublishedAt: time.Unix(n.Datetime, 0).UTC(),
		})
	}
	return out, nil
}

// get performs a signed GET against a Finnhub endpoint and decodes the
// JSON response into out. The API token is sent as a query parameter per
// Finnhub's documented scheme.
func (p *FinnhubProvider) get(ctx context.Context, path string, params url.Values, out any) error {
	params.Set("token", p.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finnhubBaseURL+path+"?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("fintech: building finnhub request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fintech: finnhub request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return fmt.Errorf("fintech: reading finnhub response: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("fintech: finnhub rate limit exceeded (free tier is 60 req/min)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fintech: unexpected finnhub HTTP status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("fintech: decoding finnhub response: %w", err)
	}
	return nil
}
