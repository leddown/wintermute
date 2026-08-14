package fintech

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrBrokerNotConfigured is returned by PlaceOrder when no paper-trading
// broker credentials have been configured.
var ErrBrokerNotConfigured = errors.New("fintech: no paper broker configured")

// alpacaPaperBaseURL is the paper-trading host, hardcoded (test-overridable
// only) so it can never be pointed at Alpaca's LIVE trading host via
// configuration. This is the hard "paper money only" scope boundary for
// this module, enforced in code rather than left to operator discipline.
var alpacaPaperBaseURL = "https://paper-api.alpaca.markets"

// PlaceOrderInput is the input to PaperBroker.PlaceOrder.
type PlaceOrderInput struct {
	Symbol   string
	Side     Side
	Quantity string
}

// OrderResult is a simulated fill.
type OrderResult struct {
	BrokerOrderID  string
	FilledQuantity string
	FillPriceCents int64
	FilledAt       time.Time
}

// PaperBroker is the seam for simulated order execution. It is never
// allowed to place a real order with real money — that is a hard scope
// boundary for this module, not just a current limitation. Every
// implementation must only ever talk to a sandbox/paper-trading API.
type PaperBroker interface {
	Configured() bool
	PlaceOrder(ctx context.Context, in PlaceOrderInput) (OrderResult, error)
}

// alpacaPaperBroker places market orders against Alpaca's free
// paper-trading sandbox (https://alpaca.markets). It only ever talks to
// the paper host (see alpacaPaperBaseURL); there is no code path to
// Alpaca's live trading API.
type alpacaPaperBroker struct {
	apiKey       string
	apiSecret    string
	httpClient   *http.Client
	pollInterval time.Duration
	pollTimeout  time.Duration
}

// NewAlpacaPaperBroker builds a PaperBroker backed by Alpaca's paper
// sandbox. apiKey/apiSecret may be empty, in which case Configured()
// reports false and PlaceOrder errors clearly rather than panicking.
func NewAlpacaPaperBroker(apiKey, apiSecret string) PaperBroker {
	return &alpacaPaperBroker{
		apiKey:       apiKey,
		apiSecret:    apiSecret,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		pollInterval: 500 * time.Millisecond,
		pollTimeout:  15 * time.Second,
	}
}

func (b *alpacaPaperBroker) Configured() bool {
	return b.apiKey != "" && b.apiSecret != ""
}

type alpacaOrder struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	FilledQty      string `json:"filled_qty"`
	FilledAvgPrice string `json:"filled_avg_price"`
	FilledAt       string `json:"filled_at"`
}

// PlaceOrder submits a market order to Alpaca's paper sandbox and polls
// until it fills (or the poll timeout elapses), returning the resulting
// fill. Market orders against the sandbox normally fill within seconds
// during market hours.
func (b *alpacaPaperBroker) PlaceOrder(ctx context.Context, in PlaceOrderInput) (OrderResult, error) {
	if !b.Configured() {
		return OrderResult{}, ErrBrokerNotConfigured
	}

	reqBody, err := json.Marshal(map[string]string{
		"symbol":        in.Symbol,
		"qty":           in.Quantity,
		"side":          string(in.Side),
		"type":          "market",
		"time_in_force": "day",
	})
	if err != nil {
		return OrderResult{}, fmt.Errorf("fintech: marshal alpaca order: %w", err)
	}

	var order alpacaOrder
	if err := b.do(ctx, http.MethodPost, "/v2/orders", reqBody, &order); err != nil {
		return OrderResult{}, err
	}

	order, err = b.awaitFill(ctx, order)
	if err != nil {
		return OrderResult{}, err
	}

	priceCents, err := parseUnsignedCents(order.FilledAvgPrice)
	if err != nil {
		return OrderResult{}, fmt.Errorf("fintech: parsing alpaca fill price %q: %w", order.FilledAvgPrice, err)
	}
	filledAt := time.Now()
	if order.FilledAt != "" {
		if t, perr := time.Parse(time.RFC3339, order.FilledAt); perr == nil {
			filledAt = t
		}
	}
	return OrderResult{
		BrokerOrderID:  order.ID,
		FilledQuantity: order.FilledQty,
		FillPriceCents: priceCents,
		FilledAt:       filledAt,
	}, nil
}

// awaitFill polls the order until it reaches a terminal state. A filled
// order is returned; a cancelled/rejected/expired order is an error.
func (b *alpacaPaperBroker) awaitFill(ctx context.Context, order alpacaOrder) (alpacaOrder, error) {
	deadline := time.Now().Add(b.pollTimeout)
	for {
		if order.Status == "filled" {
			return order, nil
		}
		switch order.Status {
		case "canceled", "cancelled", "rejected", "expired", "done_for_day":
			return alpacaOrder{}, fmt.Errorf("fintech: alpaca order %s did not fill (status %q)", order.ID, order.Status)
		}
		if time.Now().After(deadline) {
			return alpacaOrder{}, fmt.Errorf("fintech: timed out waiting for alpaca order %s to fill (last status %q)", order.ID, order.Status)
		}

		select {
		case <-ctx.Done():
			return alpacaOrder{}, ctx.Err()
		case <-time.After(b.pollInterval):
		}

		var refreshed alpacaOrder
		if err := b.do(ctx, http.MethodGet, "/v2/orders/"+order.ID, nil, &refreshed); err != nil {
			return alpacaOrder{}, err
		}
		order = refreshed
	}
}

// do performs an authenticated request against the Alpaca paper host and
// decodes the JSON response into out (if non-nil).
func (b *alpacaPaperBroker) do(ctx context.Context, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, alpacaPaperBaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("fintech: building alpaca request: %w", err)
	}
	req.Header.Set("APCA-API-KEY-ID", b.apiKey)
	req.Header.Set("APCA-API-SECRET-KEY", b.apiSecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fintech: alpaca request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return fmt.Errorf("fintech: reading alpaca response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fintech: alpaca returned HTTP %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("fintech: decoding alpaca response: %w", err)
		}
	}
	return nil
}
