package fintech

// Review persistence: the scheduled position reviews and the settings that
// decide when their digest may be sent.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GetReviewConfig returns the saved review settings, falling back to the same
// defaults the table declares so a caller never has to handle a missing row.
func (r *Repository) GetReviewConfig(ctx context.Context) (ReviewConfig, error) {
	cfg := ReviewConfig{
		AlertEnabled: true,
		QuietEnabled: true,
		QuietStart:   "22:00",
		QuietEnd:     "07:00",
	}
	var alert, quiet int
	err := r.db.QueryRowContext(ctx, `
		SELECT alert_enabled, quiet_enabled, quiet_start, quiet_end, timezone
		FROM fintech_review_config WHERE id = 1`,
	).Scan(&alert, &quiet, &cfg.QuietStart, &cfg.QuietEnd, &cfg.Timezone)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return ReviewConfig{}, fmt.Errorf("fintech: get review config: %w", err)
	}
	cfg.AlertEnabled, cfg.QuietEnabled = alert == 1, quiet == 1
	return cfg, nil
}

// SetReviewConfig saves those settings.
func (r *Repository) SetReviewConfig(ctx context.Context, cfg ReviewConfig) error {
	alert, quiet := 0, 0
	if cfg.AlertEnabled {
		alert = 1
	}
	if cfg.QuietEnabled {
		quiet = 1
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO fintech_review_config
			(id, alert_enabled, quiet_enabled, quiet_start, quiet_end, timezone, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			alert_enabled = excluded.alert_enabled, quiet_enabled = excluded.quiet_enabled,
			quiet_start = excluded.quiet_start, quiet_end = excluded.quiet_end,
			timezone = excluded.timezone, updated_at = excluded.updated_at`,
		alert, quiet, cfg.QuietStart, cfg.QuietEnd, cfg.Timezone, timestamp(time.Now()))
	if err != nil {
		return fmt.Errorf("fintech: set review config: %w", err)
	}
	return nil
}

// InsertReview persists one verdict.
func (r *Repository) InsertReview(ctx context.Context, rev Review) (Review, error) {
	if rev.ReviewedAt.IsZero() {
		rev.ReviewedAt = time.Now()
	}
	var forecastID any
	if rev.ForecastID != nil {
		forecastID = *rev.ForecastID
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO fintech_reviews
			(instrument_id, forecast_id, symbol, source, rating, rationale, reviewed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rev.InstrumentID, forecastID, rev.Symbol, string(rev.Source), string(rev.Rating),
		rev.Rationale, timestamp(rev.ReviewedAt))
	if err != nil {
		return Review{}, fmt.Errorf("fintech: insert review: %w", err)
	}
	if rev.ID, err = res.LastInsertId(); err != nil {
		return Review{}, fmt.Errorf("fintech: review id: %w", err)
	}
	return rev, nil
}

// ListReviews returns the most recent verdicts, newest first.
func (r *Repository) ListReviews(ctx context.Context, limit int) ([]Review, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, instrument_id, forecast_id, symbol, source, rating, rationale, reviewed_at, reported_at
		FROM fintech_reviews
		ORDER BY reviewed_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("fintech: list reviews: %w", err)
	}
	defer rows.Close()
	return scanReviews(rows)
}

// ListUnreportedReviews returns everything not yet in a digest, oldest first.
// This is the carry-over queue: a cycle suppressed by the quiet window leaves
// its rows here for the next delivery to collect.
func (r *Repository) ListUnreportedReviews(ctx context.Context) ([]Review, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, instrument_id, forecast_id, symbol, source, rating, rationale, reviewed_at, reported_at
		FROM fintech_reviews
		WHERE reported_at IS NULL
		ORDER BY reviewed_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("fintech: list unreported reviews: %w", err)
	}
	defer rows.Close()
	return scanReviews(rows)
}

// MarkReviewsReported stamps the rows a digest just carried, so the next one
// does not repeat them. The ids are expanded into placeholders because
// database/sql has no portable slice parameter.
func (r *Repository) MarkReviewsReported(ctx context.Context, ids []int64, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, timestamp(at))
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `UPDATE fintech_reviews SET reported_at = ? WHERE id IN (` + strings.Join(placeholders, ", ") + `)`
	if _, err := r.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("fintech: mark reviews reported: %w", err)
	}
	return nil
}

func scanReviews(rows *sql.Rows) ([]Review, error) {
	var out []Review
	for rows.Next() {
		var rev Review
		var forecastID sql.NullInt64
		var reviewedAt string
		var reportedAt sql.NullString
		if err := rows.Scan(&rev.ID, &rev.InstrumentID, &forecastID, &rev.Symbol,
			&rev.Source, &rev.Rating, &rev.Rationale, &reviewedAt, &reportedAt); err != nil {
			return nil, fmt.Errorf("fintech: scan review: %w", err)
		}
		if forecastID.Valid {
			v := forecastID.Int64
			rev.ForecastID = &v
		}
		rev.ReviewedAt = parseTime(reviewedAt)
		if reportedAt.Valid && reportedAt.String != "" {
			v := parseTime(reportedAt.String)
			rev.ReportedAt = &v
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}
