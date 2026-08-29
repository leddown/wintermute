// Package modelrepo manages a library of model weights on a disk the server
// owns — in practice an external drive attached to the wintermute host.
//
// This is the one place in the server that writes large files, and it is
// deliberately the *only* thing it does with them. It downloads weights into a
// single configured directory, records where they came from, and lets the
// operator label and delete them. It does not run them, register them with a
// backend, or execute anything anywhere. A file in this repository is inert
// until something outside this program is pointed at its path.
//
// Three properties are load-bearing:
//
//   - The disk is the truth. A listing walks the directory and uses the index
//     only for provenance the filesystem cannot hold. A GGUF copied in by hand
//     appears without ceremony; an indexed file that has vanished is reported
//     as missing rather than quietly believed.
//   - The root must prove it is mounted. An unplugged USB drive leaves its
//     mount point behind as an ordinary empty directory, and writing twelve
//     gigabytes into that directory would silently fill the server's own root
//     filesystem. A marker file on the drive itself is what distinguishes the
//     two, and it is created by an explicit operator action.
//   - Every path is confined after symlink resolution. Filenames come from
//     Hugging Face, which is to say from outside, and are treated as hostile.
package modelrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"wintermute/internal/models"
	"wintermute/internal/store"
)

// markerName is the file that proves the configured root is the operator's
// repository and not a bare mount point standing in for an absent drive.
//
// Checked before every write. The failure it prevents is quiet and expensive:
// with the drive unplugged, /mnt/models is still a perfectly good directory on
// the root filesystem, and a download would fill it up while reporting success.
const markerName = ".wintermute-repo"

// weightSuffix is the extension the repository indexes. Only GGUF for now:
// it is the format a single file can be, which is what makes a repository of
// loose files coherent at all. Safetensors arrives as a directory of shards
// plus a config, and pretending a shard is a model would be a lie the rest of
// this package would have to keep telling.
const weightSuffix = ".gguf"

// partSuffix marks an incomplete download. These are skipped by listings and
// left in place when a download fails, because they are what a resume starts
// from.
const partSuffix = ".part"

// ErrNotConfigured reports that no repository path was set.
var ErrNotConfigured = errors.New("no model repository is configured: set WINTERMUTE_MODEL_REPO")

// ErrUnavailable reports that the configured path is not usable right now —
// most often an external drive that is not mounted.
var ErrUnavailable = errors.New("the model repository is not available")

// ErrOutsideRepo reports a path that resolved outside the repository root.
var ErrOutsideRepo = errors.New("path is outside the model repository")

// ErrInvalidRequest reports a download this repository will not accept —
// a malformed repository id, a filename that is not weights, a traversal.
//
// A sentinel rather than a bare error because the API has to tell the
// operator's mistake from the server's fault: answering "your filename has a
// .. in it" with "internal error" makes the whole feature undiagnosable.
var ErrInvalidRequest = errors.New("invalid download request")

// ErrNotWritable reports a repository the server can see but cannot write to.
//
// Distinct from ErrUnavailable, which means the path is not there at all. This
// one is the harder failure to diagnose, because the directory is plainly
// present and usually writable from a shell — the process simply is not the
// user that owns it, or systemd has made the path read-only. Both are the
// operator's to fix and neither is a fault in this server, so it must never be
// reported as an internal error.
var ErrNotWritable = errors.New("the model repository is not writable")

// Repo is the model library rooted at one directory.
type Repo struct {
	// root is the configured path, exactly as the operator wrote it. It is
	// deliberately not resolved once at startup: a USB drive can be mounted,
	// unmounted and remounted while the server runs, and a root resolved at
	// boot would go stale in a way that is hard to see.
	root  string
	store *store.Store
	log   *slog.Logger
	jobs  *Jobs
	down  *Downloader
}

