package node

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store owns the fleet telemetry database.
//
// Its own file, its own migration lineage. See 0001_nodes.sql for why this does
// not live beside the conversation memory.
type Store struct{ db *sql.DB }

// stamp renders a timestamp the way this database stores them.
//
// Explicit RFC3339 text rather than handing the driver a time.Time. The driver
// stores a time.Time in a form SQLite's own date functions cannot read, which
// makes strftime — and therefore the whole rollup, which groups by truncated
// timestamp inside the query — silently return NULL. Storing text costs a few
// bytes per row and keeps every date function working.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Open opens (creating if needed) the metrics database and applies migrations.
//
// The pragmas match the main store's, minus secure_delete: that one exists
// there because deleting a conversation must really delete it, and it costs
// write throughput. Telemetry has no such requirement and is deleted constantly
// by the retention pass, which is exactly the workload secure_delete makes
// slower.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open metrics database: %w", err)
	}
	db.SetMaxOpenConns(4)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for diagnostics and tests.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// Ingest records one report from a node, in a single transaction.
//
// nodeName comes from the authenticated client rather than from the report, so
// a node cannot write samples attributed to another.
//
// Samples are inserted with OR IGNORE against the unique (node, at) pair: a
// report may be a backlog replayed after an outage, and an agent that resends
// one it is unsure landed should not double a spike.
func (s *Store) Ingest(ctx context.Context, nodeName string, report Report) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin ingest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	f := report.Facts
	// The card list is stored as JSON: it is read whole, written whole, and has
	// no queryable structure worth a second table.
	var gpuJSON string
	if len(f.GPUs) > 0 {
		if raw, err := json.Marshal(f.GPUs); err == nil {
			gpuJSON = string(raw)
		}
	}
	// The model store, likewise stored whole. An agent without one omits the
	// field, and the column is then left as it was rather than blanked: an
	// older agent reporting alongside newer ones should not erase what a
	// newer one already said about the same host.
	var storeJSON string
	if report.Store != nil {
		if raw, err := json.Marshal(report.Store); err == nil {
			storeJSON = string(raw)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes (name, hostname, os, kernel, cores, agent_version, gpus, store, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   hostname = excluded.hostname,
		   os = excluded.os,
		   kernel = excluded.kernel,
		   cores = excluded.cores,
		   agent_version = excluded.agent_version,
		   gpus = excluded.gpus,
		   store = CASE WHEN excluded.store = '' THEN nodes.store ELSE excluded.store END,
		   last_seen_at = excluded.last_seen_at`,
		nodeName, f.Hostname, f.OS, f.Kernel, f.Cores, f.AgentVersion, gpuJSON, storeJSON,
		stamp(now), stamp(now)); err != nil {
		return 0, fmt.Errorf("record node: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO node_samples
		 (node, at, cpu_percent, load_1, load_5, load_15, mem_total, mem_used, swap_used,
		  disk_read_bps, disk_write_bps, net_rx_bps, net_tx_bps, uptime_seconds,
		  gpu_util, gpu_mem_used, gpu_mem_total, gpu_temp, gpu_power)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare ingest: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	var stored int
	for _, sample := range report.Samples {
		res, err := stmt.ExecContext(ctx,
			nodeName, stamp(sample.At), sample.CPUPercent,
			sample.Load1, sample.Load5, sample.Load15,
			sample.MemTotal, sample.MemUsed, sample.SwapUsed,
			sample.DiskReadBPS, sample.DiskWriteBPS,
			sample.NetRxBPS, sample.NetTxBPS, sample.UptimeSeconds,
			sample.GPUUtilPercent, sample.GPUMemUsed, sample.GPUMemTotal,
			sample.GPUTempC, sample.GPUPowerWatts)
		if err != nil {
			return 0, fmt.Errorf("insert sample: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			stored += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit ingest: %w", err)
	}
	return stored, nil
}

// Node is a host as last reported.
type Node struct {
	Name         string    `json:"name"`
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os,omitempty"`
	Kernel       string    `json:"kernel,omitempty"`
	Cores        int       `json:"cores,omitempty"`
	AgentVersion string    `json:"agent_version,omitempty"`
	GPUs         []GPUCard `json:"gpus,omitempty"`
	FirstSeenAt  time.Time `json:"first_seen_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	// Latest is the newest sample, so a listing can show current state without
	// a second request per node.
	Latest *Sample `json:"latest,omitempty"`
	// Store is the model library this host last reported holding. Nil for a
	// node with no store configured, which is every node that only reports
	// metrics.
	Store *StoreReport `json:"store,omitempty"`
}

// Nodes lists every known host with its most recent sample.
func (s *Store) Nodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, hostname, os, kernel, cores, agent_version, gpus, store, first_seen_at, last_seen_at
		 FROM nodes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Node{}
	for rows.Next() {
		var n Node
		var firstSeen, lastSeen, gpuJSON, storeJSON string
		if err := rows.Scan(&n.Name, &n.Hostname, &n.OS, &n.Kernel, &n.Cores,
			&n.AgentVersion, &gpuJSON, &storeJSON, &firstSeen, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		if gpuJSON != "" {
			// A card list that will not decode is not worth failing the whole
			// fleet listing over; the node is still real and still reporting.
			_ = json.Unmarshal([]byte(gpuJSON), &n.GPUs)
		}
		if storeJSON != "" {
			// A malformed column is left as a nil store rather than failing the
			// whole listing: one node's bad row must not hide the fleet.
			var sr StoreReport
			if json.Unmarshal([]byte(storeJSON), &sr) == nil {
				n.Store = &sr
			}
		}
		n.FirstSeenAt, n.LastSeenAt = parseTime(firstSeen), parseTime(lastSeen)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	for i := range out {
		latest, err := s.LatestSample(ctx, out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Latest = latest
	}
	return out, nil
}

// LatestSample returns a node's newest reading, or nil if it has sent none.
func (s *Store) LatestSample(ctx context.Context, nodeName string) (*Sample, error) {
	var sample Sample
	var at string
	err := s.db.QueryRowContext(ctx,
		`SELECT at, cpu_percent, load_1, load_5, load_15, mem_total, mem_used, swap_used,
		        disk_read_bps, disk_write_bps, net_rx_bps, net_tx_bps, uptime_seconds,
		        gpu_util, gpu_mem_used, gpu_mem_total, gpu_temp, gpu_power
		 FROM node_samples WHERE node = ? ORDER BY at DESC LIMIT 1`, nodeName).
		Scan(&at, &sample.CPUPercent, &sample.Load1, &sample.Load5, &sample.Load15,
			&sample.MemTotal, &sample.MemUsed, &sample.SwapUsed,
			&sample.DiskReadBPS, &sample.DiskWriteBPS,
			&sample.NetRxBPS, &sample.NetTxBPS, &sample.UptimeSeconds,
			&sample.GPUUtilPercent, &sample.GPUMemUsed, &sample.GPUMemTotal,
			&sample.GPUTempC, &sample.GPUPowerWatts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest sample: %w", err)
	}
	sample.At = parseTime(at)
	return &sample, nil
}

// SamplesSince returns one node's raw samples over a window, oldest first.
//
// Raw resolution only survives two hours, so this answers "what is it doing
// right now"; anything older is served from the rolled-up buckets.
func (s *Store) SamplesSince(ctx context.Context, nodeName string, since time.Time) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, cpu_percent, load_1, load_5, load_15, mem_total, mem_used, swap_used,
		        disk_read_bps, disk_write_bps, net_rx_bps, net_tx_bps, uptime_seconds,
		        gpu_util, gpu_mem_used, gpu_mem_total, gpu_temp, gpu_power
		 FROM node_samples WHERE node = ? AND at >= ? ORDER BY at`, nodeName, stamp(since))
	if err != nil {
		return nil, fmt.Errorf("read samples: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Sample{}
	for rows.Next() {
		var sample Sample
		var at string
		if err := rows.Scan(&at, &sample.CPUPercent, &sample.Load1, &sample.Load5,
			&sample.Load15, &sample.MemTotal, &sample.MemUsed, &sample.SwapUsed,
			&sample.DiskReadBPS, &sample.DiskWriteBPS,
			&sample.NetRxBPS, &sample.NetTxBPS, &sample.UptimeSeconds,
			&sample.GPUUtilPercent, &sample.GPUMemUsed, &sample.GPUMemTotal,
			&sample.GPUTempC, &sample.GPUPowerWatts); err != nil {
			return nil, fmt.Errorf("scan sample: %w", err)
		}
		sample.At = parseTime(at)
		out = append(out, sample)
	}
	return out, rows.Err()
}

// Forget removes a node and everything it ever reported.
func (s *Store) Forget(ctx context.Context, nodeName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin forget: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM node_samples WHERE node = ?`, nodeName); err != nil {
		return fmt.Errorf("delete node samples: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE name = ?`, nodeName); err != nil {
		return fmt.Errorf("delete node: %w", err)
	}
	return tx.Commit()
}

// PruneRaw deletes raw samples older than the cutoff, returning how many went.
//
// A range delete against an index over time alone, so it costs the same whether
// the table holds a thousand rows or a million. This is what keeps the two-hour
// window from becoming a growing table nobody noticed.
func (s *Store) PruneRaw(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM node_samples WHERE at < ?`, stamp(olderThan))
	if err != nil {
		return 0, fmt.Errorf("prune raw samples: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune raw samples: %w", err)
	}
	return n, nil
}

// ---- reading a window ------------------------------------------------------

// Point is one plotted value, whatever tier it came from.
//
// Means are recovered here by dividing a stored sum by its stored count, which
// is correct at every tier precisely because no tier stored a mean. Peaks come
// through untouched, because a peak cannot be recovered from an average and is
// usually the thing worth seeing.
type Point struct {
	At      time.Time `json:"at"`
	Samples int       `json:"samples"`

	CPUPercent   float64 `json:"cpu_percent"`
	CPUMax       float64 `json:"cpu_max"`
	Load1        float64 `json:"load_1"`
	Load1Max     float64 `json:"load_1_max"`
	MemUsed      float64 `json:"mem_used_bytes"`
	MemUsedMax   int64   `json:"mem_used_max_bytes"`
	MemTotal     int64   `json:"mem_total_bytes"`
	SwapUsedMax  int64   `json:"swap_used_max_bytes"`
	DiskReadBPS  float64 `json:"disk_read_bps"`
	DiskWriteBPS float64 `json:"disk_write_bps"`
	NetRxBPS     float64 `json:"net_rx_bps"`
	NetTxBPS     float64 `json:"net_tx_bps"`

	// GPU figures, aggregated across the host's cards. Zero throughout on a
	// machine with no NVIDIA driver, which is most of them.
	GPUUtilPercent float64 `json:"gpu_util_percent"`
	GPUUtilMax     float64 `json:"gpu_util_max"`
	GPUMemUsedMax  int64   `json:"gpu_mem_used_max_bytes"`
	GPUMemTotal    int64   `json:"gpu_mem_total_bytes"`
	GPUTempMax     float64 `json:"gpu_temp_max_c"`
}

// Series is a node's history over a window, at whatever resolution suits it.
type Series struct {
	Node string `json:"node"`
	// Bucket names the resolution actually used: "raw", "minute", "hour" or
	// "day". Reported rather than implied, so a chart can say what it is
	// showing instead of letting the reader assume full resolution.
	Bucket string  `json:"bucket"`
	Points []Point `json:"points"`
}

// bucketFor picks the resolution that answers a window without scanning.
//
// This function is where "no query outside the raw window ever touches a raw
// row" stops being a comment and becomes true. Every window gets a few hundred
// points: a day at minute resolution is 1,440 rows, a month at hourly is 720, a
// decade at daily is 3,650. The alternative — reading raw and downsampling in
// the caller — is what makes a monitoring page get slower every week, and it is
// avoided by never being reachable.
func bucketFor(window time.Duration, rawWindow time.Duration) string {
	switch {
	case window <= rawWindow:
		return "raw"
	case window <= 36*time.Hour:
		return BucketMinute
	case window <= 90*24*time.Hour:
		return BucketHour
	default:
		return BucketDay
	}
}

// SeriesSince returns a node's history from `since` until now, choosing the
// coarsest resolution that still has enough points to be worth plotting.
func (s *Store) SeriesSince(ctx context.Context, nodeName string, since time.Time, rawWindow time.Duration) (Series, error) {
	if rawWindow <= 0 {
		rawWindow = 2 * time.Hour
	}
	bucket := bucketFor(time.Since(since), rawWindow)
	out := Series{Node: nodeName, Bucket: bucket, Points: []Point{}}

	if bucket == "raw" {
		samples, err := s.SamplesSince(ctx, nodeName, since)
		if err != nil {
			return out, err
		}
		for _, sample := range samples {
			out.Points = append(out.Points, Point{
				At: sample.At, Samples: 1,
				CPUPercent: sample.CPUPercent, CPUMax: sample.CPUPercent,
				Load1: sample.Load1, Load1Max: sample.Load1,
				MemUsed: float64(sample.MemUsed), MemUsedMax: int64(sample.MemUsed),
				MemTotal: int64(sample.MemTotal), SwapUsedMax: int64(sample.SwapUsed),
				DiskReadBPS: sample.DiskReadBPS, DiskWriteBPS: sample.DiskWriteBPS,
				NetRxBPS: sample.NetRxBPS, NetTxBPS: sample.NetTxBPS,
				GPUUtilPercent: sample.GPUUtilPercent, GPUUtilMax: sample.GPUUtilPercent,
				GPUMemUsedMax: sample.GPUMemUsed, GPUMemTotal: sample.GPUMemTotal,
				GPUTempMax: sample.GPUTempC,
			})
		}
		return out, nil
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT at, samples, cpu_sum, cpu_max, load1_sum, load1_max,
		        mem_used_sum, mem_used_max, mem_total_max, swap_used_max,
		        disk_read_sum, disk_write_sum, net_rx_sum, net_tx_sum,
		        gpu_util_sum, gpu_util_max, gpu_mem_used_max, gpu_mem_total_max, gpu_temp_max
		 FROM node_rollup
		 WHERE node = ? AND bucket = ? AND at >= ?
		 ORDER BY at`, nodeName, bucket, stamp(since))
	if err != nil {
		return out, fmt.Errorf("read series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var p Point
		var at string
		var cpuSum, load1Sum, memUsedSum, diskReadSum, diskWriteSum, netRxSum, netTxSum float64
		var gpuUtilSum float64
		if err := rows.Scan(&at, &p.Samples, &cpuSum, &p.CPUMax, &load1Sum, &p.Load1Max,
			&memUsedSum, &p.MemUsedMax, &p.MemTotal, &p.SwapUsedMax,
			&diskReadSum, &diskWriteSum, &netRxSum, &netTxSum,
			&gpuUtilSum, &p.GPUUtilMax, &p.GPUMemUsedMax, &p.GPUMemTotal, &p.GPUTempMax); err != nil {
			return out, fmt.Errorf("scan series point: %w", err)
		}
		p.At = parseTime(at)
		if p.Samples > 0 {
			n := float64(p.Samples)
			p.CPUPercent = cpuSum / n
			p.Load1 = load1Sum / n
			p.MemUsed = memUsedSum / n
			p.DiskReadBPS = diskReadSum / n
			p.DiskWriteBPS = diskWriteSum / n
			p.NetRxBPS = netRxSum / n
			p.NetTxBPS = netTxSum / n
			p.GPUUtilPercent = gpuUtilSum / n
		}
		out.Points = append(out.Points, p)
	}
	return out, rows.Err()
}
