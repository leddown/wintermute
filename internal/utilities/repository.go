package utilities

// Administrative queries, on SQLite.
//
// Two things here need more care than the rest of the port, and both are about
// how a timestamp is spelled in this database.
//
// The tables from 0001_init.sql (sessions, messages, muninn, clients) are
// written by passing a time.Time straight to the driver, and modernc's driver
// renders that with Go's default layout:
//
//	2026-08-16 08:33:19.806269268 +0000 UTC
//
// The tables added later (fintech, twire, todo) are written by their own
// repositories in RFC 3339:
//
//	2026-08-16T08:33:19Z
//
// Both are UTC and both lead with a zero-padded date, so each is orderable
// lexically — but they are not orderable against *each other*, and neither
// matches what SQLite's own datetime() produces. A prune therefore formats its
// cutoff in whichever layout the table it is about to touch actually uses, and
// compares on the leading characters the two layouts agree on. Getting this
// wrong deletes either everything or nothing, which is why each target names
// its layout explicitly rather than sharing one helper that guesses.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// driverTimeLayout is how database/sql's driver renders a time.Time into the
// tables that hand it one directly. Only the first 19 characters are compared
// against — date and time to the second — because everything after that
// (fractional seconds, zone) differs in length between values.
const driverTimeLayout = "2006-01-02 15:04:05"

// The 19 in each substr() below is the length of that layout — date and time
// to the second — and is the stretch every stamp in this database is
// fixed-width over, whichever layout wrote it.

// Repository runs administrative queries against the SQLite database.
type Repository struct {
	db *sql.DB
}

// NewRepository builds a Repository backed by db.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// DatabaseSize returns the logical size of the database in bytes.
//
// This is page_count × page_size rather than the size of the file on disk. The
// two differ after a delete: the pages are freed inside the file and the file
// itself does not shrink until VACUUM. Reporting the logical size is what makes
// the vacuum button's before/after figures mean something.
func (r *Repository) DatabaseSize(ctx context.Context) (int64, error) {
	var pageCount, pageSize int64
	if err := r.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("utilities: query page count: %w", err)
	}
	if err := r.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("utilities: query page size: %w", err)
	}
	return pageCount * pageSize, nil
}

