package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Folding raw telemetry into buckets, and ageing out what has been folded.
//
// The shape is the one every time-series store converges on, for the same
// reason: raw resolution is only interesting while it is recent, and keeping it
// is what turns a monitoring database into a problem. So raw lives briefly,
// minutes for a month, hours for a year, and days indefinitely — each tier a
// few hundred rows for any window anyone actually asks for.
//
// Two properties make it cheap rather than merely tidy:
//
//   - the job is watermark-driven, so each run folds only what arrived since
//     the last one instead of re-scanning a window;
//   - raw rows are deleted only up to the point the minute tier has confirmed,
//     so a retention sweep can never destroy readings that were never
//     summarised. Getting that order wrong is the one failure here that loses
//     data permanently, which is why it is enforced in code rather than by the
//     two intervals happening to be set sensibly.

// Bucket names. Also the values stored in node_rollup.bucket.
const (
	BucketMinute = "minute"
	BucketHour   = "hour"
	BucketDay    = "day"
)

// Retention defines how long each tier is kept.
type Retention struct {
	// Raw is how long full-resolution samples survive. Short on purpose.
	Raw time.Duration
	// Minute and Hour bound their tiers. Days are kept indefinitely: a row per
	// host per day is nothing, and it is the tier that answers "was this box
	// always like this".
	Minute time.Duration
	Hour   time.Duration
}

// DefaultRetention is two hours raw, a month of minutes, a year of hours, and
// days forever.
func DefaultRetention() Retention {
	return Retention{
		Raw:    2 * time.Hour,
		Minute: 30 * 24 * time.Hour,
		Hour:   365 * 24 * time.Hour,
	}
}

// Roller folds raw samples upward and applies retention.
type Roller struct {
	store *Store
	log   *slog.Logger
	keep  Retention
}

// NewRoller builds a Roller.
func NewRoller(s *Store, keep Retention, log *slog.Logger) *Roller {
	if keep.Raw <= 0 {
		keep = DefaultRetention()
	}
	return &Roller{store: s, log: log, keep: keep}
}

// Run folds and prunes on an interval until ctx is cancelled.
//
// The interval is a fraction of the raw window rather than equal to it, so a
// restart cannot leave raw rows sitting well past their retention waiting for a
// tick that is two hours away.
func (r *Roller) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = r.keep.Raw / 4
	}
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	r.log.Info("telemetry rollup started",
		"every", interval, "raw_kept", r.keep.Raw, "minutes_kept", r.keep.Minute)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("telemetry rollup stopped")
			return
		case <-ticker.C:
			if err := r.Once(ctx); err != nil {
				r.log.Warn("telemetry rollup failed", "error", err)
			}
		}
	}
}

// Once folds every tier and then applies retention.
//
// Order matters and is not incidental: minutes are built from raw, hours from
// minutes and days from hours, so each tier must be current before the one
// above it runs. Retention comes last, once every tier has confirmed what it
// holds.
func (r *Roller) Once(ctx context.Context) error {
	if err := r.foldRawToMinutes(ctx); err != nil {
		return err
	}
	if err := r.foldUp(ctx, BucketMinute, BucketHour, time.Hour); err != nil {
		return err
	}
	if err := r.foldUp(ctx, BucketHour, BucketDay, 24*time.Hour); err != nil {
		return err
	}
	return r.prune(ctx)
}

// sqliteTruncate is the strftime format that floors a timestamp to a bucket.
//
// Done in SQL rather than in Go so the grouping happens inside the query that
// scans the rows, rather than pulling every sample out to bucket it here.
func sqliteTruncate(bucket string) (string, error) {
	switch bucket {
	case BucketMinute:
		return "%Y-%m-%dT%H:%M:00Z", nil
	case BucketHour:
		return "%Y-%m-%dT%H:00:00Z", nil
	case BucketDay:
		return "%Y-%m-%dT00:00:00Z", nil
	default:
		return "", fmt.Errorf("unknown bucket %q", bucket)
	}
}

