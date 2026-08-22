package utilities

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// Service performs administrative operations: backups, system diagnostics,
// database maintenance, and data pruning.
type Service struct {
	repo *Repository
	// databasePath is the SQLite file this server opened. Backup writes a copy
	// of it, and the diagnostics report its size and the disk holding it.
	databasePath string
	startedAt    time.Time
	// resources samples live CPU/network/disk rates. It holds the counter
	// history the rates are measured against, so it has to be one long-lived
	// instance.
	resources resourceSampler
}

// NewService builds a Service. db is used for diagnostic and maintenance
// queries; databasePath is the file backups copy and diagnostics measure.
func NewService(db *sql.DB, databasePath string) *Service {
	return &Service{
		repo:         NewRepository(db),
		databasePath: databasePath,
		startedAt:    time.Now(),
	}
}

// ---- backup ----------------------------------------------------------------

// Backup writes a consistent copy of the database into a timestamped
// subdirectory of destDir. It returns ErrInvalidDestination when destDir is
// not an absolute path.
//
// This is SQLite's VACUUM INTO, which is the whole reason the backup here
// needs no external tool where morpheus needed pg_dump on PATH. It takes a
// read lock, writes a fresh, defragmented database file, and is safe against a
// live server still serving requests — unlike copying the file with cp, which
// can catch a write in progress and produce a corrupt result.
func (s *Service) Backup(ctx context.Context, destDir string) (BackupResult, error) {
	if !filepath.IsAbs(destDir) {
		return BackupResult{}, ErrInvalidDestination
	}

	// UTC, not local time. Retention (PruneBackups) relies on these names
	// sorting chronologically, and local time does not: an hour after a
	// daylight-saving rollback, a new snapshot would sort *before* one taken
	// earlier and become the candidate for deletion.
	stamp := time.Now().UTC().Format("2006-01-02T15-04-05")
	outDir := filepath.Join(destDir, backupDirPrefix+stamp)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return BackupResult{}, fmt.Errorf("utilities: create backup directory: %w", err)
	}

	name := filepath.Base(s.databasePath)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "wintermute.db"
	}
	dbFile := filepath.Join(outDir, name)

	if err := s.repo.BackupTo(ctx, dbFile); err != nil {
		os.RemoveAll(outDir)
		return BackupResult{}, err
	}

	fi, err := os.Stat(dbFile)
	if err != nil {
		return BackupResult{}, fmt.Errorf("utilities: stat backup file: %w", err)
	}
	sum, err := sha256File(dbFile)
	if err != nil {
		return BackupResult{}, err
	}

	// Reopen what was just written and check it. A backup that has never been
	// opened is an assumption, not a copy — and a snapshot that fails here is
	// worse than no snapshot, because it would be trusted.
	check, err := verifySnapshot(ctx, dbFile)
	if err != nil {
		// The unreadable file is removed rather than left looking like a
		// backup. Leaving it would put a broken copy in the retention set,
		// where it could be the one still standing when it is needed.
		os.RemoveAll(outDir)
		return BackupResult{}, fmt.Errorf("utilities: backup failed verification: %w", err)
	}

	result := BackupResult{
		Destination: outDir,
		Files:       []BackupFile{{Name: name, Size: fi.Size(), SHA256: sum}},
		CreatedAt:   time.Now(),
		Verified:    check.Integrity == "ok",
		Integrity:   check.Integrity,
		Rows:        check.Rows,
	}
	if !result.Verified {
		os.RemoveAll(outDir)
		return BackupResult{}, fmt.Errorf("utilities: backup failed integrity check: %s", check.Integrity)
	}

	if err := writeManifest(outDir, s.databasePath, check.SchemaVersion, result); err != nil {
		return BackupResult{}, err
	}
	return result, nil
}

