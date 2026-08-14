package fintech

import (
	"context"
	"log"
	"time"
)

// Scheduler periodically generates and evaluates forecasts for every
// user's watchlist. It is opt-in: app.go only starts it when a non-zero
// scan interval is configured, so it never silently spends Anthropic or
// market-data API quota.
//
// The scheduler is a legitimate system actor: it scopes every generated
// forecast to the watchlist row's own user_id (the authoritative owner),
// never an externally supplied ID — this is distinct from the tool-handler
// rule about not trusting model-supplied user IDs.
type Scheduler struct {
	svc      *Service
	interval time.Duration
}

// NewScheduler builds a Scheduler that scans every interval. The caller is
// responsible for only starting it when interval > 0.
func NewScheduler(svc *Service, interval time.Duration) *Scheduler {
	return &Scheduler{svc: svc, interval: interval}
}

// Run blocks, scanning once per interval until ctx is cancelled. It does
// not run an immediate scan on start (so a server restart loop can't
// trigger a burst of forecasts); the first scan happens after one
// interval.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	log.Printf("fintech scheduler started (interval %s)", s.interval)
	for {
		select {
		case <-ctx.Done():
			log.Printf("fintech scheduler stopped")
			return
		case <-ticker.C:
			s.scanOnce(ctx)
		}
	}
}

// scanOnce runs one full scan: evaluate matured forecasts, then generate
// fresh forecasts for due watchlist entries. Errors are logged and
// skipped rather than aborting the whole scan, so one bad symbol doesn't
// stall the rest.
func (s *Scheduler) scanOnce(ctx context.Context) {
	if !s.svc.marketData.Configured() {
		log.Printf("fintech scheduler: market data not configured, skipping scan")
		return
	}
	evaluated := s.evaluateDue(ctx)
	generated := s.generateDue(ctx)
	log.Printf("fintech scheduler: scan complete (%d forecasts evaluated, %d generated)", evaluated, generated)
}

func (s *Scheduler) evaluateDue(ctx context.Context) int {
	return s.svc.evaluateDueForecasts(ctx)
}

// ReviewScheduler periodically runs the position review cycle: refresh the
// forecast and deep-dive enrichment for every watched and every held
// symbol, rate each one, and email the digest outside night hours. Like
// Scheduler it is opt-in (app.go only starts it when a review interval is
// configured) so the app never auto-spends Anthropic or market-data quota.
type ReviewScheduler struct {
	svc      *Service
	interval time.Duration
}

// NewReviewScheduler builds a ReviewScheduler that runs every interval.
// The caller is responsible for only starting it when interval > 0.
func NewReviewScheduler(svc *Service, interval time.Duration) *ReviewScheduler {
	return &ReviewScheduler{svc: svc, interval: interval}
}

// Run blocks, running one review cycle per interval until ctx is
// cancelled. As with Scheduler, the first cycle happens after one full
// interval rather than immediately, so a restart loop can't trigger a
// burst of AI calls.
func (s *ReviewScheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	log.Printf("fintech review scheduler started (interval %s)", s.interval)
	for {
		select {
		case <-ctx.Done():
			log.Printf("fintech review scheduler stopped")
			return
		case <-ticker.C:
			reviewed, err := s.svc.RunReviewCycle(ctx)
			if err != nil {
				log.Printf("fintech review scheduler: cycle failed: %v", err)
				continue
			}
			log.Printf("fintech review scheduler: cycle complete (%d symbols reviewed)", reviewed)
		}
	}
}

func (s *Scheduler) generateDue(ctx context.Context) int {
	entries, err := s.svc.repo.ListEnabledWatchlist(ctx)
	if err != nil {
		log.Printf("fintech scheduler: listing watchlist: %v", err)
		return 0
	}
	cutoff := time.Now().Add(-s.interval)
	count := 0
	for _, e := range entries {
		if ctx.Err() != nil {
			return count
		}
		// Skip entries forecasted within the last interval, so a symbol
		// gets at most one fresh forecast per scan interval.
		if e.LastForecastAt != nil && e.LastForecastAt.After(cutoff) {
			continue
		}
		if _, err := s.svc.CreateForecast(ctx, e.Symbol, e.Horizons, ""); err != nil {
			log.Printf("fintech scheduler: forecasting %s: %v", e.Symbol, err)
			continue
		}
		if err := s.svc.repo.TouchWatchlistForecastedAt(ctx, e.ID, time.Now()); err != nil {
			log.Printf("fintech scheduler: stamping watchlist %d: %v", e.ID, err)
		}
		count++
	}
	return count
}