// foldRawToMinutes builds minute buckets from raw samples.
//
// Only complete minutes are folded. The current one is still filling, and
// folding it would write a bucket standing for a fraction of its interval that
// nothing would ever correct.
func (r *Roller) foldRawToMinutes(ctx context.Context) error {
	from, err := r.watermark(ctx, BucketMinute)
	if err != nil {
		return err
	}
	until := time.Now().UTC().Truncate(time.Minute)
	if !until.After(from) {
		return nil
	}
	format, err := sqliteTruncate(BucketMinute)
	if err != nil {
		return err
	}

	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin fold: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// INSERT OR REPLACE rather than INSERT: a bucket may be rewritten when a
	// node replays a backlog covering a minute already folded, and the replayed
	// aggregate is the more complete one.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO node_rollup
		 (node, bucket, at, samples,
		  cpu_sum, cpu_max, load1_sum, load1_max,
		  mem_used_sum, mem_used_max, mem_total_max, swap_used_max,
		  disk_read_sum, disk_read_max, disk_write_sum, disk_write_max,
		  net_rx_sum, net_rx_max, net_tx_sum, net_tx_max)
		 SELECT node, ?, strftime(?, at), COUNT(*),
		        SUM(cpu_percent), MAX(cpu_percent),
		        SUM(load_1), MAX(load_1),
		        SUM(mem_used), MAX(mem_used), MAX(mem_total), MAX(swap_used),
		        SUM(disk_read_bps), MAX(disk_read_bps),
		        SUM(disk_write_bps), MAX(disk_write_bps),
		        SUM(net_rx_bps), MAX(net_rx_bps),
		        SUM(net_tx_bps), MAX(net_tx_bps)
		 FROM node_samples
		 WHERE at >= ? AND at < ?
		 GROUP BY node, strftime(?, at)`,
		BucketMinute, format, stamp(from), stamp(until), format); err != nil {
		return fmt.Errorf("fold raw into minutes: %w", err)
	}

	if err := setWatermark(ctx, tx, BucketMinute, until); err != nil {
		return err
	}
	return tx.Commit()
}

// foldUp builds a coarser tier by re-aggregating a finer one.
//
// This is only correct because the finer tier stores sums and counts rather
// than averages: summing sums and summing counts gives exactly the figures the
// coarser bucket would have had if built from raw, and taking the maximum of
// maxima gives exactly the peak. An average of averages would not.
func (r *Roller) foldUp(ctx context.Context, from, to string, size time.Duration) error {
	since, err := r.watermark(ctx, to)
	if err != nil {
		return err
	}
	until := time.Now().UTC().Truncate(size)
	if !until.After(since) {
		return nil
	}
	format, err := sqliteTruncate(to)
	if err != nil {
		return err
	}

	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin fold: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO node_rollup
		 (node, bucket, at, samples,
		  cpu_sum, cpu_max, load1_sum, load1_max,
		  mem_used_sum, mem_used_max, mem_total_max, swap_used_max,
		  disk_read_sum, disk_read_max, disk_write_sum, disk_write_max,
		  net_rx_sum, net_rx_max, net_tx_sum, net_tx_max)
		 SELECT node, ?, strftime(?, at), SUM(samples),
		        SUM(cpu_sum), MAX(cpu_max),
		        SUM(load1_sum), MAX(load1_max),
		        SUM(mem_used_sum), MAX(mem_used_max), MAX(mem_total_max), MAX(swap_used_max),
		        SUM(disk_read_sum), MAX(disk_read_max),
		        SUM(disk_write_sum), MAX(disk_write_max),
		        SUM(net_rx_sum), MAX(net_rx_max),
		        SUM(net_tx_sum), MAX(net_tx_max)
		 FROM node_rollup
		 WHERE bucket = ? AND at >= ? AND at < ?
		 GROUP BY node, strftime(?, at)`,
		to, format, from, stamp(since), stamp(until), format); err != nil {
		return fmt.Errorf("fold %s into %s: %w", from, to, err)
	}

	if err := setWatermark(ctx, tx, to, until); err != nil {
		return err
	}
	return tx.Commit()
}

