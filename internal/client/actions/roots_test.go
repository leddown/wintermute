package actions

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewRootsRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		paths []string
	}{
		{"empty", nil},
		{"missing directory", []string{filepath.Join(dir, "nope")}},
		{"file not directory", []string{file}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewRoots(tt.paths); err == nil {
				t.Fatalf("NewRoots(%v) = nil error, want error", tt.paths)
			}
		})
	}
}

func TestRootsResolve(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "Season 01")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(sub, "ep.mkv")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	roots, err := NewRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("allows paths inside a root", func(t *testing.T) {
		for _, p := range []string{root, sub, inside} {
			if _, err := roots.Resolve(p); err != nil {
				t.Errorf("Resolve(%q) = %v, want nil", p, err)
			}
		}
	})

	t.Run("rejects paths outside every root", func(t *testing.T) {
		cases := []string{
			outsideFile,
			filepath.Join(root, "..", filepath.Base(outside), "secret.txt"),
			filepath.Join(root, "..", ".."),
		}
		for _, p := range cases {
			if _, err := roots.Resolve(p); !errors.Is(err, ErrOutsideRoots) {
				t.Errorf("Resolve(%q) = %v, want ErrOutsideRoots", p, err)
			}
		}
	})

	t.Run("rejects an empty path", func(t *testing.T) {
		if _, err := roots.Resolve("  "); err == nil {
			t.Error("Resolve(blank) = nil error, want error")
		}
	})

	// A sibling directory whose name merely begins with the root's name must
	// not be treated as inside it.
	t.Run("rejects a prefix-sharing sibling", func(t *testing.T) {
		sibling := root + "-other"
		if err := os.MkdirAll(sibling, 0o755); err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(sibling)

		if _, err := roots.Resolve(sibling); !errors.Is(err, ErrOutsideRoots) {
			t.Errorf("Resolve(%q) = %v, want ErrOutsideRoots", sibling, err)
		}
	})
}

// A symlink inside a root pointing out of it is the classic escape; Resolve
// follows links before the containment check to close it.
func TestRootsResolveFollowsSymlinksOutOfRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}

	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	roots, err := NewRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roots.Resolve(link); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("Resolve(symlink escaping root) = %v, want ErrOutsideRoots", err)
	}
}

func TestRootsResolveNew(t *testing.T) {
	root := t.TempDir()
	roots, err := NewRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	// A destination that does not exist yet is valid when its parent is in a root.
	got, err := roots.ResolveNew(filepath.Join(root, "New Name.mkv"))
	if err != nil {
		t.Fatalf("ResolveNew inside root = %v, want nil", err)
	}
	if filepath.Base(got) != "New Name.mkv" {
		t.Errorf("ResolveNew base = %q, want %q", filepath.Base(got), "New Name.mkv")
	}

	if _, err := roots.ResolveNew(filepath.Join(t.TempDir(), "x.mkv")); !errors.Is(err, ErrOutsideRoots) {
		t.Errorf("ResolveNew outside root = %v, want ErrOutsideRoots", err)
	}
}
