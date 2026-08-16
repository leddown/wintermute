package utilities

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
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

	stamp := time.Now().Format("2006-01-02T15-04-05")
	outDir := filepath.Join(destDir, "wintermute-backup-"+stamp)
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

	return BackupResult{
		Destination: outDir,
		Files:       []BackupFile{{Name: name, Size: fi.Size()}},
		CreatedAt:   time.Now(),
	}, nil
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
	case PruneTargetToolAudit:
		n, err = s.repo.PruneToolAudit(ctx, olderThanDays)
	case PruneTargetAIUsage:
		n, err = s.repo.PruneAIUsage(ctx, olderThanDays)
	default:
		return PruneResult{}, ErrInvalidPruneTarget
	}
	if err != nil {
		return PruneResult{}, err
	}
	return PruneResult{Target: target, OlderThanDays: olderThanDays, DeletedRows: n}, nil
}