// prune ages out each tier.
//
// Raw is never deleted past the minute tier's watermark, whatever the retention
// says. That is the guard against the one mistake here that loses data
// permanently: deleting readings that were never summarised. If folding has
// stalled — a bug, a locked database, a server that has been down — raw simply
// accumulates until it recovers, which is the right way to fail.
func (r *Roller) prune(ctx context.Context) error {
	folded, err := r.watermark(ctx, BucketMinute)
	if err != nil {
		return err
	}

	rawCutoff := time.Now().UTC().Add(-r.keep.Raw)
	if folded.Before(rawCutoff) {
		rawCutoff = folded
	}
	if n, err := r.store.PruneRaw(ctx, rawCutoff); err != nil {
		return err
	} else if n > 0 {
		r.log.Debug("raw telemetry folded and aged out", "rows", n, "before", rawCutoff)
	}

	for _, tier := range []struct {
		bucket string
		keep   time.Duration
	}{
		{BucketMinute, r.keep.Minute},
		{BucketHour, r.keep.Hour},
	} {
		if tier.keep <= 0 {
			continue
		}
		cutoff := time.Now().UTC().Add(-tier.keep)
		res, err := r.store.db.ExecContext(ctx,
			`DELETE FROM node_rollup WHERE bucket = ? AND at < ?`,
			tier.bucket, stamp(cutoff))
		if err != nil {
			return fmt.Errorf("prune %s buckets: %w", tier.bucket, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			r.log.Debug("rolled-up telemetry aged out", "bucket", tier.bucket, "rows", n)
		}
	}
	return nil
}

// watermark reads how far a tier has been folded.
//
// A tier that has never run starts at the oldest thing it could fold, rather
// than at the epoch: starting at zero would make the first run scan a range
// covering every timestamp SQLite can represent to find the handful of rows
// that exist.
func (r *Roller) watermark(ctx context.Context, bucket string) (time.Time, error) {
	var raw string
	err := r.store.db.QueryRowContext(ctx,
		`SELECT through FROM node_rollup_watermark WHERE bucket = ?`, bucket).Scan(&raw)
	if err == nil {
		return parseTime(raw), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("read watermark: %w", err)
	}

	source := `SELECT MIN(at) FROM node_samples`
	args := []any{}
	if bucket != BucketMinute {
		source = `SELECT MIN(at) FROM node_rollup WHERE bucket = ?`
		args = append(args, sourceBucket(bucket))
	}
	var oldest sql.NullString
	if err := r.store.db.QueryRowContext(ctx, source, args...).Scan(&oldest); err != nil {
		return time.Time{}, fmt.Errorf("find oldest sample: %w", err)
	}
	if !oldest.Valid {
		// Nothing to fold yet. Start at now so the first real run has a
		// sensible floor rather than sweeping history that does not exist.
		return time.Now().UTC().Truncate(time.Minute), nil
	}
	return parseTime(oldest.String), nil
}

// sourceBucket names the tier a coarser one is built from.
func sourceBucket(bucket string) string {
	if bucket == BucketDay {
		return BucketHour
	}
	return BucketMinute
}

func setWatermark(ctx context.Context, tx *sql.Tx, bucket string, through time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO node_rollup_watermark (bucket, through) VALUES (?, ?)
		 ON CONFLICT(bucket) DO UPDATE SET through = excluded.through`,
		bucket, stamp(through))
	if err != nil {
		return fmt.Errorf("set watermark: %w", err)
	}
	return nil
}

// parseTime reads the timestamp forms this database stores.
//
// Two are in play: values written by the driver from a time.Time, and values
// written by strftime in the fold queries. Both are tried rather than assuming
// one, because a mismatch here would silently reset a watermark and refold
// everything on every run.
func parseTime(raw string) time.Time {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
