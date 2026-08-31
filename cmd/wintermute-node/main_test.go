package main

import (
	"path/filepath"
	"testing"
)

// The spool is what makes an outage survivable across a restart, and under the
// shipped unit the cache-directory default was unreachable: the service user is
// created with --no-create-home and ProtectHome=true masks its home anyway, so
// every spool write failed silently and the backlog was lost on restart after
// all. STATE_DIRECTORY is the directory systemd has already created for this
// unit, owned by the service user.
func TestDefaultSpoolPrefersStateDirectory(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "/var/lib/wintermute-node")
	if got, want := defaultSpool(), "/var/lib/wintermute-node/spool.json"; got != want {
		t.Errorf("defaultSpool() = %q, want %q", got, want)
	}

	// A unit naming several state directories gets them colon-separated. Ours
	// is the first.
	t.Setenv("STATE_DIRECTORY", "/var/lib/wintermute-node:/var/lib/other")
	if got, want := defaultSpool(), "/var/lib/wintermute-node/spool.json"; got != want {
		t.Errorf("with a list, defaultSpool() = %q, want %q", got, want)
	}
}

// Run from a shell rather than by systemd there is no state directory, and the
// cache directory is both writable and the right place.
func TestDefaultSpoolFallsBackToCacheDir(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "/home/someone")

	want := filepath.Join("/home/someone", ".cache", "wintermute-node", "spool.json")
	if got := defaultSpool(); got != want {
		t.Errorf("defaultSpool() = %q, want %q", got, want)
	}
}
