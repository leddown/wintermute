package node

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func newTestRoller(t *testing.T, s *Store, keep Retention) *Roller {
	t.Helper()
	return NewRoller(s, keep, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// ingestAt writes one sample at a given instant.
func ingestAt(t *testing.T, s *Store, name string, at time.Time, cpu float64) {
	t.Helper()
	if _, err := s.Ingest(context.Background(), name, Report{
		FormatVersion: ReportFormatVersion,
		Facts:         Facts{Hostname: name, Cores: 8},
		Samples: []Sample{{
			At: at, CPUPercent: cpu, Load1: cpu / 25,
			MemTotal: 32 << 30, MemUsed: 8 << 30, NetRxBPS: 100,
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

// A minute bucket must stand for exactly what raw would have said: the mean
// recovered from sum over count, and the peak carried through untouched.
func TestFoldingPreservesMeanAndPeak(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	r := newTestRoller(t, s, DefaultRetention())

	// Four readings inside one minute, well in the past so the minute is
	// complete and eligible to fold.
	minute := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	for i, cpu := range []float64{10, 20, 30, 100} {
		ingestAt(t, s, "rig", minute.Add(time.Duration(i)*time.Second), cpu)
	}

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}

	series, err := s.SeriesSince(ctx, "rig", minute.Add(-time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if series.Bucket == "raw" {
		t.Fatalf("a window past the raw retention was answered from raw rows")
	}
	if len(series.Points) != 1 {
		t.Fatalf("got %d points, want 1 minute bucket: %+v", len(series.Points), series.Points)
	}

	p := series.Points[0]
	if p.Samples != 4 {
		t.Errorf("bucket stands for %d samples, want 4", p.Samples)
	}
	// (10+20+30+100)/4 = 40
	if p.CPUPercent < 39.9 || p.CPUPercent > 40.1 {
		t.Errorf("mean CPU = %v, want 40", p.CPUPercent)
	}
	// The peak is the thing a mean destroys, and it is usually what matters.
	if p.CPUMax != 100 {
		t.Errorf("peak CPU = %v, want 100", p.CPUMax)
	}
}

// Hours are built from minutes and days from hours. That is only correct
// because no tier stores an average — summing sums and counts gives exactly
// what building from raw would have given.
func TestCoarserTiersReAggregateCorrectly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	r := newTestRoller(t, s, DefaultRetention())

	// Two hours ago, six readings spread over two minutes.
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	cpus := []float64{10, 20, 30, 40, 50, 60}
	for i, cpu := range cpus {
		ingestAt(t, s, "rig", base.Add(time.Duration(i*20)*time.Second), cpu)
	}

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// The hour bucket must have the same mean as the raw readings did.
	var want float64
	for _, c := range cpus {
		want += c
	}
	want /= float64(len(cpus))

	series, err := s.SeriesSince(ctx, "rig", base.Add(-time.Hour), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if series.Bucket != BucketMinute {
		t.Logf("window resolved to %s", series.Bucket)
	}

	var hour []Point
	rows, err := s.DB().QueryContext(ctx,
		`SELECT samples, cpu_sum, cpu_max FROM node_rollup WHERE bucket = ? AND node = ?`,
		BucketHour, "rig")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p Point
		var sum float64
		if err := rows.Scan(&p.Samples, &sum, &p.CPUMax); err != nil {
			t.Fatal(err)
		}
		if p.Samples > 0 {
			p.CPUPercent = sum / float64(p.Samples)
		}
		hour = append(hour, p)
	}
	if len(hour) != 1 {
		t.Fatalf("got %d hour buckets, want 1", len(hour))
	}
	if hour[0].Samples != len(cpus) {
		t.Errorf("hour bucket stands for %d samples, want %d", hour[0].Samples, len(cpus))
	}
	if diff := hour[0].CPUPercent - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("hour mean = %v, want %v — re-aggregation lost accuracy", hour[0].CPUPercent, want)
	}
	if hour[0].CPUMax != 60 {
		t.Errorf("hour peak = %v, want 60", hour[0].CPUMax)
	}
}

// The one mistake here that loses data permanently is deleting raw rows that
// were never summarised. Retention must never outrun the fold.
func TestRawIsNeverDeletedBeforeItIsFolded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	old := time.Now().UTC().Add(-5 * time.Hour).Truncate(time.Minute)
	for i := 0; i < 5; i++ {
		ingestAt(t, s, "rig", old.Add(time.Duration(i)*time.Minute), 50)
	}

	// A roller whose raw retention says everything here is long past its
	// window. The coarser tiers keep their normal spans, so what survives the
	// fold is not then immediately aged out — this test is about the ordering
	// of fold and prune, not about the coarser retentions.
	r := newTestRoller(t, s, Retention{
		Raw:    time.Minute,
		Minute: 30 * 24 * time.Hour,
		Hour:   365 * 24 * time.Hour,
	})

	// Prune alone, with nothing folded, must delete nothing.
	if err := r.prune(ctx); err != nil {
		t.Fatal(err)
	}
	left, err := s.SamplesSince(ctx, "rig", old.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 5 {
		t.Fatalf("pruning before folding destroyed %d unsummarised readings", 5-len(left))
	}

	// Once folding has run, the same retention may take them.
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	left, err = s.SamplesSince(ctx, "rig", old.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d raw rows survived after being folded and aged out", len(left))
	}
	// And the readings still exist, in summarised form.
	var buckets int
	if err := s.DB().QueryRow(
		`SELECT count(*) FROM node_rollup WHERE bucket = ?`, BucketMinute).Scan(&buckets); err != nil {
		t.Fatal(err)
	}
	if buckets == 0 {
		t.Error("raw was deleted and nothing was kept in its place")
	}
}

// The current, incomplete minute must not be folded: a bucket standing for a
// fraction of its interval would never be corrected.
func TestTheCurrentBucketIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	r := newTestRoller(t, s, DefaultRetention())

	now := time.Now().UTC()
	ingestAt(t, s, "rig", now, 90)

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	var buckets int
	if err := s.DB().QueryRow(
		`SELECT count(*) FROM node_rollup WHERE bucket = ? AND at >= ?`,
		BucketMinute, now.Truncate(time.Minute).Format(time.RFC3339)).Scan(&buckets); err != nil {
		t.Fatal(err)
	}
	if buckets != 0 {
		t.Error("the in-progress minute was folded before it finished")
	}
	// And the raw reading is still there, unharmed.
	left, err := s.SamplesSince(ctx, "rig", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("the current minute's raw reading went missing")
	}
}

// Folding is incremental: a second run must not redo the first one's work.
func TestFoldingIsIncremental(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	r := newTestRoller(t, s, DefaultRetention())

	base := time.Now().UTC().Add(-40 * time.Minute).Truncate(time.Minute)
	for i := 0; i < 5; i++ {
		ingestAt(t, s, "rig", base.Add(time.Duration(i)*time.Minute), 50)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}

	first, err := r.watermark(ctx, BucketMinute)
	if err != nil {
		t.Fatal(err)
	}
	if first.IsZero() {
		t.Fatal("the watermark did not advance after folding")
	}

	// Nothing new has arrived, so a second run has nothing to do and must
	// leave the watermark where it is or ahead of it, never behind.
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := r.watermark(ctx, BucketMinute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Before(first) {
		t.Errorf("the watermark went backwards: %v then %v", first, second)
	}
}

// The rule the whole tiering exists for: a window past the raw retention must
// be answered from buckets, never by scanning raw rows.
func TestWindowsPastTheRawRetentionNeverReadRaw(t *testing.T) {
	for _, tc := range []struct {
		window time.Duration
		want   string
	}{
		{30 * time.Minute, "raw"},
		{2 * time.Hour, "raw"},
		{6 * time.Hour, BucketMinute},
		{24 * time.Hour, BucketMinute},
		{7 * 24 * time.Hour, BucketHour},
		{365 * 24 * time.Hour, BucketDay},
	} {
		if got := bucketFor(tc.window, 2*time.Hour); got != tc.want {
			t.Errorf("a %s window resolved to %q, want %q", tc.window, got, tc.want)
		}
	}
}
