package fintech

// Forecast persistence. The shapes are the Postgres original's; what changed is
// how nullable columns and timestamps are handled, since SQLite has neither a
// timestamp type nor a driver that scans into *time.Time.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// dateLayout is how a horizon's target date is stored: a plain day, with no
// time and no zone. A forecast is "ten days out", not "ten days out at
// 14:07:32Z", and storing the extra precision would only invite a comparison
// that is off by a few hours at midnight.
const dateLayout = "2006-01-02"

// GetForecastPrompt returns the saved global addition to every forecast
// prompt, empty when nothing has been saved.
func (r *Repository) GetForecastPrompt(ctx context.Context) (string, error) {
	var prompt string
	err := r.db.QueryRowContext(ctx,
		`SELECT prompt FROM fintech_forecast_prompt WHERE id = 1`).Scan(&prompt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("fintech: query forecast prompt: %w", err)
	}
	return prompt, nil
}

// SetForecastPrompt saves that addition.
func (r *Repository) SetForecastPrompt(ctx context.Context, prompt string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO fintech_forecast_prompt (id, prompt, updated_at)
		VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET prompt = excluded.prompt, updated_at = excluded.updated_at`,
		prompt, timestamp(time.Now()))
	if err != nil {
		return fmt.Errorf("fintech: save forecast prompt: %w", err)
	}
	return nil
}

// InsertForecast writes a forecast and its horizons in one transaction: a
// forecast with only some of its horizons is not a partial answer, it is a
// wrong one, and the model would have to be paid for again to fix it.
func (r *Repository) InsertForecast(ctx context.Context, f Forecast) (Forecast, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Forecast{}, fmt.Errorf("fintech: begin forecast tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	if f.RequestedAt.IsZero() {
		f.RequestedAt = now
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO fintech_forecasts
			(instrument_id, requested_at, reference_price_cents, model_name, rationale, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		f.InstrumentID, timestamp(f.RequestedAt), f.ReferencePriceCents, f.ModelName, f.Rationale, timestamp(now))
	if err != nil {
		return Forecast{}, fmt.Errorf("fintech: insert forecast: %w", err)
	}
	if f.ID, err = res.LastInsertId(); err != nil {
		return Forecast{}, fmt.Errorf("fintech: forecast id: %w", err)
	}

	for i := range f.Horizons {
		h := &f.Horizons[i]
		h.ForecastID = f.ID
		res, err := tx.ExecContext(ctx, `
			INSERT INTO fintech_forecast_horizons
				(forecast_id, horizon_days, target_date, predicted_direction,
				 predicted_low_cents, predicted_high_cents, confidence)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			h.ForecastID, h.HorizonDays, h.TargetDate.UTC().Format(dateLayout), h.PredictedDirection,
			h.PredictedLowCents, h.PredictedHighCents, h.Confidence)
		if err != nil {
			return Forecast{}, fmt.Errorf("fintech: insert forecast horizon: %w", err)
		}
		if h.ID, err = res.LastInsertId(); err != nil {
			return Forecast{}, fmt.Errorf("fintech: horizon id: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Forecast{}, fmt.Errorf("fintech: commit forecast: %w", err)
	}
	return f, nil
}

// forecastColumns is the select list every forecast read shares, so a column
// added to one is added to all of them.
const forecastColumns = `f.id, f.instrument_id, i.symbol, f.requested_at, f.reference_price_cents,
	f.model_name, f.rationale, f.enrichment, f.enriched_at`

// GetForecast returns one forecast with its horizons.
func (r *Repository) GetForecast(ctx context.Context, forecastID int64) (Forecast, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+forecastColumns+`
		FROM fintech_forecasts f
		JOIN fintech_instruments i ON i.id = f.instrument_id
		WHERE f.id = ?`, forecastID)

	f, err := scanForecast(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Forecast{}, ErrNotFound
	}
	if err != nil {
		return Forecast{}, err
	}
	if f.Horizons, err = r.listHorizons(ctx, f.ID); err != nil {
		return Forecast{}, err
	}
	return f, nil
}