// New builds a Repo over a configured path. An empty path is not an error: the
// feature is simply off, and every entry point says so rather than the server
// refusing to start over a drive it does not strictly need.
func New(root, hubToken string, st *store.Store, log *slog.Logger) *Repo {
	r := &Repo{root: strings.TrimSpace(root), store: st, log: log, jobs: NewJobs()}
	r.down = NewDownloader(r, hubToken, log)
	return r
}

// Configured reports whether a repository path was set at all.
func (r *Repo) Configured() bool { return r.root != "" }

// Jobs exposes the download registry.
func (r *Repo) Jobs() *Jobs { return r.jobs }

// Downloader exposes the fetcher.
func (r *Repo) Downloader() *Downloader { return r.down }

// Root returns the configured path, for display.
func (r *Repo) Root() string { return r.root }

// resolve returns the symlink-free absolute root, or an error saying why the
// repository cannot be used right now.
//
// Called on every operation rather than cached. The whole point of an external
// drive is that it comes and goes.
func (r *Repo) resolve() (string, error) {
	if r.root == "" {
		return "", ErrNotConfigured
	}
	abs, err := filepath.Abs(r.root)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%w: %s is not there", ErrUnavailable, abs)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrUnavailable, abs)
	}
	// Resolve symlinks so a path reached through a link and one given directly
	// compare equal, exactly as internal/client/actions/roots.go does. A
	// failure falls back to the absolute path rather than aborting, because
	// EvalSymlinks fails on some network filesystems.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return filepath.Clean(abs), nil
}

// Ready returns the resolved root, refusing unless the marker file is present.
//
// Every write goes through this. Reads use resolve, so an uninitialised
// directory can still be listed and told apart from an unmounted one.
func (r *Repo) Ready() (string, error) {
	root, err := r.resolve()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(root, markerName)); err != nil {
		return "", fmt.Errorf("%w: %s carries no %s marker, so it may be an empty mount point "+
			"rather than the drive — initialise it if this is the right directory",
			ErrUnavailable, root, markerName)
	}
	return root, nil
}

// Initialise writes the marker file, blessing this directory as the repository.
//
// A deliberate operator action rather than something done on first use, because
// its whole job is to be the step that cannot happen by accident: if the drive
// is absent, the marker is absent, and nothing writes.
func (r *Repo) Initialise() error {
	root, err := r.resolve()
	if err != nil {
		return err
	}
	path := filepath.Join(root, markerName)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	content := "This directory is a wintermute model repository.\n" +
		"Its presence is how the server tells a mounted drive from an empty mount point.\n" +
		"Deleting it does not delete any weights; it stops the server writing here.\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return writeFailure(root, err)
	}
	return nil
}

// writeFailure turns a refused write into something an operator can act on.
//
// A bare "permission denied" is the least useful true statement available here:
// the operator is looking at a directory they can write to perfectly well from
// their own shell, and nothing in that message hints at either of the two
// things that are actually wrong. So both are named, along with the identity
// the server is actually running as, which is the fact that resolves it.
func writeFailure(root string, err error) error {
	// A read-only filesystem is the systemd case, and it is worth telling
	// apart from a permissions one because the fixes do not overlap at all.
	// Under ProtectSystem=strict every path outside StateDirectory is mounted
	// read-only for the service, so writes fail with EROFS however the
	// directory is owned — chown and group membership change nothing, which is
	// a genuinely disorienting thing to discover one command at a time.
	if errors.Is(err, syscall.EROFS) {
		return fmt.Errorf("%w: %s is on a read-only filesystem. If this server runs "+
			"under systemd, that is almost certainly ProtectSystem=strict, which makes "+
			"everything outside StateDirectory read-only no matter who owns it — add "+
			"ReadWritePaths=%s to the unit, then daemon-reload and restart. Otherwise "+
			"the mount itself is read-only (%v)",
			ErrNotWritable, root, root, err)
	}
	if !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w: %s: %v", ErrNotWritable, root, err)
	}

	detail := fmt.Sprintf("%s: permission denied, writing as uid %d gid %d",
		root, os.Getuid(), os.Getgid())
	if info, statErr := os.Stat(root); statErr == nil {
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			detail += fmt.Sprintf("; the directory is owned by uid %d gid %d with mode %v",
				sys.Uid, sys.Gid, info.Mode().Perm())
		}
	}
	return fmt.Errorf("%w: %s. Two things cause this and both are outside this "+
		"server: the directory may not be owned by the user it runs as "+
		"(chown it), or systemd's ProtectSystem=strict may be making it "+
		"read-only (add ReadWritePaths=%s to the unit and reload)",
		ErrNotWritable, detail, root)
}

