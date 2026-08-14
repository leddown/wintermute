package fintech

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// WatchlistEntry is a symbol the background scheduler should periodically
// forecast and evaluate for one user.
type WatchlistEntry struct {
	ID             int64
	InstrumentID   int64
	Symbol         string
	Horizons       []int
	Enabled        bool
	LastForecastAt *time.Time
	CreatedAt      time.Time
}

// AddToWatchlist adds (or re-enables) symbol on userID's watchlist with
// the given horizons. Symbols are uppercase-normalized and horizons
// validated against ValidHorizons.
func (s *Service) AddToWatchlist(ctx context.Context, symbol string, horizonDays []int, assetClass AssetClass) (WatchlistEntry, error) {
	symbol, err := normalizeSymbol(symbol)
	if err != nil {
		return WatchlistEntry{}, err
	}
	if len(horizonDays) == 0 {
		return WatchlistEntry{}, fmt.Errorf("%w: at least one horizon is required", ErrValidation)
	}
	for _, h := range horizonDays {
		if !validHorizon(h) {
			return WatchlistEntry{}, fmt.Errorf("%w: %d is not a valid horizon (allowed: %v)", ErrValidation, h, ValidHorizons)
		}
	}
	if assetClass == "" {
		assetClass = AssetClassEquity
	}
	instrument, err := s.repo.UpsertInstrument(ctx, symbol, "", assetClass)
	if err != nil {
		return WatchlistEntry{}, err
	}
	return s.repo.UpsertWatchlistEntry(ctx, instrument.ID, horizonsToCSV(horizonDays))
}

// ListWatchlist returns userID's watchlist entries.
func (s *Service) ListWatchlist(ctx context.Context) ([]WatchlistEntry, error) {
	return s.repo.ListWatchlist(ctx)
}

// RemoveFromWatchlist removes symbol from userID's watchlist.
func (s *Service) RemoveFromWatchlist(ctx context.Context, symbol string) error {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	return s.repo.DeleteWatchlistEntry(ctx, symbol)
}

func horizonsToCSV(days []int) string {
	parts := make([]string, len(days))
	for i, d := range days {
		parts[i] = strconv.Itoa(d)
	}
	return strings.Join(parts, ",")
}

func csvToHorizons(csv string) []int {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// UpsertWatchlistEntry inserts or re-enables a watchlist row.
func (r *Repository) UpsertWatchlistEntry(ctx context.Context, instrumentID int64, horizons string) (WatchlistEntry, error) {
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO fintech_watchlist (instrument_id, horizons, enabled, created_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT (instrument_id) DO UPDATE SET
			horizons = excluded.horizons, enabled = 1`,
		instrumentID, horizons, timestamp(time.Now())); err != nil {
		return WatchlistEntry{}, fmt.Errorf("fintech: upsert watchlist: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, watchlistColumns+` WHERE w.instrument_id = ?`, instrumentID)
	if err != nil {
		return WatchlistEntry{}, fmt.Errorf("fintech: read back watchlist: %w", err)
	}
	defer rows.Close()
	out, err := scanWatchlist(rows)
	if err != nil {
		return WatchlistEntry{}, err
	}
	if len(out) == 0 {
		return WatchlistEntry{}, ErrNotFound
	}
	return out[0], nil
}

// watchlistColumns is the shared select list — every read joins the symbol,
// which is what callers actually work with.
const watchlistColumns = `
	SELECT w.id, w.instrument_id, i.symbol, w.horizons, w.enabled, w.last_forecast_at, w.created_at
	FROM fintech_watchlist w
	JOIN fintech_instruments i ON i.id = w.instrument_id`

// ListWatchlist returns the watchlist, by symbol.
func (r *Repository) ListWatchlist(ctx context.Context) ([]WatchlistEntry, error) {
	rows, err := r.db.QueryContext(ctx, watchlistColumns+` ORDER BY i.symbol ASC`)
	if err != nil {
		return nil, fmt.Errorf("fintech: list watchlist: %w", err)
	}
	defer rows.Close()
	return scanWatchlist(rows)
}

// ListEnabledWatchlist returns the entries the scheduler should act on.
func (r *Repository) ListEnabledWatchlist(ctx context.Context) ([]WatchlistEntry, error) {
	rows, err := r.db.QueryContext(ctx, watchlistColumns+` WHERE w.enabled = 1 ORDER BY w.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("fintech: list enabled watchlist: %w", err)
	}
	defer rows.Close()
	return scanWatchlist(rows)
}

func scanWatchlist(rows *sql.Rows) ([]WatchlistEntry, error) {
	var out []WatchlistEntry
	for rows.Next() {
		var e WatchlistEntry
		var horizonsCSV, createdAt string
		var enabled int
		var lastForecastAt sql.NullString
		if err := rows.Scan(&e.ID, &e.InstrumentID, &e.Symbol, &horizonsCSV,
			&enabled, &lastForecastAt, &createdAt); err != nil {
			return nil, fmt.Errorf("fintech: scan watchlist: %w", err)
		}
		e.Horizons = csvToHorizons(horizonsCSV)
		e.Enabled = enabled == 1
		e.CreatedAt = parseTime(createdAt)
		if lastForecastAt.Valid && lastForecastAt.String != "" {
			at := parseTime(lastForecastAt.String)
			e.LastForecastAt = &at
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteWatchlistEntry removes a symbol from the watchlist. Removing one that
// is not there is not an error: the caller wanted it gone, and it is.
func (r *Repository) DeleteWatchlistEntry(ctx context.Context, symbol string) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM fintech_watchlist
		WHERE instrument_id IN (SELECT id FROM fintech_instruments WHERE symbol = ?)`, symbol)
	if err != nil {
		return fmt.Errorf("fintech: delete watchlist: %w", err)
	}
	return nil
}

// TouchWatchlistForecastedAt stamps when the scheduler last forecast an entry,
// which is what stops it doing so again before the interval is up.
func (r *Repository) TouchWatchlistForecastedAt(ctx context.Context, entryID int64, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE fintech_watchlist SET last_forecast_at = ? WHERE id = ?`, timestamp(at), entryID)
	if err != nil {
		return fmt.Errorf("fintech: touch watchlist: %w", err)
	}
	return nil
}

// ForecastRef names a forecast with a due horizon, and the symbol to price it
// against — which the evaluation sweep needs and would otherwise re-query.
type ForecastRef struct {
	ForecastID int64
	Symbol     string
}