// ListForecasts returns forecasts with their horizons, most recent first.
// symbol filters to one instrument when set; limit <= 0 means everything.
func (r *Repository) ListForecasts(ctx context.Context, symbol string, limit int) ([]Forecast, error) {
	query := `
		SELECT ` + forecastColumns + `
		FROM fintech_forecasts f
		JOIN fintech_instruments i ON i.id = f.instrument_id`
	var args []any
	if symbol != "" {
		query += ` WHERE i.symbol = ?`
		args = append(args, symbol)
	}
	query += ` ORDER BY f.requested_at DESC, f.id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fintech: list forecasts: %w", err)
	}
	defer rows.Close()

	var out []Forecast
	for rows.Next() {
		f, err := scanForecast(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fintech: read forecast rows: %w", err)
	}

	// Horizons after the rows are read, not during: SQLite allows one
	// statement per connection to be mid-iteration, and querying inside the
	// loop deadlocks against the pool it is already holding.
	for i := range out {
		if out[i].Horizons, err = r.listHorizons(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// rowScanner is what *sql.Row and *sql.Rows have in common.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanForecast(row rowScanner) (Forecast, error) {
	var f Forecast
	var requestedAt, enrichment, enrichedAt string
	if err := row.Scan(&f.ID, &f.InstrumentID, &f.Symbol, &requestedAt, &f.ReferencePriceCents,
		&f.ModelName, &f.Rationale, &enrichment, &enrichedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Forecast{}, err
		}
		return Forecast{}, fmt.Errorf("fintech: scan forecast: %w", err)
	}
	f.RequestedAt = parseTime(requestedAt)

	// An enrichment that no longer parses costs the forecast its deep dive,
	// not its existence: the prediction and its score are the record, the
	// commentary is an extra.
	if enrichment != "" {
		var e ForecastEnrichment
		if err := json.Unmarshal([]byte(enrichment), &e); err == nil {
			f.Enrichment = &e
		}
	}
	if enrichedAt != "" {
		at := parseTime(enrichedAt)
		f.EnrichedAt = &at
	}
	return f, nil
}

// DeleteForecast removes a forecast; its horizons go with it by cascade.
func (r *Repository) DeleteForecast(ctx context.Context, forecastID int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM fintech_forecasts WHERE id = ?`, forecastID)
	if err != nil {
		return fmt.Errorf("fintech: delete forecast: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("fintech: delete forecast: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateForecastEnrichment attaches the deep-dive analysis to a forecast.
func (r *Repository) UpdateForecastEnrichment(ctx context.Context, forecastID int64, enrichment ForecastEnrichment, enrichedAt time.Time) error {
	data, err := json.Marshal(enrichment)
	if err != nil {
		return fmt.Errorf("fintech: encode enrichment: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `
		UPDATE fintech_forecasts SET enrichment = ?, enriched_at = ? WHERE id = ?`,
		string(data), timestamp(enrichedAt), forecastID); err != nil {
		return fmt.Errorf("fintech: update forecast enrichment: %w", err)
	}
	return nil
}

func (r *Repository) listHorizons(ctx context.Context, forecastID int64) ([]ForecastHorizon, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, forecast_id, horizon_days, target_date, predicted_direction,
		       predicted_low_cents, predicted_high_cents, confidence,
		       actual_price_cents, actual_direction, within_predicted_range, evaluated_at
		FROM fintech_forecast_horizons
		WHERE forecast_id = ?
		ORDER BY horizon_days ASC`, forecastID)
	if err != nil {
		return nil, fmt.Errorf("fintech: list horizons: %w", err)
	}
	defer rows.Close()

	var out []ForecastHorizon
	for rows.Next() {
		var h ForecastHorizon
		var targetDate string
		var actualPrice sql.NullInt64
		var actualDirection sql.NullString
		var withinRange sql.NullBool
		var evaluatedAt sql.NullString
		if err := rows.Scan(&h.ID, &h.ForecastID, &h.HorizonDays, &targetDate, &h.PredictedDirection,
			&h.PredictedLowCents, &h.PredictedHighCents, &h.Confidence,
			&actualPrice, &actualDirection, &withinRange, &evaluatedAt); err != nil {
			return nil, fmt.Errorf("fintech: scan horizon: %w", err)
		}
		h.TargetDate, _ = time.Parse(dateLayout, targetDate)
		if actualPrice.Valid {
			v := actualPrice.Int64
			h.ActualPriceCents = &v
		}
		if actualDirection.Valid {
			v := actualDirection.String
			h.ActualDirection = &v
		}
		if withinRange.Valid {
			v := withinRange.Bool
			h.WithinPredictedRange = &v
		}
		if evaluatedAt.Valid && evaluatedAt.String != "" {
			v := parseTime(evaluatedAt.String)
			h.EvaluatedAt = &v
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// UpdateHorizonOutcome records what actually happened for one horizon.
func (r *Repository) UpdateHorizonOutcome(ctx context.Context, horizonID, actualPriceCents int64, actualDirection string, withinRange bool, evaluatedAt time.Time) error {
	within := 0
	if withinRange {
		within = 1
	}
	if _, err := r.db.ExecContext(ctx, `
		UPDATE fintech_forecast_horizons
		SET actual_price_cents = ?, actual_direction = ?, within_predicted_range = ?, evaluated_at = ?
		WHERE id = ?`,
		actualPriceCents, actualDirection, within, timestamp(evaluatedAt), horizonID); err != nil {
		return fmt.Errorf("fintech: update horizon outcome: %w", err)
	}
	return nil
}

// ListForecastIDsWithDueHorizons returns the forecasts holding at least one
// horizon whose target date has passed and which has not been scored, so the
// sweep asks for a price once per forecast rather than once per horizon.
func (r *Repository) ListForecastIDsWithDueHorizons(ctx context.Context) ([]ForecastRef, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT f.id, i.symbol
		FROM fintech_forecasts f
		JOIN fintech_instruments i ON i.id = f.instrument_id
		JOIN fintech_forecast_horizons h ON h.forecast_id = f.id
		WHERE h.evaluated_at IS NULL AND h.target_date <= ?
		ORDER BY f.id`, time.Now().UTC().Format(dateLayout))
	if err != nil {
		return nil, fmt.Errorf("fintech: list due forecasts: %w", err)
	}
	defer rows.Close()

	var out []ForecastRef
	for rows.Next() {
		var ref ForecastRef
		if err := rows.Scan(&ref.ForecastID, &ref.Symbol); err != nil {
			return nil, fmt.Errorf("fintech: scan due forecast: %w", err)
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}