// verifySnapshot opens a freshly written snapshot read-only and asks SQLite
// whether it is sound, then counts what it holds.
//
// query_only is set so that opening the file to check it cannot modify it —
// verification must not be able to damage the thing it is verifying.
func verifySnapshot(ctx context.Context, path string) (snapshotCheck, error) {
	var out snapshotCheck
	db, err := sql.Open("sqlite", path+"?_pragma=query_only(1)&_pragma=foreign_keys(ON)")
	if err != nil {
		return out, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&out.Integrity); err != nil {
		return out, fmt.Errorf("integrity check: %w", err)
	}
	if out.Integrity != "ok" {
		return out, nil
	}

	// The newest applied migration names the schema this file was written
	// under, which is what a future reader needs before interpreting it.
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(name), '') FROM schema_migrations`).Scan(&out.SchemaVersion); err != nil {
		return out, fmt.Errorf("read schema version: %w", err)
	}

	// Row counts double as proof that the tables are actually readable, not
	// merely structurally intact.
	out.Rows = map[string]int64{}
	for _, table := range backupTables {
		var n int64
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			// A table absent from an older snapshot is not a corruption.
			continue
		}
		out.Rows[table] = n
	}
	return out, nil
}

// snapshotCheck is what reopening a snapshot established about it.
type snapshotCheck struct {
	Integrity     string
	SchemaVersion string
	Rows          map[string]int64
}

// backupTables are the tables whose counts are recorded with a snapshot. These
// are the ones whose loss would be unrecoverable: the conversations, what was
// done in them, and who owned them.
var backupTables = []string{"sessions", "messages", "muninn", "clients"}

// writeManifest makes the snapshot directory self-describing.
func writeManifest(outDir, sourcePath, schemaVersion string, res BackupResult) error {
	manifest := BackupManifest{
		ManifestVersion: backupManifestVersion,
		Application:     "wintermute",
		CreatedAt:       res.CreatedAt.UTC(),
		SourcePath:      sourcePath,
		SchemaVersion:   schemaVersion,
		Files:           res.Files,
		Integrity:       res.Integrity,
		Rows:            res.Rows,
	}
	buf, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("utilities: encode manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), append(buf, '\n'), 0o640); err != nil {
		return fmt.Errorf("utilities: write manifest: %w", err)
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("utilities: open for checksum: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("utilities: checksum: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- system info -----------------------------------------------------------

// SystemInfo returns server diagnostics: database and WAL size, per-table
// stats, disk usage, Go version, and app uptime.
func (s *Service) SystemInfo(ctx context.Context) (SystemInfo, error) {
	dbSize, err := s.repo.DatabaseSize(ctx)
	if err != nil {
		return SystemInfo{}, err
	}
	tables, err := s.repo.TableStats(ctx)
	if err != nil {
		return SystemInfo{}, err
	}
	if tables == nil {
		tables = []TableStat{}
	}

	var walSize int64
	if fi, statErr := os.Stat(s.databasePath + "-wal"); statErr == nil {
		walSize = fi.Size()
	}

	// The filesystem holding the database, not "/" as morpheus reported. There
	// the database was on another host entirely and "/" was the only honest
	// thing to measure; here the database is a file this process owns, and the
	// disk that matters is the one it will run out of space on.
	return SystemInfo{
		DatabasePath:      s.databasePath,
		DatabaseSizeBytes: dbSize,
		WALSizeBytes:      walSize,
		Tables:            tables,
		Disk:              diskUsage(databaseDir(s.databasePath)),
		GoVersion:         runtime.Version(),
		UptimeSeconds:     time.Since(s.startedAt).Seconds(),
	}, nil
}

// databaseDir is the directory holding the database file, which is what statfs
// needs. A relative or bare filename resolves to the working directory.
func databaseDir(path string) string {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "/"
	}
	return dir
}

// Resources returns the current CPU, network and disk rates, averaged over the
// last few seconds. Unlike SystemInfo's Disk field — which is capacity, how
// full the filesystem is — these are throughput: how hard the machine is
// working right now.
func (s *Service) Resources() ResourceSample {
	return s.resources.Sample()
}

// diskUsage returns disk capacity info for the filesystem containing path.
// On error (e.g. path does not exist) it returns a zero-value DiskInfo.
func diskUsage(path string) DiskInfo {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return DiskInfo{Path: path}
	}
	total := stat.Blocks * uint64(stat.Bsize) //nolint:unconvert
	free := stat.Bfree * uint64(stat.Bsize)   //nolint:unconvert
	return DiskInfo{
		Path:       path,
		TotalBytes: total,
		UsedBytes:  total - free,
		FreeBytes:  free,
	}
}

// ---- api usage -------------------------------------------------------------

// usageNote states the limit of these figures. See APIUsage.
const usageNote = "Counts cover the portfolio's forecasting and review calls, which are the only " +
	"model calls this server records. Chat turns and tool-use loops are not counted."

// APIUsage reports recorded model API usage broken down by the kind of call,
// plus a combined total.
//
// It is deliberately explicit about being partial. Morpheus counted its
// assistant's usage because every assistant message carried Anthropic's token
// counts; here a turn can go to any backend — several of them local, where
// tokens cost nothing and are not reported the same way — and the agent loop
// records none. Rather than present a total that silently omits the largest
// consumer, the shortfall is named in the payload.
func (s *Service) APIUsage(ctx context.Context) (APIUsage, error) {
	sources, err := s.repo.AIUsageByKind(ctx)
	if err != nil {
		return APIUsage{}, err
	}
	if sources == nil {
		sources = []APIUsageSource{}
	}
	total := APIUsageSource{Name: "total"}
	for _, src := range sources {
		total.RequestCount += src.RequestCount
		total.InputTokens += src.InputTokens
		total.OutputTokens += src.OutputTokens
		total.CacheCreateTokens += src.CacheCreateTokens
		total.CacheReadTokens += src.CacheReadTokens
		total.TodayRequestCount += src.TodayRequestCount
		total.TodayInputTokens += src.TodayInputTokens
		total.TodayOutputTokens += src.TodayOutputTokens
	}
	return APIUsage{Sources: sources, Total: total, Note: usageNote}, nil
}

// ---- vacuum ----------------------------------------------------------------

// Vacuum rebuilds the database file and refreshes planner statistics,
// reporting how long it took and how much space came back.
func (s *Service) Vacuum(ctx context.Context) (VacuumResult, error) {
	before, err := s.repo.DatabaseSize(ctx)
	if err != nil {
		return VacuumResult{}, err
	}

	start := time.Now()
	if err := s.repo.Vacuum(ctx); err != nil {
		return VacuumResult{}, err
	}
	elapsed := time.Since(start)

	after, err := s.repo.DatabaseSize(ctx)
	if err != nil {
		return VacuumResult{}, err
	}

	reclaimed := before - after
	if reclaimed < 0 {
		// A concurrent write during the rebuild can leave the file larger than
		// it started. Report nothing reclaimed rather than a negative figure.
		reclaimed = 0
	}
	return VacuumResult{
		DurationMs:     elapsed.Milliseconds(),
		BeforeBytes:    before,
		AfterBytes:     after,
		ReclaimedBytes: reclaimed,
	}, nil
}

// ---- prune -----------------------------------------------------------------

// Prune deletes rows from target (one of the PruneTarget* constants) older
// than olderThanDays days. Returns the number deleted.
func (s *Service) Prune(ctx context.Context, target string, olderThanDays int) (PruneResult, error) {
	if olderThanDays < 1 {
		return PruneResult{}, ErrInvalidDays
	}
	var n int64
	var err error
	switch target {
	case PruneTargetSessions:
		n, err = s.repo.PruneSessions(ctx, olderThanDays)
	case PruneTargetMuninn, PruneTargetToolAudit:
		n, err = s.repo.PruneMuninn(ctx, olderThanDays)
	case PruneTargetAIUsage:
		n, err = s.repo.PruneAIUsage(ctx, olderThanDays)
	case PruneTargetInference:
		n, err = s.repo.PruneInference(ctx, olderThanDays)
	default:
		return PruneResult{}, ErrInvalidPruneTarget
	}
	if err != nil {
		return PruneResult{}, err
	}
	return PruneResult{Target: target, OlderThanDays: olderThanDays, DeletedRows: n}, nil
}
