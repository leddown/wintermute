package fintech

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeAlerter records what the review digest would have emailed.
type fakeAlerter struct {
	deliverable bool
	subjects    []string
	bodies      []string
}

func (a *fakeAlerter) Deliverable(ctx context.Context) bool { return a.deliverable }

func (a *fakeAlerter) Send(ctx context.Context, subject, body string) error {
	a.subjects = append(a.subjects, subject)
	a.bodies = append(a.bodies, body)
	return nil
}

func TestQuietAt(t *testing.T) {
	// 2026-07-29 is a Wednesday; only the clock time matters here.
	at := func(hour, min int) time.Time {
		return time.Date(2026, 7, 29, hour, min, 0, 0, time.UTC)
	}

	tests := []struct {
		name string
		cfg  ReviewConfig
		when time.Time
		want bool
	}{
		{
			name: "disabled is never quiet",
			cfg:  ReviewConfig{QuietEnabled: false, QuietStart: "22:00", QuietEnd: "07:00", Timezone: "UTC"},
			when: at(23, 0),
			want: false,
		},
		{
			name: "wrapping window, late evening",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "22:00", QuietEnd: "07:00", Timezone: "UTC"},
			when: at(23, 30),
			want: true,
		},
		{
			name: "wrapping window, early morning",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "22:00", QuietEnd: "07:00", Timezone: "UTC"},
			when: at(3, 0),
			want: true,
		},
		{
			name: "wrapping window, midday is not quiet",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "22:00", QuietEnd: "07:00", Timezone: "UTC"},
			when: at(12, 0),
			want: false,
		},
		{
			name: "wrapping window, end is exclusive",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "22:00", QuietEnd: "07:00", Timezone: "UTC"},
			when: at(7, 0),
			want: false,
		},
		{
			name: "wrapping window, start is inclusive",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "22:00", QuietEnd: "07:00", Timezone: "UTC"},
			when: at(22, 0),
			want: true,
		},
		{
			name: "same-day window, inside",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "01:00", QuietEnd: "05:00", Timezone: "UTC"},
			when: at(3, 0),
			want: true,
		},
		{
			name: "same-day window, outside",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "01:00", QuietEnd: "05:00", Timezone: "UTC"},
			when: at(6, 0),
			want: false,
		},
		{
			// 23:30 UTC is 01:30 in Berlin (CEST, UTC+2) in July, which
			// falls inside the window — the timezone must be applied.
			name: "timezone shifts the window",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "01:00", QuietEnd: "05:00", Timezone: "Europe/Berlin"},
			when: at(23, 30),
			want: true,
		},
		{
			name: "start equal to end is treated as no window",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "22:00", QuietEnd: "22:00", Timezone: "UTC"},
			when: at(22, 0),
			want: false,
		},
		{
			name: "unparseable time fails open",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "not-a-time", QuietEnd: "07:00", Timezone: "UTC"},
			when: at(23, 0),
			want: false,
		},
		{
			name: "unknown timezone fails open",
			cfg:  ReviewConfig{QuietEnabled: true, QuietStart: "22:00", QuietEnd: "07:00", Timezone: "Mars/Olympus"},
			when: at(23, 0),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.quietAt(tt.when); got != tt.want {
				t.Errorf("quietAt(%s) = %v, want %v", tt.when.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

func TestNormalizeRating(t *testing.T) {
	tests := map[string]Rating{
		"max_sell":   RatingMaxSell,
		"sell":       RatingSell,
		"hold":       RatingHold,
		"buy":        RatingBuy,
		"max_buy":    RatingMaxBuy,
		"  MAX_BUY ": RatingMaxBuy,
		"strong buy": RatingHold, // unrecognized falls back to hold
		"":           RatingHold,
	}
	for raw, want := range tests {
		if got := normalizeRating(raw); got != want {
			t.Errorf("normalizeRating(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSetReviewConfigValidation(t *testing.T) {
	// Validation runs before any repository call, so a bare Service is
	// enough to exercise the rejection paths.
	svc := &Service{}
	ctx := context.Background()

	tests := []struct {
		name string
		cfg  ReviewConfig
	}{
		{"bad start time", ReviewConfig{QuietStart: "25:00", QuietEnd: "07:00"}},
		{"bad end time", ReviewConfig{QuietStart: "22:00", QuietEnd: "7pm"}},
		{"empty start time", ReviewConfig{QuietStart: "", QuietEnd: "07:00"}},
		{"unknown timezone", ReviewConfig{QuietStart: "22:00", QuietEnd: "07:00", Timezone: "Mars/Olympus"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := svc.SetReviewConfig(ctx, tt.cfg); err == nil {
				t.Fatalf("SetReviewConfig(%+v) = nil, want a validation error", tt.cfg)
			}
		})
	}
}

func TestComposeReviewDigest(t *testing.T) {
	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	reviews := []Review{
		{Symbol: "HOLDME", Rating: RatingHold, Source: ReviewSourceWatchlist, ReviewedAt: now},
		{Symbol: "BUYME", Rating: RatingBuy, Source: ReviewSourceHolding, ReviewedAt: now},
		{Symbol: "DUMPME", Rating: RatingMaxSell, Source: ReviewSourceHolding, Rationale: "guidance cut", ReviewedAt: now},
		{Symbol: "LOADUP", Rating: RatingMaxBuy, Source: ReviewSourceWatchlist, ReviewedAt: now},
	}

	subject, body := composeReviewDigest(reviews, now)

	if !strings.Contains(subject, "1 MAX SELL") || !strings.Contains(subject, "1 MAX BUY") {
		t.Errorf("subject %q should summarize the verdict counts", subject)
	}

	// Most actionable first: MAX SELL, MAX BUY, then SELL/BUY, then HOLD.
	wantOrder := []string{"DUMPME", "LOADUP", "BUYME", "HOLDME"}
	at := make([]int, len(wantOrder))
	for i, sym := range wantOrder {
		at[i] = strings.Index(body, sym)
		if at[i] < 0 {
			t.Fatalf("body is missing %s:\n%s", sym, body)
		}
	}
	for i := 1; i < len(at); i++ {
		if at[i-1] > at[i] {
			t.Errorf("digest order wrong: %s should precede %s\n%s", wantOrder[i-1], wantOrder[i], body)
		}
	}

	if !strings.Contains(body, "guidance cut") {
		t.Errorf("body should include the rating rationale:\n%s", body)
	}
	if !strings.Contains(body, "held") || !strings.Contains(body, "watched") {
		t.Errorf("body should distinguish held from watched positions:\n%s", body)
	}
}

// reviewTestService builds a Service with the review fakes wired in.
func reviewTestService(t *testing.T, alerter Alerter, forecaster Forecaster) *Service {
	svc, _ := newTestService(t, fakeMarketData{priceCents: 10000}, forecaster, alerter)
	return svc
}

// The review cycle works from what is actually held, which means a position
// that was bought and then fully sold must not appear — reviewing a holding
// nobody holds would spend a model call to advise on nothing.
func TestListHeldPositions(t *testing.T) {
	svc := reviewTestService(t, nil, nil)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	trades := []RecordTradeInput{
		{Symbol: "AAPL", Side: SideBuy, Quantity: "10", PriceCents: 10000, ExecutedAt: base},
		// Bought and fully sold: closed, and so not a position.
		{Symbol: "MSFT", Side: SideBuy, Quantity: "4", PriceCents: 20000, ExecutedAt: base},
		{Symbol: "MSFT", Side: SideSell, Quantity: "4", PriceCents: 21000, ExecutedAt: base.Add(time.Hour)},
		// Partly sold: still held, at the moving-average cost of what is left.
		{Symbol: "TSLA", Side: SideBuy, Quantity: "6", PriceCents: 30000, ExecutedAt: base},
		{Symbol: "TSLA", Side: SideSell, Quantity: "2", PriceCents: 32000, ExecutedAt: base.Add(2 * time.Hour)},
	}
	for i, in := range trades {
		if _, err := svc.RecordTrade(ctx, in); err != nil {
			t.Fatalf("RecordTrade(%d) error: %v", i, err)
		}
	}

	positions, err := svc.ListHeldPositions(ctx)
	if err != nil {
		t.Fatalf("ListHeldPositions() error: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("got %d open positions, want 2 (the closed MSFT position is not one): %+v", len(positions), positions)
	}

	bySymbol := make(map[string]HeldPosition, len(positions))
	for _, p := range positions {
		bySymbol[p.Symbol] = p
	}
	if _, ok := bySymbol["MSFT"]; ok {
		t.Error("MSFT was bought and fully sold; it must not be listed as held")
	}
	if got := bySymbol["AAPL"].Quantity; !strings.HasPrefix(got, "10") {
		t.Errorf("AAPL quantity = %q, want 10", got)
	}
	// Four of six shares left, bought at 300.00 each: 1200.00 of cost stays.
	if got := bySymbol["TSLA"].TotalCostCents; got != 120000 {
		t.Errorf("TSLA remaining cost = %d cents, want 120000", got)
	}
}

func TestReviewCycleHoldsDigestDuringNightHours(t *testing.T) {
	// One canned response satisfies both structured calls: the forecast
	// call reads "horizons", the review call reads the rest.
	output := json.RawMessage(`{
		"horizons":[{"horizon_days":5,"direction":"up","predicted_low_cents":9500,"predicted_high_cents":11000,"confidence":0.6}],
		"overall_rationale":"momentum",
		"summary":"looks strong",
		"price_range_challenge":"range is too narrow",
		"rating":"max_buy",
		"rating_rationale":"conviction call"
	}`)
	alerter := &fakeAlerter{deliverable: true}
	svc := reviewTestService(t, alerter, fakeForecaster{output: output})
	ctx := context.Background()

	if _, err := svc.AddToWatchlist(ctx, "NVDA", []int{5}, AssetClassEquity); err != nil {
		t.Fatalf("AddToWatchlist() error: %v", err)
	}

	// A quiet window that straddles right now, so the first cycle's
	// digest must be held back.
	now := time.Now()
	if err := svc.SetReviewConfig(ctx, ReviewConfig{
		AlertEnabled: true,
		QuietEnabled: true,
		QuietStart:   now.Add(-time.Hour).Format("15:04"),
		QuietEnd:     now.Add(time.Hour).Format("15:04"),
	}); err != nil {
		t.Fatalf("SetReviewConfig() error: %v", err)
	}

	if _, err := svc.RunReviewCycle(ctx); err != nil {
		t.Fatalf("RunReviewCycle() (quiet) error: %v", err)
	}

	reviews, err := svc.ListReviews(ctx, 0)
	if err != nil {
		t.Fatalf("ListReviews() error: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("after quiet cycle: got %d reviews, want 1", len(reviews))
	}
	if reviews[0].Rating != RatingMaxBuy {
		t.Errorf("rating = %q, want %q", reviews[0].Rating, RatingMaxBuy)
	}
	if reviews[0].Source != ReviewSourceWatchlist {
		t.Errorf("source = %q, want %q", reviews[0].Source, ReviewSourceWatchlist)
	}
	if len(alerter.bodies) != 0 {
		t.Fatalf("digest was emailed during night hours: %v", alerter.bodies)
	}
	if reviews[0].ReportedAt != nil {
		t.Errorf("review was marked reported despite no email being sent")
	}

	// The enrichment from the review call must land on the forecast.
	forecasts, err := svc.ListForecasts(ctx, "NVDA", 0)
	if err != nil {
		t.Fatalf("ListForecasts() error: %v", err)
	}
	if len(forecasts) != 1 || forecasts[0].Enrichment == nil {
		t.Fatalf("review did not refresh the forecast enrichment: %+v", forecasts)
	}
	if forecasts[0].Enrichment.PriceRangeChallenge != "range is too narrow" {
		t.Errorf("enrichment not persisted from the review call: %+v", forecasts[0].Enrichment)
	}

	// Leaving night hours: the next cycle's digest must cover BOTH runs.
	if err := svc.SetReviewConfig(ctx, ReviewConfig{
		AlertEnabled: true,
		QuietEnabled: false,
		QuietStart:   "22:00",
		QuietEnd:     "07:00",
	}); err != nil {
		t.Fatalf("SetReviewConfig() error: %v", err)
	}
	if _, err := svc.RunReviewCycle(ctx); err != nil {
		t.Fatalf("RunReviewCycle() (awake) error: %v", err)
	}

	if len(alerter.bodies) != 1 {
		t.Fatalf("got %d digests sent, want exactly 1", len(alerter.bodies))
	}
	if !strings.Contains(alerter.bodies[0], "NVDA") {
		t.Errorf("digest does not mention the reviewed symbol:\n%s", alerter.bodies[0])
	}
	if !strings.Contains(alerter.bodies[0], "2 symbol(s) reviewed") {
		t.Errorf("digest should carry over the suppressed night run:\n%s", alerter.bodies[0])
	}

	reviews, err = svc.ListReviews(ctx, 0)
	if err != nil {
		t.Fatalf("ListReviews() error: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("got %d reviews, want 2", len(reviews))
	}
	for _, rev := range reviews {
		if rev.ReportedAt == nil {
			t.Errorf("review %d was not stamped reported after a successful send", rev.ID)
		}
	}
}
