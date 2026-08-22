// Package utilities provides housekeeping operations for the wintermute
// server: database backups, system diagnostics, live resource sampling,
// database maintenance, and data pruning. It has no tables of its own — all
// work is either transient (filesystem I/O, /proc reads) or queries against
// other modules' tables.
//
// This came across from morpheus, where the same view did the same six jobs
// against PostgreSQL. What the database is changes most of the implementation
// and none of the interface: pg_dump becomes VACUUM INTO, pg_database_size
// becomes page_count × page_size, pg_stat_user_tables becomes sqlite_master
// plus dbstat, and VACUUM ANALYZE becomes VACUUM then ANALYZE.
//
// One thing did not come across. Morpheus's Utilities view could export its
// media catalog as a JSON document to upload to a wintermute agent; that
// export reads media_files, media_shares and media_metadata, and wintermute
// has no media library of its own to read. It stays on the morpheus side,
// which is the end of that pipe that has the data.
package utilities

import (
	"errors"
	"time"
)

// ErrInvalidDestination is returned when the backup destination path is not
// an absolute filesystem path.
var ErrInvalidDestination = errors.New("utilities: destination must be an absolute path")

// ErrInvalidPruneTarget is returned for an unrecognised prune target name.
var ErrInvalidPruneTarget = errors.New("utilities: unknown prune target")

// ErrInvalidDays is returned when older_than_days is less than 1.
var ErrInvalidDays = errors.New("utilities: older_than_days must be at least 1")

// ---- backup ----------------------------------------------------------------

// BackupFile describes one file written during a backup run.
type BackupFile struct {
	Name string `json:"name"`
	Size int64  `json:"size_bytes"`
	// SHA256 lets a copy made years later be checked against what was
	// actually written, which is the difference between having a file and
	// having the file.
	SHA256 string `json:"sha256,omitempty"`
}

// BackupResult summarises a completed backup run.
type BackupResult struct {
	// Destination is the timestamped subdirectory actually written to,
	// e.g. /chosen/path/wintermute-backup-2026-07-05T15-04-05.
	Destination string       `json:"destination"`
	Files       []BackupFile `json:"files"`
	CreatedAt   time.Time    `json:"created_at"`

	// Verified reports that the snapshot was reopened after being written and
	// found to be a readable, internally consistent database.
	//
	// An unverified backup is not a backup. The whole point of this file is to
	// still be readable after the machine that wrote it is gone, and the only
	// way to know that is to open it and look — so Backup does, every time,
	// rather than trusting that VACUUM INTO returned without an error.
	Verified bool `json:"verified"`
	// Integrity is SQLite's own verdict: "ok", or the first problem it found.
	Integrity string `json:"integrity"`
	// Rows counts what the snapshot actually contains, per table. A backup
	// that verifies clean but holds no messages is a successful backup of
	// nothing, and that is worth being able to see.
	Rows map[string]int64 `json:"rows"`
}

// Snapshot manifest.

// backupManifestVersion is the manifest schema's own version, so a reader
// years from now can tell what it is looking at before it parses the rest.
const backupManifestVersion = 1

// BackupManifest is written alongside the snapshot as manifest.json, so the
// directory describes itself without needing this program to interpret it.
// It is the difference between an archive and an unlabelled binary blob.
type BackupManifest struct {
	ManifestVersion int              `json:"manifest_version"`
	Application     string           `json:"application"`
	CreatedAt       time.Time        `json:"created_at"`
	SourcePath      string           `json:"source_path"`
	SchemaVersion   string           `json:"schema_version"`
	Files           []BackupFile     `json:"files"`
	Integrity       string           `json:"integrity"`
	Rows            map[string]int64 `json:"rows"`
}

// ---- system info -----------------------------------------------------------

// TableStat holds a table's live row count and its on-disk size.
type TableStat struct {
	Name      string `json:"name"`
	RowCount  int64  `json:"row_count"`
	SizeBytes int64  `json:"size_bytes"`
}

