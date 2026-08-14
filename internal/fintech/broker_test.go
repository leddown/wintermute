package fintech

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAlpacaPaperBroker_NotConfigured(t *testing.T) {
	b := NewAlpacaPaperBroker("", "")
	if b.Configured() {
		t.Fatal("empty credentials should not be Configured()")
	}
	if _, err := b.PlaceOrder(context.Background(), PlaceOrderInput{Symbol: "AAPL", Side: SideBuy, Quantity: "1"}); err != ErrBrokerNotConfigured {
		t.Errorf("PlaceOrder() error = %v, want ErrBrokerNotConfigured", err)
	}
}

func TestAlpacaPaperBroker_PlaceOrder_PollsToFill(t *testing.T) {
	var getCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("APCA-API-KEY-ID") != "key" || r.Header.Get("APCA-API-SECRET-KEY") != "secret" {
			t.Errorf("auth headers not forwarded")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/orders":
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"type":"market"`) {
				t.Errorf("order body missing market type: %s", raw)
			}
			fmt.Fprint(w, `{"id": "ord_1", "status": "new", "filled_qty": "0", "filled_avg_price": ""}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orders/ord_1":
			getCount++
			if getCount < 2 {
				fmt.Fprint(w, `{"id": "ord_1", "status": "pending_new", "filled_qty": "0", "filled_avg_price": ""}`)
				return
			}
			fmt.Fprint(w, `{"id": "ord_1", "status": "filled", "filled_qty": "2", "filled_avg_price": "150.27", "filled_at": "2026-06-23T20:00:00Z"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	original := alpacaPaperBaseURL
	alpacaPaperBaseURL = ts.URL
	t.Cleanup(func() { alpacaPaperBaseURL = original })

	b := &alpacaPaperBroker{apiKey: "key", apiSecret: "secret", httpClient: ts.Client(), pollInterval: time.Millisecond, pollTimeout: 5 * time.Second}
	res, err := b.PlaceOrder(context.Background(), PlaceOrderInput{Symbol: "AAPL", Side: SideBuy, Quantity: "2"})
	if err != nil {
		t.Fatalf("PlaceOrder() error: %v", err)
	}
	if res.BrokerOrderID != "ord_1" {
		t.Errorf("BrokerOrderID = %q, want ord_1", res.BrokerOrderID)
	}
	if res.FilledQuantity != "2" {
		t.Errorf("FilledQuantity = %q, want 2", res.FilledQuantity)
	}
	if res.FillPriceCents != 15027 {
		t.Errorf("FillPriceCents = %d, want 15027", res.FillPriceCents)
	}
	if res.FilledAt.UTC().Format(time.RFC3339) != "2026-06-23T20:00:00Z" {
		t.Errorf("FilledAt = %v, want 2026-06-23T20:00:00Z", res.FilledAt)
	}
}

func TestAlpacaPaperBroker_PlaceOrder_Rejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id": "ord_2", "status": "rejected", "filled_qty": "0", "filled_avg_price": ""}`)
	}))
	defer ts.Close()

	original := alpacaPaperBaseURL
	alpacaPaperBaseURL = ts.URL
	t.Cleanup(func() { alpacaPaperBaseURL = original })

	b := &alpacaPaperBroker{apiKey: "key", apiSecret: "secret", httpClient: ts.Client(), pollInterval: time.Millisecond, pollTimeout: time.Second}
	_, err := b.PlaceOrder(context.Background(), PlaceOrderInput{Symbol: "AAPL", Side: SideBuy, Quantity: "1"})
	if err == nil || !strings.Contains(err.Error(), "did not fill") {
		t.Errorf("error = %v, want one mentioning 'did not fill'", err)
	}
}
