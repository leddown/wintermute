package actions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Roots confines every local action to a set of operator-approved directories.
//
// This is the hard boundary, checked before any approval prompt: the model
// cannot reach outside these paths even if a human approves the call, because
// the path is rejected before it is ever offered. Approval controls *whether* a
// permitted action runs; roots control *what is permitted at all*.
type Roots struct {
	paths []string
}

// ErrOutsideRoots is returned for a path outside every configured root.
var ErrOutsideRoots = errors.New("path is outside the allowed roots")

// NewRoots resolves and validates the configured roots. A root that does not
// exist is an error at startup rather than a confusing failure mid-turn.
func NewRoots(paths []string) (*Roots, error) {
	if len(paths) == 0 {
		return nil, errors.New("no roots configured: set \"roots\" in the client config")
	}
	resolved := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("root %q: %w", p, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("root %q: %w", p, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("root %q is not a directory", p)
		}
		// Resolve symlinks so a root given through a link and a path given
		// directly compare equal. UNC paths on Windows have nothing to
		// resolve, and EvalSymlinks can fail on some network filesystems, so
		// a failure falls back to the absolute path rather than aborting.
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		}
		resolved = append(resolved, filepath.Clean(abs))
	}
	return &Roots{paths: resolved}, nil
}

// List returns the configured roots.
func (r *Roots) List() []string {
	out := make([]string, len(r.paths))
	copy(out, r.paths)
	return out
}

// Resolve cleans p, makes it absolute, and verifies it lies within a root. It
// is used for paths that must already exist.
func (r *Roots) Resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	abs = filepath.Clean(abs)

	// Follow symlinks before the containment check, so a link inside a root
	// cannot be used to reach a file outside one. A path that does not exist
	// yet is checked via its parent by ResolveNew.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if !r.contains(abs) {
		return "", fmt.Errorf("%q: %w (allowed: %s)", p, ErrOutsideRoots, strings.Join(r.paths, ", "))
	}
	return abs, nil
}

// ResolveNew validates a path that does not exist yet — a rename destination —
// by checking the directory that will contain it.
func (r *Roots) ResolveNew(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	abs = filepath.Clean(abs)
	if _, err := r.Resolve(filepath.Dir(abs)); err != nil {
		return "", err
	}
	return abs, nil
}

// contains reports whether abs is at or below one of the roots.
func (r *Roots) contains(abs string) bool {
	for _, root := range r.paths {
		if pathEqual(abs, root) {
			return true
		}
		prefix := root
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if pathHasPrefix(abs, prefix) {
			return true
		}
	}
	return false
}

// caseInsensitiveFS reports whether path comparison should ignore case.
// Windows — the platform this client primarily targets, and the one where SMB
// shares live — compares paths case-insensitively.
var caseInsensitiveFS = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

func pathEqual(a, b string) bool {
	if caseInsensitiveFS {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func pathHasPrefix(s, prefix string) bool {
	if caseInsensitiveFS {
		return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
	}
	return strings.HasPrefix(s, prefix)
}
