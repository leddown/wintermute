package fintech

import (
	"testing"

	"wintermute/internal/store"
)

// newTestRepository opens an in-memory SQLite database with the migrations
// applied.
//
// The tests this came from needed a live PostgreSQL server and skipped
// themselves when one was not configured, which in practice meant the ledger's
// arithmetic went unverified on most runs. SQLite in memory has no such excuse:
// these run everywhere, every time, in milliseconds.
func newTestRepository(t *testing.T) *Repository {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open(:memory:) error: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRepository(st.DB())
}

// newTestService builds a Service over that database with the supplied fakes.
// Anything not passed is an unconfigured stub, which is what the production
// wiring hands it when no API key is set.
func newTestService(t *testing.T, marketData MarketDataProvider, forecaster Forecaster, alerter Alerter) (*Service, *Repository) {
	t.Helper()
	repo := newTestRepository(t)
	return serviceOver(repo, marketData, forecaster, alerter), repo
}

// serviceOver builds another Service over a repository a test already has.
// Swapping the fakes mid-test is how the forecast tests move from generating a
// forecast to enriching and then scoring it, and each step needs a different
// canned answer over the same ledger.
func serviceOver(repo *Repository, marketData MarketDataProvider, forecaster Forecaster, alerter Alerter) *Service {
	if marketData == nil {
		marketData = NewNotConfiguredProvider()
	}
	return NewService(repo, "", marketData,
		NewAlpacaPaperBroker("", ""), NewKrakenSync("", ""), forecaster, alerter)
}

// backdateHorizons moves every horizon's target date into the past, which is
// what makes an evaluation sweep have something to do without waiting days for
// it. Raw SQL on purpose: there is no production path that rewrites a target
// date, and adding one for a test would be a worse trade than this line.
func backdateHorizons(t *testing.T, repo *Repository) {
	t.Helper()
	if _, err := repo.db.Exec(
		`UPDATE fintech_forecast_horizons SET target_date = date('now', '-1 day')`); err != nil {
		t.Fatalf("backdate horizons: %v", err)
	}
}
