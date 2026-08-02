package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"wintermute/internal/tool"
)

// defaultMaxEntries bounds a listing so a large media directory cannot blow
// past the model's context window in a single tool result.
const defaultMaxEntries = 200

func listDirectory(roots *Roots) Action {
	const schema = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Directory to list. Must be inside an allowed root; UNC paths such as \\\\NAS\\Media are supported on Windows."
    },
    "include_subdirectories": {
      "type": "boolean",
      "description": "Recurse into subdirectories. Defaults to false."
    },
    "max_entries": {
      "type": "integer",
      "description": "Maximum entries to return. Defaults to 200."
    }
  },
  "required": ["path"]
}`

	return Action{
		Definition: tool.Definition{
			Name:        "list_directory",
			Description: "List the files and folders in a directory on this machine, including mapped drives and network shares. Read-only.",
			Parameters:  json.RawMessage(schema),
			Risk:        tool.RiskRead,
		},
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var in struct {
				Path       string `json:"path"`
				Recursive  bool   `json:"include_subdirectories"`
				MaxEntries int    `json:"max_entries"`
			}
			if err := decodeInput("list_directory", raw, &in); err != nil {
				return "", err
			}
			dir, err := roots.Resolve(in.Path)
			if err != nil {
				return "", err
			}
			maxEntries := in.MaxEntries
			if maxEntries <= 0 || maxEntries > 1000 {
				maxEntries = defaultMaxEntries
			}

			entries, truncated, err := walk(ctx, dir, in.Recursive, maxEntries)
			if err != nil {
				return "", err
			}

			var b strings.Builder
			fmt.Fprintf(&b, "%s — %d entries", dir, len(entries))
			if truncated {
				fmt.Fprintf(&b, " (truncated at %d; narrow the path or raise max_entries)", maxEntries)
			}
			b.WriteString("\n")
			for _, e := range entries {
				if e.isDir {
					fmt.Fprintf(&b, "[dir]  %s\n", e.name)
					continue
				}
				fmt.Fprintf(&b, "[file] %s  (%s)\n", e.name, humanSize(e.size))
			}
			if len(entries) == 0 {
				b.WriteString("(empty)\n")
			}
			return b.String(), nil
		},
	}
}

type entry struct {
	name  string
	isDir bool
	size  int64
}

// walk lists dir, optionally recursively, stopping once max entries are
// collected. Names are returned relative to dir so the model sees short,
// comparable filenames rather than repeated absolute prefixes.
func walk(ctx context.Context, dir string, recursive bool, max int) ([]entry, bool, error) {
	var out []entry

	if !recursive {
		items, err := os.ReadDir(dir)
		if err != nil {
			return nil, false, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, it := range items {
			if len(out) >= max {
				return out, true, nil
			}
			out = append(out, toEntry(it.Name(), it))
		}
		sortEntries(out)
		return out, false, nil
	}

	truncated := false
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory shouldn't abort the whole listing;
			// on a NAS, permission gaps are routine.
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if path == dir {
			return nil
		}
		if len(out) >= max {
			truncated = true
			return filepath.SkipAll
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}
		out = append(out, toEntry(rel, d))
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("walk %s: %w", dir, err)
	}
	sortEntries(out)
	return out, truncated, nil
}

func toEntry(name string, d os.DirEntry) entry {
	e := entry{name: name, isDir: d.IsDir()}
	if info, err := d.Info(); err == nil {
		e.size = info.Size()
	}
	return e
}

// sortEntries puts directories first, then files, each alphabetically — the
// order a person would scan, which also groups season folders together.
func sortEntries(entries []entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].isDir != entries[j].isDir {
			return entries[i].isDir
		}
		return strings.ToLower(entries[i].name) < strings.ToLower(entries[j].name)
	})
}

func statPath(roots *Roots) Action {
	const schema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File or directory to inspect."}
  },
  "required": ["path"]
}`

	return Action{
		Definition: tool.Definition{
			Name:        "stat_path",
			Description: "Report whether a path exists on this machine and, if so, its size and last-modified time. Read-only. Use this to check whether a proposed new filename is already taken.",
			Parameters:  json.RawMessage(schema),
			Risk:        tool.RiskRead,
		},
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var in struct {
				Path string `json:"path"`
			}
			if err := decodeInput("stat_path", raw, &in); err != nil {
				return "", err
			}
			// A path that doesn't exist still has to be inside a root, so
			// resolve through the parent rather than requiring existence.
			abs, err := roots.ResolveNew(in.Path)
			if err != nil {
				return "", err
			}

			info, err := os.Stat(abs)
			if os.IsNotExist(err) {
				return fmt.Sprintf("%s does not exist.", abs), nil
			}
			if err != nil {
				return "", fmt.Errorf("stat %s: %w", abs, err)
			}
			kind := "file"
			if info.IsDir() {
				kind = "directory"
			}
			return fmt.Sprintf("%s exists: %s, %s, modified %s",
				abs, kind, humanSize(info.Size()), info.ModTime().Format("2006-01-02 15:04")), nil
		},
	}
}

