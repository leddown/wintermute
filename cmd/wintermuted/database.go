package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"wintermute/internal/config"
	"wintermute/internal/store"
)

// Finding the database a management command should act on.
//
// This used to be one line — WINTERMUTE_DB from the environment, falling back
// to a *relative* "wintermute.db" — and that line was a trap. The env file the
// service uses is a systemd EnvironmentFile, which an interactive shell never
// sees, so `wintermuted -add-client` run by hand from a checkout created a
// second database in the current directory and registered the client there.
// The token was real. It was just in a database the server never opens, so the
// browser answered "invalid token" and nothing explained why.
//
// scripts/clients.sh exists because of that, resolving the path out of the env
// file before calling this binary. That is the right thing for a script to do
// and the wrong thing for it to have to do: the trap belongs closed here, where
// every caller gets it, including the operator typing the flag directly.

// serviceEnvFile is where an installed server keeps its configuration. Read
// rather than parsed for meaning: the one setting wanted from it is the path.
// A var rather than a const so the tests can point it somewhere writable.
var serviceEnvFile = "/etc/wintermute/wintermute.env"

// resolveDatabase reports the database to open and where that came from.
//
// The service's env file is consulted before a checkout's .env because the
// point of these commands is to act on what the *service* reads. An explicit
// WINTERMUTE_DB still wins over both — someone naming a file has said which one
// they mean.
func resolveDatabase() (path, source string) {
	return resolveSetting("WINTERMUTE_DB", "wintermute.db")
}

// resolveSetting reports one setting's value and where it came from, by the
// rule above: the environment first, then the files the service reads, in the
// order the service reads them. An empty fallback means "nothing configured",
// and the caller says what that costs.
func resolveSetting(key, fallback string) (value, source string) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v, key + " in the environment"
	}
	// Set but empty counts as set to LookupEnv, which is what LoadEnvFile
	// checks before it will write a value. Leaving it in place would let an
	// exported KEY= defeat every file below while looking like nothing had
	// been set at all.
	os.Unsetenv(key)

	for _, f := range []string{serviceEnvFile, ".env"} {
		// A file that cannot be read is not an error here: on a developer's
		// machine there is no /etc/wintermute, and on a server there is often
		// no checkout.
		if err := config.LoadEnvFile(f); err != nil {
			continue
		}
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v, f
		}
	}
	return fallback, "the built-in default"
}

// openManaged opens the database for a command that only touches storage.
//
// mustExist separates the two kinds of caller. -migrate-only is how a database
// is created in the first place, so it may. Everything else may not: a client
// command that creates its own database issues a token nothing will ever
// accept, which is the failure this whole file is about.
func openManaged(mustExist bool) (*store.Store, string, error) {
	path, source := resolveDatabase()
	fmt.Fprintf(os.Stderr, "database: %s (from %s)\n", path, source)

	if mustExist {
		if _, err := os.Stat(path); err != nil {
			return nil, "", fmt.Errorf(
				"no database at %s (from %s)\n"+
					"       Refusing to create one: a fresh database would take the token and then\n"+
					"       reject every use of it. Check the path, or run scripts/setup.sh.", path, source)
		}
	}

	st, err := store.Open(path)
	if err != nil {
		return nil, "", err
	}
	return st, path, nil
}

// handBackDatabase returns the database and its write-ahead sidecars to the
// account that owns them.
//
// SQLite writes <db>-wal and <db>-shm beside the file, creating them as
// whoever is running. A management command run under sudo therefore leaves
// root-owned sidecars behind, and the service — running as its own user — then
// fails on its first write with a permission error that points nowhere near
// the command that caused it. The symptom arrives minutes later, in another
// process, as a database that has quietly become read-only.
//
// scripts/clients.sh avoided this by running the binary as the service user.
// Doing it here too is what makes the flag safe to type directly, which is the
// point of the exercise.
//
// Best effort throughout: this is repair, and failing to repair must not turn a
// command that already succeeded into one that reports failure.
func handBackDatabase(path string) {
	if os.Geteuid() != 0 {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) == os.Geteuid() {
		// Already root-owned: a database root created, with nobody to hand it
		// back to.
		return
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		_ = os.Chown(p, int(sys.Uid), int(sys.Gid))
	}
}
