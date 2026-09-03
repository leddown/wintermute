package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"wintermute/internal/modelrepo"
)

// Blessing the model repository without a running server.
//
// The marker file that proves a directory is the repository was, for a while,
// creatable in exactly one place: Admin → Repository, in the UI the server
// serves. That made a circle out of two reasonable rules. update.sh refuses to
// deploy when the marker is missing, because a repository held by
// RequiresMountsFor= turns the closing `systemctl restart` into a passphrase
// prompt nobody can answer; and the marker could only be created by the server
// the deploy was meant to restore. Reformat the drive with the service down
// and there was no way in from either end.
//
// So the same action lives here too. It is still deliberate — a flag typed on
// purpose, naming the directory it blessed — but it no longer depends on the
// thing it is often needed to repair.

// initRepo writes the marker file into the configured repository directory.
//
// Creating the directory is part of the job: a reformatted drive comes back
// empty, and `Repo.Initialise` deliberately will not create anything, since
// from inside the server a missing directory is indistinguishable from a drive
// that is not there.
func initRepo(log *slog.Logger) error {
	path, source := resolveModelRepo()
	if path == "" {
		return fmt.Errorf("no model repository configured: WINTERMUTE_MODEL_REPO is unset in the "+
			"environment, in %s and in ./.env", serviceEnvFile)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("WINTERMUTE_MODEL_REPO must be an absolute path, but %s gives %q", source, path)
	}
	fmt.Fprintf(os.Stderr, "model repository: %s (from %s)\n", path, source)

	created := false
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check %s: %w", path, err)
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		created = true
	}

	// A warning rather than a refusal. Plenty of installations keep the
	// repository on the system disk on purpose, so this cannot be an error —
	// but the expensive mistake is blessing a bare mount point while the drive
	// is unplugged, and that case looks exactly like this one.
	if sameFilesystemAsRoot(path) {
		fmt.Fprintf(os.Stderr,
			"note: %s is on the same filesystem as /, so it is not a separate drive.\n"+
				"      If it was meant to be one, mount it, delete the marker, and run this again.\n", path)
	}

	// Only Initialise is called, and it touches no storage — it resolves the
	// path and writes one file — so the repository needs neither a database
	// nor a Hugging Face token here.
	if err := modelrepo.New(path, "", nil, log).Initialise(); err != nil {
		return err
	}
	handBackRepo(path, created)

	if created {
		fmt.Printf("created and initialised %s\n", path)
	} else {
		fmt.Printf("initialised %s\n", path)
	}
	return nil
}

// resolveModelRepo reports the repository directory and where that came from,
// by the same rules as resolveDatabase: this command exists to act on what the
// *service* reads, so its env file outranks a checkout's .env.
func resolveModelRepo() (path, source string) {
	return resolveSetting("WINTERMUTE_MODEL_REPO", "")
}

// handBackRepo gives the directory and its marker to the account the service
// runs as.
//
// The same failure as handBackDatabase, one directory along, and a nastier one
// to read: a root-owned marker passes every check the server makes — the drive
// is mounted, the repository is initialised, the UI is green — and then every
// download fails with a permission error, because the marker proves only that
// *something* could write here, not that the service can.
//
// Who the service runs as is read off the env file rather than guessed from a
// name, because that file is chowned to the service user by scripts/setup.sh
// and is the one artefact on disk that records the choice.
//
// Best effort: this is repair, and failing to repair must not turn a command
// that already succeeded into one that reports failure.
func handBackRepo(path string, created bool) {
	if os.Geteuid() != 0 {
		return
	}
	info, err := os.Stat(serviceEnvFile)
	if err != nil {
		return
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid == 0 {
		// A root-owned env file means a server running as root, or an
		// installation this command cannot speak for. Either way there is
		// nobody to hand anything to.
		return
	}

	// The marker was written by this process, so it is always ours to hand
	// over. The directory is only touched when this command made it, or when
	// it is root-owned and therefore already unusable by the service — an
	// operator who set the ownership themselves has said what they meant.
	targets := []string{filepath.Join(path, ".wintermute-repo")}
	if dir, err := os.Stat(path); err == nil {
		if ds, ok := dir.Sys().(*syscall.Stat_t); ok && (created || ds.Uid == 0) {
			targets = append(targets, path)
		}
	}
	for _, t := range targets {
		_ = os.Chown(t, int(sys.Uid), int(sys.Gid))
	}
	fmt.Fprintf(os.Stderr, "handed %s to uid %d, the owner of %s\n", path, sys.Uid, serviceEnvFile)
}

// sameFilesystemAsRoot reports whether path lives on the root filesystem.
func sameFilesystemAsRoot(path string) bool {
	here, err := os.Stat(path)
	if err != nil {
		return false
	}
	root, err := os.Stat("/")
	if err != nil {
		return false
	}
	hs, ok1 := here.Sys().(*syscall.Stat_t)
	rs, ok2 := root.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && hs.Dev == rs.Dev
}
