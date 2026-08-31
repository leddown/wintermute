// Package nodestore is a fleet node's local library of model weights.
//
// It is the node half of internal/modelrepo. The server keeps the repository;
// a node keeps whatever subset of it that node has been assigned, so that
// switching a model on that host is a local file read rather than a download.
//
// The shape follows one rule, which is the rule the whole fleet design rests
// on: **the node is never told what to do, only what it should have.** An
// assignment arriving from the server is a repository-relative name and a
// digest. Where the file lands, what is done with it afterwards, and whether it
// is fetched at all are decided here, from this host's own configuration. There
// is no field in an assignment that could name a local path or a command, which
// is what keeps a compromised server from being a shell on every node.
//
// The three jobs are kept apart deliberately:
//
//   - Scan reports what is on the disk, and is the only thing the server ever
//     learns about this store.
//   - Fetch brings a missing file over, resumably, and verifies it.
//   - Ingest makes the runtime able to serve it, which differs completely
//     between llama.cpp and Ollama.
package nodestore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"wintermute/internal/node"
)

// weightSuffix and partSuffix match internal/modelrepo, because a node's store
// and the server's repository hold the same kind of thing under the same names.
const (
	weightSuffix = ".gguf"
	partSuffix   = ".part"
)

// Runtime is what serves models on this host.
type Runtime string

const (
	// RuntimeLlamaCPP references a GGUF where it lies. No copy, no import: the
	// file in the store is the file that gets served.
	RuntimeLlamaCPP Runtime = "llamacpp"
	// RuntimeOllama has to import a GGUF into its own content-addressed blob
	// store before it can serve it, which means a second copy on disk. That is
	// a real cost of running Ollama rather than a defect here, and it is
	// reported rather than hidden — see Ingest.
	RuntimeOllama Runtime = "ollama"
	// RuntimeNone keeps the files and does nothing else with them. The honest
	// setting for a host whose runtime is wired up by hand.
	RuntimeNone Runtime = ""
)

// Valid reports whether r is a runtime this agent knows how to ingest for.
func (r Runtime) Valid() bool {
	switch r {
	case RuntimeLlamaCPP, RuntimeOllama, RuntimeNone:
		return true
	}
	return false
}

// Store is one node's model library.
type Store struct {
	root    string
	runtime Runtime
	// ingester makes a fetched file servable. Nil for RuntimeNone.
	ingester Ingester
}

// ErrNoStore reports that this agent has no model store configured, which is
// the ordinary state of a node that only reports metrics.
var ErrNoStore = errors.New("no model store is configured")