// safeJoin resolves a repository-relative path against the root and verifies it
// stays inside.
//
// The containment check is done on the cleaned path *and* again after symlink
// resolution when the target exists, so neither "../../etc/passwd" nor a
// symlink planted inside the repository can reach out of it. Filenames here
// come from Hugging Face, so they are untrusted input in the same sense as a
// path in a tool call.
func (r *Repo) safeJoin(root, rel string) (string, error) {
	// Checked before normalisation, which strips leading separators. Without
	// this, "/etc/passwd" would quietly become "etc/passwd" *inside* the
	// repository — confined, and so not a breach, but a caller that passed an
	// absolute path has misunderstood something and deserves to be told rather
	// than to have its input silently redefined.
	if filepath.IsAbs(strings.TrimSpace(rel)) || strings.HasPrefix(strings.TrimSpace(rel), "/") {
		return "", fmt.Errorf("%q: %w (paths here are relative to the repository root)",
			rel, ErrOutsideRepo)
	}
	rel = store.RepoKey(rel)
	if rel == "" {
		return "", errors.New("a path within the repository is required")
	}
	if strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") || rel == ".." {
		return "", fmt.Errorf("%q: %w", rel, ErrOutsideRepo)
	}
	abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if !within(abs, root) {
		return "", fmt.Errorf("%q: %w", rel, ErrOutsideRepo)
	}
	// A file that already exists may be a symlink pointing anywhere. Resolve
	// and check again; a path that does not exist yet has nothing to resolve
	// and its parent was covered by the check above.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		if !within(real, root) {
			return "", fmt.Errorf("%q: %w (it is a link out of the repository)", rel, ErrOutsideRepo)
		}
	}
	return abs, nil
}

// within reports whether abs is at or below root.
func within(abs, root string) bool {
	if abs == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(abs, prefix)
}

// Status describes the repository for the UI.
type Status struct {
	Configured bool   `json:"configured"`
	Root       string `json:"root,omitempty"`
	// Available means the path resolves to a directory right now.
	Available bool `json:"available"`
	// Initialised means the marker file is present, so writing is allowed.
	Initialised bool   `json:"initialised"`
	Detail      string `json:"detail,omitempty"`

	TotalBytes int64 `json:"total_bytes,omitempty"`
	FreeBytes  int64 `json:"free_bytes,omitempty"`
	// UsedByRepoBytes is what the weights themselves occupy, which is the
	// number that answers "what can I delete", as opposed to how full the
	// filesystem happens to be.
	UsedByRepoBytes int64 `json:"used_by_repo_bytes,omitempty"`
	FileCount       int   `json:"file_count"`
}

