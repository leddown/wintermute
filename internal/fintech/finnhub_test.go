package fintech

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFinnhubProvider_NotConfigured(t *testing.T) {
	p := NewFinnhubProvider("")
	if p.Configured() {
		t.Fatal("empty key should not be Configured()")
	}
	if _, err := p.Quote(context.Background(), "AAPL"); err != ErrMarketDataNotConfigured {
		t.Errorf("Quote() error = %v, want ErrMarketDataNotConfigured", err)
	}
}

func TestFinnhubProvider_Quote(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/quote" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("symbol"); got != "AAPL" {
			t.Errorf("symbol = %q, want AAPL", got)
		}
		if r.URL.Query().Get("token") != "test-key" {
			t.Errorf("token not forwarded")
		}
		fmt.Fprint(w, `{"c": 150.27, "h": 151, "l": 149, "o": 150, "pc": 149.5, "t": 1700000000}`)
	}))
	defer ts.Close()

	original := finnhubBaseURL
	finnhubBaseURL = ts.URL
	t.Cleanup(func() { finnhubBaseURL = original })

	p := NewFinnhubProvider("test-key")
	q, err := p.Quote(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}
	if q.PriceCents != 15027 {
		t.Errorf("PriceCents = %d, want 15027", q.PriceCents)
	}
	if q.AsOf.Unix() != 1700000000 {
		t.Errorf("AsOf = %v, want unix 1700000000", q.AsOf)
	}
}

func TestFinnhubProvider_Quote_UnknownSymbol(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Finnhub returns all-zero fields for an unknown symbol.
		fmt.Fprint(w, `{"c": 0, "h": 0, "l": 0, "o": 0, "pc": 0, "t": 0}`)
	}))
	defer ts.Close()

	original := finnhubBaseURL
	finnhubBaseURL = ts.URL
	t.Cleanup(func() { finnhubBaseURL = original })

	p := NewFinnhubProvider("test-key")
	if _, err := p.Quote(context.Background(), "NOPE"); err == nil {
		t.Error("expected an error for an unknown symbol (zero price), got nil")
	}
}

func TestFinnhubProvider_News(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/company-news" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		fmt.Fprint(w, `[
			{"headline": "A", "source": "Reuters", "url": "http://x/a", "datetime": 1700000000},
			{"headline": "B", "source": "Bloomberg", "url": "http://x/b", "datetime": 1700000100},
			{"headline": "C", "source": "WSJ", "url": "http://x/c", "datetime": 1700000200}
		]`)
	}))
	defer ts.Close()

	original := finnhubBaseURL
	finnhubBaseURL = ts.URL
	t.Cleanup(func() { finnhubBaseURL = original })

	p := NewFinnhubProvider("test-key")
	news, err := p.News(context.Background(), "AAPL", 2)
	if err != nil {
		t.Fatalf("News() error: %v", err)
	}
	if len(news) != 2 {
		t.Fatalf("got %d news items, want 2 (limit)", len(news))
	}
	if news[0].Headline != "A" || news[0].Source != "Reuters" {
		t.Errorf("news[0] = %+v, want headline A from Reuters", news[0])
	}
}

func TestFinnhubProvider_RateLimited(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	original := finnhubBaseURL
	finnhubBaseURL = ts.URL
	t.Cleanup(func() { finnhubBaseURL = original })

	p := NewFinnhubProvider("test-key")
	_, err := p.Quote(context.Background(), "AAPL")
	if err == nil {
		t.Fatal("expected a rate-limit error, got nil")
	}
}
