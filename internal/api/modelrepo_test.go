package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wintermute/internal/modelrepo"
	"wintermute/internal/store"
)

// repoServer wires a server with a repository rooted at a temporary directory,
// and returns a helper that makes authenticated calls.
func repoServer(t *testing.T) (*Server, string, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	srv, st := newTestServer(t)

	root := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv = srv.WithModelRepo(modelrepo.New(root, "", st, log))

	_, token, err := st.CreateClient(t.Context(), "laptop", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()

	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, r)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	return srv, root, call
}

// Without a repository configured the routes must not exist at all, rather than
// existing and failing — the same shape as an absent CRM or twire.
func TestRepoRoutesAbsentWhenUnconfigured(t *testing.T) {
	srv, st := newTestServer(t)
	_, token, err := st.CreateClient(t.Context(), "laptop", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// No repository attached at all: the route falls through to the web UI
	// handler rather than being served.
	if rec.Code == http.StatusOK && strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Fatalf("repository routes should not be registered without a repository")
	}
}

func TestRepoRequiresAuth(t *testing.T) {
	srv, _, _ := repoServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repo", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// The lifecycle the UI walks: an uninitialised directory reports itself, an
// initialise blesses it, and only then will anything write.
func TestRepoInitialiseThenList(t *testing.T) {
	_, root, call := repoServer(t)

	rec := call(http.MethodGet, "/api/v1/repo", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rec.Code, rec.Body.String())
	}
	var first struct {
		Status modelrepo.Status `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if !first.Status.Configured || !first.Status.Available {
		t.Fatalf("status = %+v; a present directory should be available", first.Status)
	}
	if first.Status.Initialised {
		t.Fatal("a directory with no marker must not report itself as initialised")
	}

	// A download must be refused until the drive is claimed.
	rec = call(http.MethodPost, "/api/v1/repo/download",
		`{"hub_id":"Qwen/Qwen3-8B-GGUF","filename":"model.gguf"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("download before init = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	if rec = call(http.MethodPost, "/api/v1/repo/init", ""); rec.Code != http.StatusOK {
		t.Fatalf("init status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".wintermute-repo")); err != nil {
		t.Fatalf("the marker should be on the drive: %v", err)
	}
}

// A traversal in the filename must be refused with a 400 that names the reason,
// not swallowed as an internal error.
func TestRepoDownloadRejectsTraversal(t *testing.T) {
	_, _, call := repoServer(t)
	if rec := call(http.MethodPost, "/api/v1/repo/init", ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}

	for _, body := range []string{
		`{"hub_id":"Qwen/Qwen3-8B-GGUF","filename":"../../escape.gguf"}`,
		`{"hub_id":"../../etc","filename":"passwd.gguf"}`,
		`{"hub_id":"Qwen/Qwen3-8B-GGUF","filename":"notes.txt"}`,
		`{"hub_id":"","filename":"model.gguf"}`,
	} {
		rec := call(http.MethodPost, "/api/v1/repo/download", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("download %s = %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
}

// Deleting weights is gated on a typed confirmation checked here, not in the
// browser — a check that lives only in the page is not a check.
func TestRepoDeleteNeedsConfirmation(t *testing.T) {
	_, root, call := repoServer(t)
	if rec := call(http.MethodPost, "/api/v1/repo/init", ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	dir := filepath.Join(root, "Qwen", "Qwen3-8B-GGUF")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(path, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	const rel = "Qwen/Qwen3-8B-GGUF/model.gguf"

	rec := call(http.MethodPost, "/api/v1/repo/delete", `{"rel_path":"`+rel+`","confirm":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed delete = %d, want 400", rec.Code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the file must survive an unconfirmed delete")
	}

	rec = call(http.MethodPost, "/api/v1/repo/delete", `{"rel_path":"`+rel+`","confirm":"delete"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed delete = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the file should be gone")
	}
}

// Tags round-trip through the API and come back normalised on the listing.
func TestRepoTags(t *testing.T) {
	_, root, call := repoServer(t)
	if rec := call(http.MethodPost, "/api/v1/repo/init", ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if err := os.WriteFile(filepath.Join(root, "qwen3-8b-Q4_K_M.gguf"), []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	const rel = "qwen3-8b-Q4_K_M.gguf"

	if rec := call(http.MethodPost, "/api/v1/repo/tags",
		`{"rel_path":"`+rel+`","tag":"Long Context"}`); rec.Code != http.StatusOK {
		t.Fatalf("add tag = %d: %s", rec.Code, rec.Body.String())
	}

	rec := call(http.MethodGet, "/api/v1/repo", "")
	var listing struct {
		Files []modelrepo.Entry `json:"files"`
		Tags  []string          `json:"tags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Files) != 1 {
		t.Fatalf("want one file, got %+v", listing.Files)
	}
	if got := listing.Files[0].Tags; len(got) != 1 || got[0] != "long-context" {
		t.Fatalf("tags = %v, want [long-context]", got)
	}
	if len(listing.Tags) != 1 || listing.Tags[0] != "long-context" {
		t.Errorf("vocabulary = %v", listing.Tags)
	}

	if rec := call(http.MethodPost, "/api/v1/repo/tags/remove",
		`{"rel_path":"`+rel+`","tag":"long-context"}`); rec.Code != http.StatusOK {
		t.Fatalf("remove tag = %d: %s", rec.Code, rec.Body.String())
	}
	// Decoded into a fresh value, not the one above. encoding/json reuses the
	// elements of an existing slice and leaves fields absent from the payload
	// untouched, and Tags is omitempty — so reusing `listing` would show the
	// tag still there after a removal that worked perfectly well.
	var after struct {
		Files []modelrepo.Entry `json:"files"`
	}
	rec = call(http.MethodGet, "/api/v1/repo", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Files[0].Tags) != 0 {
		t.Errorf("tags should be empty, got %v", after.Files[0].Tags)
	}
}

// Cancelling a job that does not exist is the operator's error, not the
// server's, and must not come back as a 500.
func TestRepoCancelUnknownJob(t *testing.T) {
	_, _, call := repoServer(t)
	if rec := call(http.MethodPost, "/api/v1/repo/jobs/nope/cancel", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