// Entry is one model file in the repository, as presented to the UI.
type Entry struct {
	RelPath   string  `json:"rel_path"`
	Name      string  `json:"name"`
	SizeBytes int64   `json:"size_bytes"`
	Quant     string  `json:"quant,omitempty"`
	ParamsB   float64 `json:"params_b,omitempty"`
	// Estimated marks a parameter count or quantisation worked out from the
	// filename rather than recorded from the Hub. The UI says so, because the
	// fit verdict downstream is only as good as this.
	Estimated bool     `json:"estimated,omitempty"`
	HubID     string   `json:"hub_id,omitempty"`
	SourceURL string   `json:"source_url,omitempty"`
	SHA256    string   `json:"sha256,omitempty"`
	Verified  bool     `json:"verified"`
	Tags      []string `json:"tags,omitempty"`
	// Missing marks a file the index remembers but the disk no longer has —
	// the normal state after deleting a file outside this program, and a
	// symptom worth showing rather than hiding.
	Missing bool `json:"missing,omitempty"`
	// Fit is the best estimate across every machine that could hold these
	// weights, attached at query time because free VRAM moves.
	Fit *models.Fit `json:"fit,omitempty"`
	// HostFits is that estimate per machine. Which box has room for a file
	// already on the drive is the same question the Hub half of the screen
	// answers about one it has not fetched yet, and it is answered the same
	// way: one line per machine, named. Carried only when there is a choice.
	HostFits []models.Fit `json:"host_fits,omitempty"`

	AddedAt string `json:"added_at,omitempty"`
}

// Status reports what the UI needs to draw the repository header.
func (r *Repo) Status(ctx context.Context) Status {
	st := Status{Configured: r.Configured(), Root: r.root}
	if !st.Configured {
		st.Detail = ErrNotConfigured.Error()
		return st
	}
	root, err := r.resolve()
	if err != nil {
		st.Detail = err.Error()
		return st
	}
	st.Available = true
	st.Root = root
	if _, err := os.Stat(filepath.Join(root, markerName)); err == nil {
		st.Initialised = true
	} else {
		st.Detail = fmt.Sprintf("%s carries no %s marker. If this is the right drive, "+
			"initialise it; if it is an empty mount point, mount the drive first.", root, markerName)
	}
	if total, free, err := diskSpace(root); err == nil {
		st.TotalBytes, st.FreeBytes = total, free
	}
	if entries, err := r.List(ctx, nil); err == nil {
		for _, e := range entries {
			if !e.Missing {
				st.UsedByRepoBytes += e.SizeBytes
				st.FileCount++
			}
		}
	}
	return st
}

