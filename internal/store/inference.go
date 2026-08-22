package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// InferenceSample is one completed model call, as stored.
type InferenceSample struct {
	Backend          string
	Model            string
	PromptTokens     int
	CompletionTokens int
	DurationMS       int64
	Failed           bool
	FellBack         bool
	CreatedAt        time.Time
}

// RecordInference writes a batch of samples in one transaction.
//
// Batched because samples arrive one per model call and a transaction per row
// would make the metrics more expensive to store than they are to gather. The
// caller buffers and hands them over in groups.
func (s *Store) RecordInference(ctx context.Context, samples []InferenceSample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin inference write: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO inference_samples
		 (backend, model, prompt_tokens, completion_tokens, duration_ms, failed, fell_back, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare inference write: %w", err)
	}
	defer stmt.Close()

	for _, sample := range samples {
		if _, err := stmt.ExecContext(ctx,
			sample.Backend, sample.Model, sample.PromptTokens, sample.CompletionTokens,
			sample.DurationMS, sample.Failed, sample.FellBack, sample.CreatedAt); err != nil {
			return fmt.Errorf("insert inference sample: %w", err)
		}
	}
	return tx.Commit()
}

// ModelPerformance is what one model on one backend has actually been doing.
//
// The rate is computed from summed tokens over summed time rather than from an
// average of per-call rates. Averaging rates weights a two-token reply the same
// as a two-thousand-token one, which flatters whichever model is asked the
// shortest questions.
type ModelPerformance struct {
	Backend string `json:"backend"`
	Model   string `json:"model"`
	Calls   int    `json:"calls"`
	Failed  int    `json:"failed"`
	// TokensPerSecond is output tokens over wall time across the window. Zero
	// when no call in the window reported usage.
	TokensPerSecond float64 `json:"tokens_per_second"`
	// MedianMS and SlowestMS bracket what a call actually cost. A mean would
	// be dragged around by one long generation; the pair says both what is
	// typical and what the worst case looked like.
	MedianMS  int64 `json:"median_ms"`
	SlowestMS int64 `json:"slowest_ms"`
	// PromptTokens and CompletionTokens are the totals over the window.
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	LastCallAt       time.Time `json:"last_call_at"`
}

// ModelPerformanceSince summarises every model's behaviour over a window.
//
// Failed calls are counted but excluded from the timing and rate figures: a
// backend that refuses in two seconds is not fast, and letting those rows into
// the average would say it was.
func (s *Store) ModelPerformanceSince(ctx context.Context, since time.Time) ([]ModelPerformance, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT backend, model,
		        COUNT(*),
		        SUM(CASE WHEN failed THEN 1 ELSE 0 END),
		        SUM(CASE WHEN failed THEN 0 ELSE duration_ms END),
		        SUM(CASE WHEN failed THEN 0 ELSE prompt_tokens END),
		        SUM(CASE WHEN failed THEN 0 ELSE completion_tokens END),
		        MAX(CASE WHEN failed THEN 0 ELSE duration_ms END),
		        MAX(created_at)
		 FROM inference_samples
		 WHERE created_at >= ?
		 GROUP BY backend, model
		 ORDER BY COUNT(*) DESC`, since)
	if err != nil {
		return nil, fmt.Errorf("read model performance: %w", err)
	}
	defer rows.Close()

	var out []ModelPerformance
	for rows.Next() {
		var p ModelPerformance
		var totalMS int64
		// MAX() over a timestamp column comes back as a string: an aggregate
		// loses the column's type affinity, so the driver has nothing to tell
		// it this is a time. Scanned as text and parsed here rather than
		// wrapped in a subquery, which would cost a second pass over the group
		// to recover a type the value never really lost.
		var lastCall string
		if err := rows.Scan(&p.Backend, &p.Model, &p.Calls, &p.Failed, &totalMS,
			&p.PromptTokens, &p.CompletionTokens, &p.SlowestMS, &lastCall); err != nil {
			return nil, fmt.Errorf("scan model performance: %w", err)
		}
		p.LastCallAt = parseStoredTime(lastCall)
		if totalMS > 0 && p.CompletionTokens > 0 {
			p.TokensPerSecond = float64(p.CompletionTokens) / (float64(totalMS) / 1000)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read model performance: %w", err)
	}

	// The median needs a second pass per model. SQLite has no percentile
	// function, and the alternative — a window function over the whole table —
	// costs far more than a bounded lookup per model on an index that already
	// exists.
	for i := range out {
		median, err := s.medianDuration(ctx, out[i].Backend, out[i].Model, since)
		if err != nil {
			return nil, err
		}
		out[i].MedianMS = median
	}
	return out, nil
}

// parseStoredTime reads a timestamp the driver handed back as text.
//
// The layouts are tried in the order they actually occur: the driver's own
// full-precision form first, then the shorter variants a hand-written row or an
// older write may have left. An unparseable value yields the zero time, which
// the UI renders as "never" — better than failing a whole summary over one
// malformed timestamp.
func parseStoredTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999 -0700 MST",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// medianDuration returns the middle successful call duration for one model.
func (s *Store) medianDuration(ctx context.Context, backend, model string, since time.Time) (int64, error) {
	var median int64
	err := s.db.QueryRowContext(ctx,
		`SELECT duration_ms FROM inference_samples
		 WHERE backend = ? AND model = ? AND created_at >= ? AND failed = 0
		 ORDER BY duration_ms
		 LIMIT 1
		 OFFSET (SELECT COUNT(*) / 2 FROM inference_samples
		         WHERE backend = ? AND model = ? AND created_at >= ? AND failed = 0)`,
		backend, model, since, backend, model, since).Scan(&median)
	if err != nil {
		// No successful calls in the window is a normal answer, not a failure.
		return 0, nil
	}
	return median, nil
}

// PruneInference deletes samples older than olderThanDays.
//
// A range delete on an index over created_at alone, so ageing the table out
// costs the same whether it holds a thousand rows or a million.
func (s *Store) PruneInference(ctx context.Context, olderThanDays int) (int64, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -olderThanDays)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM inference_samples WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune inference samples: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune inference samples: %w", err)
	}
	return n, nil
}
