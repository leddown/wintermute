package actions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wintermute/internal/tool"
)

// newTestSet builds an action set over a temporary root containing the given
// files, and returns the set plus the root path.
func newTestSet(t *testing.T, files ...string) (*Set, string) {
	t.Helper()

	root := t.TempDir()
	for _, name := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	roots, err := NewRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	return New(roots), root
}

func call(t *testing.T, name string, input map[string]any) tool.Call {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Call{ID: "call_1", Name: name, Input: raw}
}

func TestRenameFile(t *testing.T) {
	t.Run("renames a file in place", func(t *testing.T) {
		set, root := newTestSet(t, "show.s01e01.1080p.WEB.mkv")

		res := set.Execute(context.Background(), call(t, "rename_file", map[string]any{
			"path":     filepath.Join(root, "show.s01e01.1080p.WEB.mkv"),
			"new_name": "Show - S01E01 - Pilot.mkv",
		}))
		if res.IsError {
			t.Fatalf("rename failed: %s", res.Content)
		}
		if _, err := os.Stat(filepath.Join(root, "Show - S01E01 - Pilot.mkv")); err != nil {
			t.Fatalf("renamed file missing: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "show.s01e01.1080p.WEB.mkv")); !os.IsNotExist(err) {
			t.Error("original filename still present after rename")
		}
	})

	// os.Rename would silently replace the destination on Unix, so a batch of
	// renames that collide would destroy files. The check must be explicit.
	t.Run("refuses to overwrite an existing file", func(t *testing.T) {
		set, root := newTestSet(t, "a.mkv", "b.mkv")

		res := set.Execute(context.Background(), call(t, "rename_file", map[string]any{
			"path":     filepath.Join(root, "a.mkv"),
			"new_name": "b.mkv",
		}))
		if !res.IsError {
			t.Fatal("rename onto an existing file succeeded, want error")
		}
		if !strings.Contains(res.Content, "refusing to overwrite") {
			t.Errorf("unexpected error: %s", res.Content)
		}
		// Both files must survive.
		for _, name := range []string{"a.mkv", "b.mkv"} {
			if _, err := os.Stat(filepath.Join(root, name)); err != nil {
				t.Errorf("%s missing after refused rename: %v", name, err)
			}
		}
	})

	t.Run("rejects a path in new_name", func(t *testing.T) {
		set, root := newTestSet(t, "a.mkv")

		for _, bad := range []string{"../a.mkv", "sub/a.mkv", `sub\a.mkv`, "", ".", "a:b.mkv"} {
			res := set.Execute(context.Background(), call(t, "rename_file", map[string]any{
				"path":     filepath.Join(root, "a.mkv"),
				"new_name": bad,
			}))
			if !res.IsError {
				t.Errorf("new_name %q was accepted, want rejection", bad)
			}
		}
	})

	t.Run("rejects a source outside the roots", func(t *testing.T) {
		set, _ := newTestSet(t, "a.mkv")
		outside := filepath.Join(t.TempDir(), "elsewhere.mkv")
		if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		res := set.Execute(context.Background(), call(t, "rename_file", map[string]any{
			"path":     outside,
			"new_name": "renamed.mkv",
		}))
		if !res.IsError {
			t.Fatal("rename outside roots succeeded, want error")
		}
	})

	t.Run("rejects a directory", func(t *testing.T) {
		set, root := newTestSet(t, "Season 01/a.mkv")

		res := set.Execute(context.Background(), call(t, "rename_file", map[string]any{
			"path":     filepath.Join(root, "Season 01"),
			"new_name": "Season 1",
		}))
		if !res.IsError {
			t.Fatal("renaming a directory succeeded, want error")
		}
	})

	t.Run("renaming to the same name is a no-op, not an error", func(t *testing.T) {
		set, root := newTestSet(t, "a.mkv")

		res := set.Execute(context.Background(), call(t, "rename_file", map[string]any{
			"path":     filepath.Join(root, "a.mkv"),
			"new_name": "a.mkv",
		}))
		if res.IsError {
			t.Fatalf("same-name rename reported an error: %s", res.Content)
		}
	})
}

func TestListDirectory(t *testing.T) {
	set, root := newTestSet(t, "b.mkv", "a.mkv", "Season 01/deep.mkv")

	res := set.Execute(context.Background(), call(t, "list_directory", map[string]any{"path": root}))
	if res.IsError {
		t.Fatalf("list failed: %s", res.Content)
	}
	// Directories sort before files, and files sort alphabetically.
	wantOrder := []string{"Season 01", "a.mkv", "b.mkv"}
	last := -1
	for _, want := range wantOrder {
		at := strings.Index(res.Content, want)
		if at < 0 {
			t.Fatalf("listing missing %q:\n%s", want, res.Content)
		}
		if at < last {
			t.Errorf("listing out of order around %q:\n%s", want, res.Content)
		}
		last = at
	}
	// A non-recursive listing must not descend.
	if strings.Contains(res.Content, "deep.mkv") {
		t.Errorf("non-recursive listing included a nested file:\n%s", res.Content)
	}

	t.Run("recursive listing descends", func(t *testing.T) {
		res := set.Execute(context.Background(), call(t, "list_directory", map[string]any{
			"path":                   root,
			"include_subdirectories": true,
		}))
		if res.IsError {
			t.Fatalf("recursive list failed: %s", res.Content)
		}
		if !strings.Contains(res.Content, "deep.mkv") {
			t.Errorf("recursive listing missing nested file:\n%s", res.Content)
		}
	})
}

func TestStatPath(t *testing.T) {
	set, root := newTestSet(t, "a.mkv")

	res := set.Execute(context.Background(), call(t, "stat_path", map[string]any{
		"path": filepath.Join(root, "a.mkv"),
	}))
	if res.IsError || !strings.Contains(res.Content, "exists") {
		t.Fatalf("stat of an existing file = %q (error=%v)", res.Content, res.IsError)
	}

	// A missing path is an answer, not an error — the model uses this to check
	// whether a proposed name is free.
	res = set.Execute(context.Background(), call(t, "stat_path", map[string]any{
		"path": filepath.Join(root, "nothing-here.mkv"),
	}))
	if res.IsError {
		t.Fatalf("stat of a missing file reported an error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "does not exist") {
		t.Errorf("stat of a missing file = %q", res.Content)
	}
}

func TestUnknownToolIsReportedNotPanicked(t *testing.T) {
	set, _ := newTestSet(t)

	res := set.Execute(context.Background(), tool.Call{ID: "x", Name: "delete_everything"})
	if !res.IsError {
		t.Fatal("unknown tool returned success")
	}
}

func TestDefinitionsAreClientSideAndSorted(t *testing.T) {
	set, _ := newTestSet(t)

	defs := set.Definitions()
	if len(defs) == 0 {
		t.Fatal("no definitions")
	}
	for i, d := range defs {
		if d.Side != tool.SideClient {
			t.Errorf("%s: side = %q, want %q", d.Name, d.Side, tool.SideClient)
		}
		if !d.Risk.Valid() {
			t.Errorf("%s: invalid risk %q", d.Name, d.Risk)
		}
		if i > 0 && defs[i-1].Name > d.Name {
			t.Errorf("definitions not sorted: %q before %q", defs[i-1].Name, d.Name)
		}
	}
}
