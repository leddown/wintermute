package node

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sampleAt(at time.Time, cpu float64) Sample {
	return Sample{
		At: at, CPUPercent: cpu, Load1: 1.5,
		MemTotal: 32 << 30, MemUsed: 8 << 30,
		NetRxBPS: 1000, UptimeSeconds: 3600,
	}
}

// A report registers the host and stores its readings.
func TestIngestRecordsFactsAndSamples(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	stored, err := s.Ingest(ctx, "rig", Report{
		FormatVersion: ReportFormatVersion,
		Facts:         Facts{Hostname: "rig.lan", OS: "linux", Kernel: "6.8.0", Cores: 16, AgentVersion: "1"},
		Samples:       []Sample{sampleAt(now.Add(-time.Minute), 20), sampleAt(now, 40)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored != 2 {
		t.Errorf("stored %d samples, want 2", stored)
	}

	nodes, err := s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	n := nodes[0]
	if n.Name != "rig" || n.Hostname != "rig.lan" || n.Cores != 16 {
		t.Errorf("node = %+v", n)
	}
	// The listing carries the newest reading so a dashboard needs one request.
	if n.Latest == nil {
		t.Fatal("listing carried no latest sample")
	}
	if n.Latest.CPUPercent != 40 {
		t.Errorf("latest CPU = %v, want the newest reading (40)", n.Latest.CPUPercent)
	}
}

// An agent that could not reach the server keeps collecting and sends the
// backlog later. If it resends a batch it was unsure landed, the replay must
// not double a spike.
func TestReplayedSamplesAreIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	batch := []Sample{sampleAt(now.Add(-2*time.Minute), 10), sampleAt(now.Add(-time.Minute), 20)}
	report := Report{FormatVersion: ReportFormatVersion, Facts: Facts{Hostname: "rig.lan"}, Samples: batch}

	if _, err := s.Ingest(ctx, "rig", report); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Ingest(ctx, "rig", report)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("a replayed batch stored %d new samples, want 0", stored)
	}

	samples, err := s.SamplesSince(ctx, "rig", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Errorf("after a replay there are %d samples, want 2", len(samples))
	}
}

// Samples land by their own timestamps, so a backlog replayed after an outage
// sits where it belongs in the series rather than all at the moment it arrived.
func TestBacklogLandsAtItsOwnTimestamps(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	// An hour of readings, all delivered at once.
	var backlog []Sample
	for i := 60; i > 0; i-- {
		backlog = append(backlog, sampleAt(now.Add(-time.Duration(i)*time.Minute), float64(i)))
	}
	if _, err := s.Ingest(ctx, "rig", Report{
		FormatVersion: ReportFormatVersion, Samples: backlog,
	}); err != nil {
		t.Fatal(err)
	}

	// A thirty minute window must contain only the readings from that window,
	// not the whole hour that happened to arrive together.
	recent, err := s.SamplesSince(ctx, "rig", now.Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) == 0 || len(recent) >= len(backlog) {
		t.Errorf("window returned %d of %d samples; the backlog collapsed to arrival time",
			len(recent), len(backlog))
	}
	for i := 1; i < len(recent); i++ {
		if recent[i].At.Before(recent[i-1].At) {
			t.Fatal("samples came back out of order")
		}
	}
}

// The two-hour window has to be enforced by something, and it must be a range
// delete rather than a scan.
func TestPruneRawDropsOnlyWhatIsPastTheWindow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.Ingest(ctx, "rig", Report{
		FormatVersion: ReportFormatVersion,
		Samples: []Sample{
			sampleAt(now.Add(-5*time.Hour), 10),
			sampleAt(now.Add(-3*time.Hour), 20),
			sampleAt(now.Add(-time.Hour), 30),
			sampleAt(now, 40),
		},
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.PruneRaw(ctx, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d rows, want the 2 past the window", deleted)
	}
	left, err := s.SamplesSince(ctx, "rig", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Errorf("%d samples left, want 2", len(left))
	}
	// The node itself survives having its old samples aged out — a quiet host
	// must not vanish from the fleet just because nothing happened recently.
	nodes, err := s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Errorf("pruning samples removed the node itself")
	}
}

// Decommissioning a machine should not leave it on the dashboard forever.
func TestForgetRemovesTheHostAndItsHistory(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()

	for _, name := range []string{"rig", "nuc"} {
		if _, err := s.Ingest(ctx, name, Report{
			FormatVersion: ReportFormatVersion, Samples: []Sample{sampleAt(now, 50)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Forget(ctx, "rig"); err != nil {
		t.Fatal(err)
	}

	nodes, err := s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "nuc" {
		t.Errorf("after forgetting rig the fleet is %+v", nodes)
	}
	samples, err := s.SamplesSince(ctx, "rig", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Errorf("forgetting a node left %d of its samples behind", len(samples))
	}
}

// Facts are re-sent with every report, so a host that reboots into a new
// kernel or gains memory updates without a separate enrolment step.
func TestFactsUpdateOnEveryReport(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()

	if _, err := s.Ingest(ctx, "rig", Report{
		FormatVersion: ReportFormatVersion,
		Facts:         Facts{Hostname: "rig.lan", Kernel: "6.8.0", Cores: 16},
		Samples:       []Sample{sampleAt(now.Add(-time.Hour), 10)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingest(ctx, "rig", Report{
		FormatVersion: ReportFormatVersion,
		Facts:         Facts{Hostname: "rig.lan", Kernel: "6.11.0", Cores: 32},
		Samples:       []Sample{sampleAt(now, 10)},
	}); err != nil {
		t.Fatal(err)
	}

	nodes, err := s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if nodes[0].Kernel != "6.11.0" || nodes[0].Cores != 32 {
		t.Errorf("facts did not update: %+v", nodes[0])
	}
	// And the first_seen date is not rewritten by the update.
	if !nodes[0].FirstSeenAt.Before(nodes[0].LastSeenAt.Add(time.Second)) {
		t.Error("first_seen_at moved forward with the update")
	}
}