// DiskInfo reports disk capacity for a given filesystem path.
type DiskInfo struct {
	Path       string `json:"path"`
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

// SystemInfo aggregates server and database diagnostics.
type SystemInfo struct {
	DatabasePath      string `json:"database_path"`
	DatabaseSizeBytes int64  `json:"database_size_bytes"`
	// WALSizeBytes is the write-ahead log beside the database. A WAL that
	// keeps growing is the usual sign of a reader holding a transaction open,
	// so it is worth seeing next to the database itself.
	WALSizeBytes  int64       `json:"wal_size_bytes"`
	Tables        []TableStat `json:"tables"`
	Disk          DiskInfo    `json:"disk"`
	GoVersion     string      `json:"go_version"`
	UptimeSeconds float64     `json:"uptime_seconds"`
}

// ---- api usage -------------------------------------------------------------

// APIUsageSource reports model API usage for one subsystem.
//
// The cache-token fields are carried from morpheus's shape and are always zero
// here: they are an Anthropic prompt-caching measure, and what this server
// records is a backend-agnostic input/output count that has nowhere to put
// them. They are kept rather than dropped so the two deployments' usage
// documents stay the same shape.
type APIUsageSource struct {
	Name              string `json:"name"`
	RequestCount      int64  `json:"request_count"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CacheCreateTokens int64  `json:"cache_creation_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	TodayRequestCount int64  `json:"today_request_count"`
	TodayInputTokens  int64  `json:"today_input_tokens"`
	TodayOutputTokens int64  `json:"today_output_tokens"`
}

// APIUsage breaks model API usage down by the subsystem that made the calls,
// alongside a combined total across all of them.
type APIUsage struct {
	Sources []APIUsageSource `json:"sources"`
	Total   APIUsageSource   `json:"total"`
	// Note explains what the numbers do and do not cover. The chat agent does
	// not record token counts at all, so a total that looked like the whole
	// server's spend would be wrong; saying so beside the figure is cheaper
	// than the alternative, which is somebody trusting it.
	Note string `json:"note,omitempty"`
}

// ---- vacuum ----------------------------------------------------------------

// VacuumResult reports how long VACUUM took and how much it reclaimed.
type VacuumResult struct {
	DurationMs int64 `json:"duration_ms"`
	// BeforeBytes and AfterBytes bracket the run. Unlike Postgres, where
	// VACUUM usually returns space to the table's free-space map rather than
	// to the filesystem, SQLite's VACUUM rewrites the whole file — so the
	// difference here is real, visible, and worth reporting.
	BeforeBytes    int64 `json:"before_bytes"`
	AfterBytes     int64 `json:"after_bytes"`
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
}

// ---- prune -----------------------------------------------------------------

// Prune targets. Morpheus had one (its assistant conversations); these are the
// three tables here that grow without bound and that nothing else ever cleans
// up.
const (
	// PruneTargetSessions deletes whole conversations by last activity.
	// Messages and audit rows go with them via ON DELETE CASCADE.
	PruneTargetSessions = "sessions"
	// PruneTargetMuninn deletes audit rows on their own, leaving the
	// conversations they belong to. The audit trail is the bulky part of an
	// old session and the part least often read back.
	PruneTargetMuninn = "muninn"
	// PruneTargetToolAudit is what muninn was called before 0012_muninn.sql.
	// It is still accepted so a browser left open across the rename, or a
	// saved request, prunes what the operator meant rather than failing with
	// "unknown target" for a table that does exist under another name.
	PruneTargetToolAudit = "tool_audit"
	// PruneTargetAIUsage deletes recorded model-call costs.
	PruneTargetAIUsage = "fintech_ai_usage"
)

// PruneResult reports how many rows were deleted by a prune operation.
type PruneResult struct {
	Target        string `json:"target"`
	OlderThanDays int    `json:"older_than_days"`
	DeletedRows   int64  `json:"deleted_rows"`
}
