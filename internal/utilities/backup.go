package utilities

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupDirPrefix is how a snapshot directory is recognised. Retention only
// ever considers directories it wrote itself, so pointing the backup
// destination at a directory holding anything else cannot delete it.
const backupDirPrefix = "wintermute-backup-"

// Scheduler takes a verified snapshot every interval.
//
// It is opt-in: app.go starts it only when a destination and a non-zero
// interval are configured, because writing copies of the operator's entire
// database somewhere on a timer should be asked for rather than assumed.
//
// The first snapshot is taken one interval after start rather than
// immediately, so a server crash-looping on startup cannot fill a disk with
// backups of a database it never finished serving.
type Scheduler struct {
	svc      *Service
	destDir  string
	interval time.Duration
	keep     int
	log      *slog.Logger
}

// NewScheduler builds a backup scheduler. keep is how many snapshots to retain;
// zero or less keeps every snapshot ever taken.
func NewScheduler(svc *Service, destDir string, interval time.Duration, keep int, log *slog.Logger) *Scheduler {
	return &Scheduler{svc: svc, destDir: destDir, interval: interval, keep: keep, log: log}
}

// Run blocks, backing up once per interval until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.log.Info("backup scheduler started",
		"interval", s.interval, "destination", s.destDir, "keep", s.keep)
	for {
		select {
		case <-ctx.Done():
			s.log.Info("backup scheduler stopped")
			return
		case <-ticker.C:
			s.once(ctx)
		}
	}
}

// once takes one snapshot and applies retention. A failure is logged rather
// than propagated: the next tick will try again, and a backup that failed must
// not be able to stop the server it was protecting.
func (s *Scheduler) once(ctx context.Context) {
	res, err := s.svc.Backup(ctx, s.destDir)
	if err != nil {
		s.log.Error("scheduled backup failed", "destination", s.destDir, "error", err)
		return
	}
	s.log.Info("scheduled backup written",
		"destination", res.Destination, "verified", res.Verified,
		"messages", res.Rows["messages"], "sessions", res.Rows["sessions"])

	removed, err := s.svc.PruneBackups(s.destDir, s.keep)
	if err != nil {
		// Retention failing is not a reason to treat the backup as failed —
		// the copy exists and is verified, which is the part that matters.
		s.log.Warn("backup retention failed", "destination", s.destDir, "error", err)
		return
	}
	if removed > 0 {
		s.log.Info("old backups removed", "count", removed, "kept", s.keep)
	}
}

// PruneBackups deletes all but the newest keep snapshots in destDir, returning
// how many it removed. keep <= 0 keeps everything, which is the default: this
// is a deletion routine pointed at the operator's backups, and the safe
// default for that is to do nothing.
//
// Snapshot directories are named with a sortable UTC-ish timestamp, so
// lexicographic order is chronological order.
//
// The newest snapshot is never deleted regardless of keep. Retention exists so
// a disk does not fill; a full disk is bad, and having no backup at all is
// worse, so the two are not allowed to trade against each other.
func (s *Service) PruneBackups(destDir string, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	if !filepath.IsAbs(destDir) {
		return 0, ErrInvalidDestination
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("utilities: read backup directory: %w", err)
	}

	var snapshots []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), backupDirPrefix) {
			snapshots = append(snapshots, e.Name())
		}
	}
	if len(snapshots) <= keep {
		return 0, nil
	}
	sort.Strings(snapshots)

	// Everything except the newest `keep`.
	doomed := snapshots[:len(snapshots)-keep]
	var removed int
	for _, name := range doomed {
		if err := os.RemoveAll(filepath.Join(destDir, name)); err != nil {
			return removed, fmt.Errorf("utilities: remove old backup %s: %w", name, err)
		}
		removed++
	}
	return removed, nil
}