// New builds a Store rooted at dir.
//
// The directory is created if it is missing, unlike the server's repository,
// which refuses to. The reasoning differs because the risk does: the server's
// repository is an external drive whose absence must never be mistaken for an
// empty one, while a node's store is ordinary local disk named by the operator
// on the command line, and creating it is what makes deploying an agent one
// step instead of two.
func New(dir string, runtime Runtime, ingester Ingester) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, ErrNoStore
	}
	if !runtime.Valid() {
		return nil, fmt.Errorf("unknown runtime %q — expected llamacpp or ollama", runtime)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("model store %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("model store %q: %w", dir, err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	store := &Store{root: filepath.Clean(abs), runtime: runtime, ingester: ingester}

	// llama.cpp's config is generated in full from what this agent knows, and
	// what it knows starts empty on every start. Handing it the store's
	// existing contents now is what stops the first import after a restart
	// rewriting the config with one model in it. Nothing equivalent is needed
	// for Ollama, which keeps its own catalogue and is asked.
	if l, ok := ingester.(*LlamaCPPIngester); ok {
		if err := l.adopt(store); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// Root returns the store directory.
func (s *Store) Root() string { return s.root }

// Runtime returns what serves models on this host.
func (s *Store) Runtime() Runtime { return s.runtime }

// Path resolves a repository-relative name to a local file, confined to the
// store.
//
// This is the security boundary on the node, and it is the only place an
// assignment's text becomes a path. The name arrives from the server, so it is
// treated exactly as internal/modelrepo treats a filename from Hugging Face:
// cleaned, checked for traversal, and confined after resolution.
func (s *Store) Path(relPath string) (string, error) {
	rel := strings.Trim(strings.ReplaceAll(strings.TrimSpace(relPath), "\\", "/"), "/")
	if rel == "" {
		return "", errors.New("an assignment with no name")
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("%q: an assignment must be a repository-relative name", relPath)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || seg == "." {
			return "", fmt.Errorf("%q: an assignment may not contain a path traversal", relPath)
		}
	}
	abs := filepath.Clean(filepath.Join(s.root, filepath.FromSlash(rel)))
	if !within(abs, s.root) {
		return "", fmt.Errorf("%q resolves outside the model store", relPath)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil && !within(real, s.root) {
		return "", fmt.Errorf("%q is a link out of the model store", relPath)
	}
	return abs, nil
}

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

// Has reports whether a complete copy of the weights is already here.
//
// A .part file is deliberately not a hit. It is a download that was
// interrupted, and treating it as present would have the node report a model it
// cannot serve — and the server then load a model that is not there.
func (s *Store) Has(relPath string) bool {
	abs, err := s.Path(relPath)
	if err != nil {
		return false
	}
	info, err := os.Stat(abs)
	return err == nil && !info.IsDir()
}

// Scan walks the store and describes it, which is everything the server is ever
// told about this host's weights.
//
// A store that cannot be read is reported as an error rather than as an empty
// inventory: an unmounted disk that looked like "this node holds nothing" would
// have the server cheerfully re-download every model onto a host that already
// has them, or onto the root filesystem underneath the missing mount.
func (s *Store) Scan() node.StoreReport {
	report := node.StoreReport{Path: s.root, Runtime: string(s.runtime)}
	if s.ingester != nil {
		report.RuntimeURL = s.ingester.Endpoint()
	}

	if total, free, err := diskSpace(s.root); err == nil {
		report.TotalBytes, report.FreeBytes = total, free
	}

	// Which of these the runtime can actually serve. Read once for the whole
	// walk rather than per file: on Ollama it is an HTTP call.
	servable := map[string]bool{}
	if s.ingester != nil {
		if names, err := s.ingester.ServableNames(); err == nil {
			servable = names
		}
	}

	partials := map[string]bool{}
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		rel, relErr := filepath.Rel(s.root, path)
		if relErr != nil {
			return nil
		}
		key := filepath.ToSlash(rel)

		switch {
		case strings.HasSuffix(name, weightSuffix+partSuffix):
			partials[strings.TrimSuffix(key, partSuffix)] = true
		case strings.HasSuffix(name, weightSuffix):
			var size int64
			if info, err := d.Info(); err == nil {
				size = info.Size()
			}
			// ServeName is reported whether or not the runtime has it yet:
			// it is what this file *will* be called, which is what a server
			// waiting on an import needs in order to name it afterwards.
			report.Files = append(report.Files, node.StoreFile{
				RelPath: key, SizeBytes: size, Ingested: servable[ModelName(key)],
				ServeName: ModelName(key),
			})
		}
		return nil
	})
	if err != nil {
		report.Error = err.Error()
		return report
	}

	// A partial with no completed file beside it is a transfer in progress, and
	// is reported as such so the fleet view can show it without the server
	// having to poll anything.
	have := map[string]bool{}
	for _, f := range report.Files {
		have[f.RelPath] = true
	}
	for key := range partials {
		if !have[key] {
			report.Files = append(report.Files, node.StoreFile{RelPath: key, Partial: true})
		}
	}

	sort.Slice(report.Files, func(i, j int) bool {
		return report.Files[i].RelPath < report.Files[j].RelPath
	})
	return report
}

// Pending returns the assignments this store still has work outstanding for, in
// the order given: the ones it does not hold, and the ones it holds but the
// runtime cannot yet serve.
//
// The second half is why this is not simply "what is absent". Weights can be
// here without this agent having fetched them — an operator copied them in, or
// the store is a share the server already writes to — and the import is the
// step that makes them servable. Selecting on absence alone left those files
// assigned, present and never imported: Ollama never saw them, llama-swap was
// never told about them, and the deploy screen showed a transfer that could not
// finish because there was nothing left to transfer.
//
// A store with no ingester imports nothing and claims nothing is servable, so
// for that one holding the file is the whole of the work.
func (s *Store) Pending(assignments []node.Assignment) []node.Assignment {
	out := []node.Assignment{}

	// Asked once for the whole batch rather than per file: on Ollama it is an
	// HTTP call. A runtime that cannot be reached is treated as having nothing
	// outstanding rather than as serving nothing — an unreachable runtime is
	// exactly when an import cannot succeed, and Ollama's takes the digest of
	// every byte before it discovers that. The files are still reported present
	// and not ingested; when the runtime answers again, so does this.
	var servable map[string]bool
	if s.ingester != nil {
		names, err := s.ingester.ServableNames()
		if err != nil {
			servable = nil
		} else {
			servable = names
		}
	}

	for _, a := range assignments {
		switch {
		case !s.Has(a.RelPath):
			out = append(out, a)
		case s.ingester != nil && servable != nil && !servable[ModelName(a.RelPath)]:
			out = append(out, a)
		}
	}
	return out
}

// diskSpace reports capacity and free bytes of the filesystem holding path.
func diskSpace(path string) (total, free int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	return int64(stat.Blocks) * int64(stat.Bsize), int64(stat.Bavail) * int64(stat.Bsize), nil
}
