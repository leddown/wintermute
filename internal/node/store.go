package node

import (
	"context"
	"database/sql"
	"embed"
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
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes (name, hostname, os, kernel, cores, agent_version, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   hostname = excluded.hostname,
		   os = excluded.os,
		   kernel = excluded.kernel,
		   cores = excluded.cores,
		   agent_version = excluded.agent_version,
		   last_seen_at = excluded.last_seen_at`,
		nodeName, f.Hostname, f.OS, f.Kernel, f.Cores, f.AgentVersion, now, now); err != nil {
		return 0, fmt.Errorf("record node: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO node_samples
		 (node, at, cpu_percent, load_1, load_5, load_15, mem_total, mem_used, swap_used,
		  disk_read_bps, disk_write_bps, net_rx_bps, net_tx_bps, uptime_seconds)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare ingest: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	var stored int
	for _, sample := range report.Samples {
		res, err := stmt.ExecContext(ctx,
			nodeName, sample.At.UTC(), sample.CPUPercent,
			sample.Load1, sample.Load5, sample.Load15,
			sample.MemTotal, sample.MemUsed, sample.SwapUsed,
			sample.DiskReadBPS, sample.DiskWriteBPS,
			sample.NetRxBPS, sample.NetTxBPS, sample.UptimeSeconds)
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
	FirstSeenAt  time.Time `json:"first_seen_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	// Latest is the newest sample, so a listing can show current state without
	// a second request per node.
	Latest *Sample `json:"latest,omitempty"`
}

// Nodes lists every known host with its most recent sample.
func (s *Store) Nodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, hostname, os, kernel, cores, agent_version, first_seen_at, last_seen_at
		 FROM nodes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Node{}
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.Name, &n.Hostname, &n.OS, &n.Kernel, &n.Cores,
			&n.AgentVersion, &n.FirstSeenAt, &n.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
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
	err := s.db.QueryRowContext(ctx,
		`SELECT at, cpu_percent, load_1, load_5, load_15, mem_total, mem_used, swap_used,
		        disk_read_bps, disk_write_bps, net_rx_bps, net_tx_bps, uptime_seconds
		 FROM node_samples WHERE node = ? ORDER BY at DESC LIMIT 1`, nodeName).
		Scan(&sample.At, &sample.CPUPercent, &sample.Load1, &sample.Load5, &sample.Load15,
			&sample.MemTotal, &sample.MemUsed, &sample.SwapUsed,
			&sample.DiskReadBPS, &sample.DiskWriteBPS,
			&sample.NetRxBPS, &sample.NetTxBPS, &sample.UptimeSeconds)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest sample: %w", err)
	}
	return &sample, nil
}

// SamplesSince returns one node's raw samples over a window, oldest first.
//
// Raw resolution only survives two hours, so this answers "what is it doing
// right now"; anything older is served from the rolled-up buckets.
func (s *Store) SamplesSince(ctx context.Context, nodeName string, since time.Time) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, cpu_percent, load_1, load_5, load_15, mem_total, mem_used, swap_used,
		        disk_read_bps, disk_write_bps, net_rx_bps, net_tx_bps, uptime_seconds
		 FROM node_samples WHERE node = ? AND at >= ? ORDER BY at`, nodeName, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("read samples: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Sample{}
	for rows.Next() {
		var sample Sample
		if err := rows.Scan(&sample.At, &sample.CPUPercent, &sample.Load1, &sample.Load5,
			&sample.Load15, &sample.MemTotal, &sample.MemUsed, &sample.SwapUsed,
			&sample.DiskReadBPS, &sample.DiskWriteBPS,
			&sample.NetRxBPS, &sample.NetTxBPS, &sample.UptimeSeconds); err != nil {
			return nil, fmt.Errorf("scan sample: %w", err)
		}
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
	res, err := s.db.ExecContext(ctx, `DELETE FROM node_samples WHERE at < ?`, olderThan.UTC())
	if err != nil {
		return 0, fmt.Errorf("prune raw samples: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune raw samples: %w", err)
	}
	return n, nil
}