// TableStats returns per-table row counts and on-disk sizes, largest first.
//
// Postgres kept both numbers in pg_stat_user_tables, ready to read. SQLite
// keeps neither: the sizes come from the dbstat virtual table, and the row
// counts have to be counted, one COUNT(*) per table. That is a full scan each,
// which is why this is a diagnostics screen someone opens rather than anything
// on a request path.
func (r *Repository) TableStats(ctx context.Context) ([]TableStat, error) {
	names, err := r.tableNames(ctx)
	if err != nil {
		return nil, err
	}
	sizes, err := r.tableSizes(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]TableStat, 0, len(names))
	for _, name := range names {
		t := TableStat{Name: name, SizeBytes: sizes[name]}
		// The name comes from sqlite_master, never from a request, so it is
		// safe to interpolate — and it has to be, because a table name cannot
		// be a bound parameter.
		if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM "`+name+`"`).Scan(&t.RowCount); err != nil {
			return nil, fmt.Errorf("utilities: count rows in %s: %w", name, err)
		}
		out = append(out, t)
	}
	sortTableStats(out)
	return out, nil
}

// tableNames lists the application's tables, excluding SQLite's internal ones
// (sqlite_sequence, sqlite_stat1, and the rest of the sqlite_ prefix).
func (r *Repository) tableNames(ctx context.Context) ([]string, error) {
	virtual, err := r.virtualTables(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("utilities: list tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("utilities: scan table name: %w", err)
		}
		// A virtual table's storage lives in shadow tables it creates for
		// itself — recall_fts_data, recall_fts_idx and so on. Listing those
		// beside the real tables fills the diagnostics with names nobody
		// recognises, so they are folded into their parent instead, the same
		// way indexes already are.
		if shadowOf(name, virtual) != "" {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// virtualTables names the virtual tables in the schema.
func (r *Repository) virtualTables(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND sql LIKE 'CREATE VIRTUAL TABLE%'`)
	if err != nil {
		return nil, fmt.Errorf("utilities: list virtual tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("utilities: scan virtual table name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// shadowOf reports which virtual table a shadow table belongs to, or "" when
// the name is not a shadow of any of them.
func shadowOf(name string, virtual []string) string {
	for _, v := range virtual {
		if strings.HasPrefix(name, v+"_") {
			return v
		}
	}
	return ""
}

// tableSizes reads per-table byte totals from dbstat, which accounts for the
// table's own pages plus its indexes and overflow.
//
// dbstat is a compile-time option. It is present in the modernc build this
// server uses, but a missing one costs the page only its size column: the
// error is swallowed and every size reads zero, rather than the whole
// diagnostics panel failing over a nicety.
func (r *Repository) tableSizes(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	rows, err := r.db.QueryContext(ctx, `SELECT name, sum(pgsize) FROM dbstat GROUP BY name`)
	if err != nil {
		return out, nil //nolint:nilerr // no dbstat: sizes are unavailable, not fatal
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var size sql.NullInt64
		if err := rows.Scan(&name, &size); err != nil {
			return nil, fmt.Errorf("utilities: scan table size: %w", err)
		}
		// dbstat reports indexes under their own names; fold them onto the
		// table they belong to so a table's size is its real footprint.
		out[name] += size.Int64
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("utilities: read table sizes: %w", err)
	}

	// Fold each virtual table's shadow storage onto the virtual table itself,
	// which otherwise reports zero bytes while its shadows report all of them.
	virtual, err := r.virtualTables(ctx)
	if err != nil {
		return nil, err
	}
	for name, size := range out {
		if parent := shadowOf(name, virtual); parent != "" {
			out[parent] += size
			delete(out, name)
		}
	}

	return r.foldIndexSizes(ctx, out)
}

// foldIndexSizes moves each index's bytes onto its parent table. Without it a
// heavily indexed table looks small and a list of index names nobody
// recognises appears beside it.
func (r *Repository) foldIndexSizes(ctx context.Context, sizes map[string]int64) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT name, tbl_name FROM sqlite_master
		WHERE type = 'index' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return sizes, nil //nolint:nilerr // same reasoning as tableSizes
	}
	defer rows.Close()
	for rows.Next() {
		var index, table string
		if err := rows.Scan(&index, &table); err != nil {
			return nil, fmt.Errorf("utilities: scan index owner: %w", err)
		}
		if n, ok := sizes[index]; ok {
			sizes[table] += n
			delete(sizes, index)
		}
	}
	return sizes, rows.Err()
}