func renameFile(roots *Roots) Action {
	const schema = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Full path of the existing file to rename."
    },
    "new_name": {
      "type": "string",
      "description": "The new base name, including the extension. This is a filename only — it must not contain a path separator. The file stays in its current directory."
    },
    "reason": {
      "type": "string",
      "description": "One short line explaining why this name is correct, shown to the user when they approve the change."
    }
  },
  "required": ["path", "new_name"]
}`

	return Action{
		Definition: tool.Definition{
			Name:        "rename_file",
			Description: "Rename a single file in place on this machine. Requires the user's approval. The file is not moved between directories.",
			Parameters:  json.RawMessage(schema),
			Risk:        tool.RiskWrite,
		},
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var in struct {
				Path    string `json:"path"`
				NewName string `json:"new_name"`
				Reason  string `json:"reason"`
			}
			if err := decodeInput("rename_file", raw, &in); err != nil {
				return "", err
			}
			if err := validateBaseName(in.NewName); err != nil {
				return "", err
			}

			src, err := roots.Resolve(in.Path)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(src)
			if err != nil {
				return "", fmt.Errorf("stat %s: %w", src, err)
			}
			if info.IsDir() {
				return "", fmt.Errorf("%s is a directory; rename_file only renames files", src)
			}

			dst := filepath.Join(filepath.Dir(src), in.NewName)
			if pathEqual(src, dst) {
				return fmt.Sprintf("%s already has that name; nothing to do.", src), nil
			}
			// os.Rename silently replaces an existing destination on Unix, so
			// the collision has to be checked explicitly. This is the check
			// that stops a bad batch from destroying files.
			if _, err := os.Stat(dst); err == nil {
				return "", fmt.Errorf("refusing to overwrite existing file %s", dst)
			} else if !os.IsNotExist(err) {
				return "", fmt.Errorf("stat %s: %w", dst, err)
			}

			if err := os.Rename(src, dst); err != nil {
				return "", fmt.Errorf("rename %s: %w", filepath.Base(src), err)
			}
			return fmt.Sprintf("Renamed %q to %q in %s.",
				filepath.Base(src), in.NewName, filepath.Dir(src)), nil
		},
	}
}

// validateBaseName rejects anything that isn't a plain filename. A model that
// puts a path in new_name would otherwise move the file somewhere unexpected.
func validateBaseName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("new_name is required")
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("new_name %q has leading or trailing whitespace", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("new_name %q must be a filename, not a path", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("new_name %q is not a valid filename", name)
	}
	// Reserved on Windows, and a source of surprising behaviour elsewhere.
	if strings.ContainsAny(name, `:*?"<>|`) {
		return fmt.Errorf("new_name %q contains characters that are not allowed in a filename", name)
	}
	return nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