// List walks the repository and merges what is on disk with what is recorded.
//
// hosts is optional; when given, each entry is graded for fit against every
// machine that could run it and keeps the best verdict. The walk is the
// authority on existence and size — a file the index has never heard of is
// listed all the same, because the operator putting a GGUF on the drive by hand
// is a perfectly ordinary way to add one.
func (r *Repo) List(ctx context.Context, hosts []*models.Hardware) ([]Entry, error) {
	root, err := r.resolve()
	if err != nil {
		return nil, err
	}
	recorded, err := r.store.RepoFiles(ctx)
	if err != nil {
		return nil, err
	}
	tags, err := r.store.Tags(ctx)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	out := []Entry{}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// One unreadable subdirectory should not lose the whole listing.
			r.log.Debug("model repository walk", "path", path, "error", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), weightSuffix) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		key := store.RepoKey(filepath.ToSlash(rel))
		seen[key] = true

		info, statErr := d.Info()
		var size int64
		if statErr == nil {
			size = info.Size()
		}
		out = append(out, r.entry(key, d.Name(), size, recorded[key], tags[key], hosts))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk model repository: %w", walkErr)
	}

	// Rows whose file is gone. Reported, not deleted: a drive mounted at the
	// wrong path, or a file moved aside during a clean-up, should look like a
	// problem to look at rather than provenance quietly discarded.
	for key, rec := range recorded {
		if seen[key] {
			continue
		}
		e := r.entry(key, filepath.Base(key), rec.SizeBytes, rec, tags[key], nil)
		e.Missing = true
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

// entry assembles one listing row from the disk, the index and the tags.
func (r *Repo) entry(key, name string, size int64, rec store.RepoFile, tags []string,
	hosts []*models.Hardware) Entry {

	e := Entry{
		RelPath:   key,
		Name:      name,
		SizeBytes: size,
		Quant:     rec.Quant,
		ParamsB:   rec.ParamsB,
		HubID:     rec.HubID,
		SourceURL: rec.SourceURL,
		SHA256:    rec.SHA256,
		Verified:  rec.SHA256 != "",
		Tags:      tags,
	}
	if !rec.AddedAt.IsZero() {
		e.AddedAt = rec.AddedAt.UTC().Format("2006-01-02")
	}
	// Nothing recorded means a file somebody copied in, so fall back to the
	// filename heuristics and mark the result as the guess it is.
	if e.ParamsB == 0 || e.Quant == "" {
		params, quant := models.Describe(key)
		if e.ParamsB == 0 && params > 0 {
			e.ParamsB, e.Estimated = params, true
		}
		if e.Quant == "" && quant != "" {
			e.Quant, e.Estimated = quant, true
		}
	}
	if len(hosts) > 0 && e.ParamsB > 0 {
		graded := models.EstimateFleetFit(models.FitInput{
			ParamsB: e.ParamsB,
			Quant:   e.Quant,
		}, hosts)
		best := models.BestFit(graded)
		e.Fit = &best
		if len(graded) > 1 {
			e.HostFits = graded
		}
	}
	return e
}

// Open resolves one repository file for reading, confined to the root.
//
// Used to serve weights to fleet nodes, which is the one path where a path from
// off this machine reaches the repository — so it goes through the same
// safeJoin as everything else, and refuses anything that is not weights. A
// missing marker is deliberately *not* required here: reading is not writing,
// and refusing to serve a file that is plainly present because the marker was
// deleted would be pedantry at the expense of a node waiting on a download.
func (r *Repo) Open(relPath string) (path string, info os.FileInfo, err error) {
	root, err := r.resolve()
	if err != nil {
		return "", nil, err
	}
	abs, err := r.safeJoin(root, relPath)
	if err != nil {
		return "", nil, err
	}
	if !strings.HasSuffix(strings.ToLower(abs), weightSuffix) {
		return "", nil, fmt.Errorf("%w: only %s files are served from the repository",
			ErrInvalidRequest, weightSuffix)
	}
	info, err = os.Stat(abs)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %s is not in the repository", ErrInvalidRequest, relPath)
	}
	if info.IsDir() {
		return "", nil, fmt.Errorf("%w: %s is a directory", ErrInvalidRequest, relPath)
	}
	return abs, info, nil
}

// Delete removes one file from the repository and forgets what was recorded
// about it.
//
// The file goes first. If the unlink fails there is nothing to forget, and a
// row deleted before a file that turns out to be undeletable would leave the
// repository holding weights it can no longer say anything about.
func (r *Repo) Delete(ctx context.Context, relPath string) error {
	root, err := r.Ready()
	if err != nil {
		return err
	}
	abs, err := r.safeJoin(root, relPath)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.ToLower(abs), weightSuffix) &&
		!strings.HasSuffix(strings.ToLower(abs), partSuffix) {
		return fmt.Errorf("refusing to delete %q: only %s and %s files are the repository's to remove",
			relPath, weightSuffix, partSuffix)
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete %q: %w", relPath, err)
	}
	// Prune the directory if this was the last thing in it, so a repository
	// browsed by hand does not silt up with empty author folders. Failure is
	// ignored: a non-empty directory is the ordinary case.
	if dir := filepath.Dir(abs); within(dir, root) && dir != root {
		_ = os.Remove(dir)
	}
	return r.store.ForgetRepoFile(ctx, relPath)
}

// diskSpace reports the capacity and free bytes of the filesystem holding path.
//
// Free rather than available-to-root: what matters is whether the next download
// fits, and the reserved blocks are not ours to spend.
func diskSpace(path string) (total, free int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	return int64(stat.Blocks) * int64(stat.Bsize), int64(stat.Bavail) * int64(stat.Bsize), nil
}