// AIUsageByKind aggregates recorded model-call costs, one source per kind of
// call (forecast, enrichment, review).
//
// Morpheus split its usage by the subsystem that spent it. Here every recorded
// call comes from the one subsystem that records any — the portfolio — so the
// useful split is by what the call was for. See Service.APIUsage for what this
// deliberately does not count.
func (r *Repository) AIUsageByKind(ctx context.Context) ([]APIUsageSource, error) {
	// created_at is RFC 3339 here (fintech writes it), so the cutoff for
	// "today" is formatted to match and compared on the date alone.
	today := time.Now().UTC().Format("2006-01-02")
	rows, err := r.db.QueryContext(ctx, `
		SELECT kind,
		       count(*),
		       coalesce(sum(input_tokens), 0),
		       coalesce(sum(output_tokens), 0),
		       count(*) FILTER (WHERE substr(created_at, 1, 10) >= ?),
		       coalesce(sum(input_tokens) FILTER (WHERE substr(created_at, 1, 10) >= ?), 0),
		       coalesce(sum(output_tokens) FILTER (WHERE substr(created_at, 1, 10) >= ?), 0)
		FROM fintech_ai_usage
		GROUP BY kind
		ORDER BY kind`, today, today, today)
	if err != nil {
		return nil, fmt.Errorf("utilities: query ai usage: %w", err)
	}
	defer rows.Close()

	var out []APIUsageSource
	for rows.Next() {
		var s APIUsageSource
		if err := rows.Scan(&s.Name, &s.RequestCount, &s.InputTokens, &s.OutputTokens,
			&s.TodayRequestCount, &s.TodayInputTokens, &s.TodayOutputTokens); err != nil {
			return nil, fmt.Errorf("utilities: scan ai usage: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Vacuum rewrites the database file, reclaiming free pages, then refreshes the
// planner's statistics.
//
// Both statements run on one dedicated connection. VACUUM cannot run inside a
// transaction and rewrites the whole file, so it wants a connection to itself;
// taking one explicitly also keeps the pool from handing this long statement's
// connection to a concurrent request mid-run.
func (r *Repository) Vacuum(ctx context.Context) error {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("utilities: acquire connection for vacuum: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("utilities: vacuum: %w", err)
	}
	// Postgres folded this into VACUUM ANALYZE; SQLite spells it separately.
	if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
		return fmt.Errorf("utilities: analyze: %w", err)
	}
	return nil
}

// PruneSessions deletes conversations last touched more than olderThanDays ago.
// Their messages and audit rows go with them through ON DELETE CASCADE.
func (r *Repository) PruneSessions(ctx context.Context, olderThanDays int) (int64, error) {
	cutoff := driverCutoff(olderThanDays)
	return r.exec(ctx, "prune sessions",
		`DELETE FROM sessions WHERE substr(updated_at, 1, 19) < ?`, cutoff)
}

// PruneInference deletes measured model-call timings older than olderThanDays.
//
// A range delete against an index over created_at alone, so it costs the same
// whether the table holds a thousand rows or a million.
func (r *Repository) PruneInference(ctx context.Context, olderThanDays int) (int64, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -olderThanDays).Format("2006-01-02T15:04:05")
	return r.exec(ctx, "prune inference samples",
		`DELETE FROM inference_samples WHERE substr(created_at, 1, 19) < ?`, cutoff)
}

// PruneMuninn deletes audit rows older than olderThanDays, leaving the
// sessions they belong to in place.
func (r *Repository) PruneMuninn(ctx context.Context, olderThanDays int) (int64, error) {
	cutoff := driverCutoff(olderThanDays)
	return r.exec(ctx, "prune tool audit",
		`DELETE FROM muninn WHERE substr(created_at, 1, 19) < ?`, cutoff)
}

// PruneAIUsage deletes recorded model-call costs older than olderThanDays.
// This table is written in RFC 3339, not the driver layout.
func (r *Repository) PruneAIUsage(ctx context.Context, olderThanDays int) (int64, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -olderThanDays).Format(time.RFC3339)
	return r.exec(ctx, "prune ai usage",
		`DELETE FROM fintech_ai_usage WHERE created_at < ?`, cutoff)
}

// driverCutoff renders the cutoff in the layout the driver wrote, truncated to
// the second so it lines up with substr(col, 1, 19).
func driverCutoff(olderThanDays int) string {
	return time.Now().UTC().AddDate(0, 0, -olderThanDays).Format(driverTimeLayout)
}

// exec runs a delete and reports how many rows it removed.
func (r *Repository) exec(ctx context.Context, what, query string, args ...any) (int64, error) {
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("utilities: %s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("utilities: %s rows affected: %w", what, err)
	}
	return n, nil
}

// sortTableStats orders by size descending, then by name so the listing does
// not reshuffle between refreshes when sizes tie (which they do constantly —
// dbstat reports in whole pages).
func sortTableStats(in []TableStat) {
	sort.Slice(in, func(i, j int) bool {
		if in[i].SizeBytes != in[j].SizeBytes {
			return in[i].SizeBytes > in[j].SizeBytes
		}
		return in[i].Name < in[j].Name
	})
}

// BackupTo writes a consistent copy of the database to path using VACUUM INTO.
//
// The path is interpolated into the statement because VACUUM INTO takes a
// string literal, not a bound parameter. It is quote-escaped for that reason —
// the destination reaches here from a request body, and a path containing a
// single quote would otherwise end the literal and leave the rest of it being
// parsed as SQL.
//
// SQLite refuses to overwrite an existing file, which is a useful safety rather
// than an obstacle: the caller writes into a freshly created timestamped
// directory, so a collision means something is wrong.
func (r *Repository) BackupTo(ctx context.Context, path string) error {
	quoted := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	if _, err := r.db.ExecContext(ctx, `VACUUM INTO `+quoted); err != nil {
		return fmt.Errorf("utilities: back up database: %w", err)
	}
	return nil
}
