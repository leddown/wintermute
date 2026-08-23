package modelrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wintermute/internal/store"
)

func newTestRepo(t *testing.T) (*Repo, string) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(root, "", st, slog.New(slog.DiscardHandler)), root
}

// The marker is the only thing standing between a download and the server's own
// root filesystem when the drive is not mounted, so nothing may write without
// it.
func TestReadyRequiresMarker(t *testing.T) {
	repo, root := newTestRepo(t)

	if _, err := repo.Ready(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a directory with no marker must not be writable, got %v", err)
	}
	// A listing still works: an uninitialised directory has to be
	// distinguishable from an absent one.
	if _, err := repo.List(context.Background(), nil); err != nil {
		t.Fatalf("listing an uninitialised repository should work: %v", err)
	}

	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Ready(); err != nil || got != root {
		t.Fatalf("Ready() after Initialise() = %q, %v; want %q", got, err, root)
	}
	// Initialising twice is not an error — the operator may press it again.
	if err := repo.Initialise(); err != nil {
		t.Fatalf("re-initialising should be a no-op: %v", err)
	}
}

func TestUnconfiguredAndMissingRoot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	none := New("", "", st, slog.New(slog.DiscardHandler))
	if none.Configured() {
		t.Fatal("an empty path must report itself as not configured")
	}
	if _, err := none.Ready(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}

	// A configured path that is not there is the unplugged-drive case, and it
	// must be reported rather than created.
	gone := filepath.Join(t.TempDir(), "not-mounted")
	repo := New(gone, "", st, slog.New(slog.DiscardHandler))
	if _, err := repo.Ready(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable for an absent root, got %v", err)
	}
	if _, err := os.Stat(gone); err == nil {
		t.Fatal("a missing repository root must not be created behind the operator's back")
	}
	if st := repo.Status(context.Background()); st.Available || !st.Configured {
		t.Fatalf("status for an absent root = %+v", st)
	}
}

// Filenames come from Hugging Face, so they are hostile input.
func TestSafeJoinConfinesPaths(t *testing.T) {
	repo, root := newTestRepo(t)

	for _, bad := range []string{
		"../escape.gguf",
		"../../etc/passwd",
		"a/../../outside.gguf",
		"/etc/passwd",
		"",
	} {
		if _, err := repo.safeJoin(root, bad); err == nil {
			t.Errorf("safeJoin(%q) was allowed; it must be refused", bad)
		}
	}

	got, err := repo.safeJoin(root, "Qwen/Qwen3-8B-GGUF/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "Qwen", "Qwen3-8B-GGUF", "model.gguf"); got != want {
		t.Fatalf("safeJoin = %q, want %q", got, want)
	}
}

// A symlink planted inside the repository must not become a way out of it —
// which is why containment is checked after resolution, not before.
func TestSafeJoinRefusesSymlinkOut(t *testing.T) {
	repo, root := newTestRepo(t)

	outside := filepath.Join(t.TempDir(), "elsewhere.gguf")
	if err := os.WriteFile(outside, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.gguf")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := repo.safeJoin(root, "escape.gguf"); !errors.Is(err, ErrOutsideRepo) {
		t.Fatalf("a symlink out of the repository must be refused, got %v", err)
	}
}

func TestCleanHubID(t *testing.T) {
	ok := map[string]string{
		"Qwen/Qwen3-8B-GGUF":       "Qwen/Qwen3-8B-GGUF",
		" bartowski/Llama-3.2-1B ": "bartowski/Llama-3.2-1B",
		"/owner/name/":             "owner/name",
	}
	for in, want := range ok {
		got, err := cleanHubID(in)
		if err != nil || got != want {
			t.Errorf("cleanHubID(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "no-slash", "a/b/c", "../etc", "owner/../..", "own er/name",
		"https://huggingface.co/a/b", "owner/name?x=1"} {
		if got, err := cleanHubID(bad); err == nil {
			t.Errorf("cleanHubID(%q) = %q, want an error", bad, got)
		}
	}
}

// A GGUF copied onto the drive by hand must appear in the listing without any
// index row, because the disk is the source of truth.
func TestListFindsUnrecordedFiles(t *testing.T) {
	repo, root := newTestRepo(t)
	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "local")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qwen3-8b-Q4_K_M.gguf"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A partial download and a stray file must both stay out of the listing.
	if err := os.WriteFile(filepath.Join(dir, "half.gguf.part"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := repo.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("listed %d files, want 1: %+v", len(files), files)
	}
	got := files[0]
	if got.RelPath != "local/qwen3-8b-Q4_K_M.gguf" {
		t.Errorf("RelPath = %q", got.RelPath)
	}
	// Nothing was recorded, so the figures come from the filename and must be
	// flagged as the guesses they are.
	if got.ParamsB != 8 || got.Quant != "Q4_K_M" || !got.Estimated {
		t.Errorf("inferred metadata = %+v; want 8B/Q4_K_M marked estimated", got)
	}
	if got.Verified {
		t.Error("a file that was never hashed must not report itself as verified")
	}
}

// A row whose file has gone is a symptom to show, not provenance to discard.
func TestListReportsMissingFiles(t *testing.T) {
	repo, root := newTestRepo(t)
	_ = root
	ctx := context.Background()
	if err := repo.store.RecordRepoFile(ctx, store.RepoFile{
		RelPath: "Qwen/Qwen3-8B-GGUF/model.gguf", HubID: "Qwen/Qwen3-8B-GGUF", SizeBytes: 42,
	}); err != nil {
		t.Fatal(err)
	}
	files, err := repo.List(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !files[0].Missing {
		t.Fatalf("want one missing entry, got %+v", files)
	}
}

func TestDeleteRemovesFileAndRecord(t *testing.T) {
	repo, root := newTestRepo(t)
	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := filepath.Join(root, "Qwen", "Qwen3-8B-GGUF")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(path, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := "Qwen/Qwen3-8B-GGUF/model.gguf"
	if err := repo.store.RecordRepoFile(ctx, store.RepoFile{RelPath: rel}); err != nil {
		t.Fatal(err)
	}
	// A tag the operator applied survives the file, deliberately.
	if err := repo.store.AddTag(ctx, rel, "coding"); err != nil {
		t.Fatal(err)
	}

	if err := repo.Delete(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the file should be gone")
	}
	recorded, err := repo.store.RepoFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := recorded[rel]; ok {
		t.Error("the index row should be gone")
	}
	tags, err := repo.store.Tags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags[rel]) != 1 {
		t.Errorf("tags should outlive the file, got %v", tags[rel])
	}
}

// Deleting anything that is not weights is refused, so a traversal that somehow
// got this far still cannot remove arbitrary files.
func TestDeleteRefusesNonWeights(t *testing.T) {
	repo, root := newTestRepo(t)
	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(context.Background(), "notes.txt"); err == nil {
		t.Fatal("deleting a non-weight file must be refused")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("the refused file must still be there")
	}
	if err := repo.Delete(context.Background(), "../outside.gguf"); err == nil {
		t.Fatal("deleting outside the repository must be refused")
	}
}

// ---- downloading -----------------------------------------------------------

// hubStub serves one file, optionally honouring Range and optionally publishing
// a digest, so the resume and verification paths can be driven.
type hubStub struct {
	body      []byte
	etag      string
	allowded  bool // honour Range requests
	failAfter int  // bytes to serve before hanging up; 0 serves everything
	requests  int
}

func (h *hubStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.requests++
	start := int64(0)
	if rng := r.Header.Get("Range"); rng != "" && h.allowded {
		if n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"), 10, 64); err == nil {
			start = n
		}
	}
	if h.etag != "" {
		w.Header().Set("X-Linked-Etag", `"`+h.etag+`"`)
		// A decoy in the shape Xet's CDN uses: 64 hex characters that are not
		// the content hash. Nothing may verify against this.
		w.Header().Set("ETag", `"`+strings.Repeat("f", 64)+`"`)
	}
	w.Header().Set("X-Linked-Size", strconv.Itoa(len(h.body)))

	if start >= int64(len(h.body)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	rest := h.body[start:]
	if start > 0 {
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, len(h.body)-1, len(h.body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(rest)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(rest)))
		w.WriteHeader(http.StatusOK)
	}
	if h.failAfter > 0 && h.failAfter < len(rest) {
		_, _ = w.Write(rest[:h.failAfter])
		// Hang up mid-stream, which is what a real interrupted transfer does.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(rest)
}

// downloadTo drives a transfer against a stub, bypassing Start's URL building
// so the test can point at httptest.
func downloadTo(t *testing.T, repo *Repo, srv *httptest.Server, part string) (string, error) {
	t.Helper()
	job, _, err := repo.jobs.Start(context.Background(), "owner/name", "model.gguf", "owner/name/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	// Finish it the way run() would, so a second call in the same test is not
	// refused as a duplicate of a job nothing ever closed.
	defer func() { repo.jobs.Finish(job.ID, JobDone, nil) }()
	return repo.down.transfer(context.Background(), job.ID, srv.URL, part)
}

func TestTransferResumesAfterInterruption(t *testing.T) {
	repo, root := newTestRepo(t)
	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 64<<10)
	for i := range body {
		body[i] = byte(i % 251)
	}
	sum := sha256.Sum256(body)

	stub := &hubStub{body: body, etag: hex.EncodeToString(sum[:]), allowded: true, failAfter: 8 << 10}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	part := filepath.Join(root, "model.gguf"+partSuffix)

	// First attempt is cut off part-way and must leave what it got.
	if _, err := downloadTo(t, repo, srv, part); err == nil {
		t.Fatal("an interrupted transfer should report an error")
	}
	info, err := os.Stat(part)
	if err != nil {
		t.Fatalf("the partial file must survive for a resume: %v", err)
	}
	if info.Size() == 0 || info.Size() >= int64(len(body)) {
		t.Fatalf("partial file is %d bytes, want a genuine prefix of %d", info.Size(), len(body))
	}
	partial := info.Size()

	// Second attempt resumes from that offset and completes.
	stub.failAfter = 0
	digest, err := downloadTo(t, repo, srv, part)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if digest != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %q", digest)
	}
	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(body) {
		t.Fatalf("resumed file is %d bytes, want %d", len(got), len(body))
	}
	// The bytes must be the real file, not a prefix repeated or bytes doubled
	// up at the join — the failure a naive resume produces.
	if string(got) != string(body) {
		t.Fatal("resumed file does not match the original")
	}
	if partial >= int64(len(body)) {
		t.Fatal("the test did not actually exercise a resume")
	}
}

// A server that ignores Range must cause a restart from zero, not a file with
// the tail written over the middle.
func TestTransferRestartsWhenRangeIgnored(t *testing.T) {
	repo, root := newTestRepo(t)
	body := []byte(strings.Repeat("abcdefgh", 1024))
	stub := &hubStub{body: body, allowded: false}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	part := filepath.Join(root, "model.gguf"+partSuffix)
	if err := os.WriteFile(part, []byte("stale-prefix"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadTo(t, repo, srv, part); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatal("a server ignoring Range must produce the whole file, not an appended one")
	}
}

// A digest mismatch must discard the partial file. Keeping it would make every
// later resume start from known-bad bytes and fail identically forever.
func TestVerifyDiscardsCorruptPartial(t *testing.T) {
	repo, root := newTestRepo(t)
	part := filepath.Join(root, "model.gguf"+partSuffix)
	if err := os.WriteFile(part, []byte("not the weights you are looking for"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrong := sha256.Sum256([]byte("something else"))

	job, _, err := repo.jobs.Start(context.Background(), "o/n", "model.gguf", "o/n/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.down.verify(context.Background(), job.ID, part, hex.EncodeToString(wrong[:])); err == nil {
		t.Fatal("a digest mismatch must be reported")
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("a corrupt partial file must be discarded")
	}
}

// Hugging Face only publishes a content hash for LFS files. When there is none,
// the file is recorded as unverified rather than given a hash that proves
// nothing.
func TestVerifySkippedWithoutPublishedDigest(t *testing.T) {
	repo, root := newTestRepo(t)
	part := filepath.Join(root, "model.gguf"+partSuffix)
	if err := os.WriteFile(part, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	job, _, err := repo.jobs.Start(context.Background(), "o/n", "model.gguf", "o/n/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := repo.down.verify(context.Background(), job.ID, part, "")
	if err != nil {
		t.Fatal(err)
	}
	if sum != "" {
		t.Errorf("want an empty digest when none was published, got %q", sum)
	}
	if _, err := os.Stat(part); err != nil {
		t.Error("the file must be left alone when there is nothing to check")
	}
}

// resp builds a response, optionally as the tail of a redirect chain.
func resp(h http.Header, prev *http.Response) *http.Response {
	r := &http.Response{Header: h, Request: &http.Request{}}
	if prev != nil {
		r.Request.Response = prev
	}
	return r
}

func TestPublishedDigest(t *testing.T) {
	const contentSHA = "9ee36184e616dfc76df4f5dd66f908dbde6979524ae36e6cefb67f532f798cb8"
	const xetHash = "e3545da244065b001a228bb6fce131405cb600be4d4a56057957aa83ef9deed9"

	// The case that matters, taken from a real Xet-backed repository. The
	// huggingface.co hop carries the content sha256 in X-Linked-Etag; the CDN
	// it redirects to answers with the Xet hash in a plain ETag, which is also
	// 64 hex characters and is not a hash of the bytes at all. Reading the
	// final response alone rejects a download that arrived perfectly.
	hf := resp(http.Header{
		"X-Linked-Etag": {`"` + contentSHA + `"`},
		"X-Xet-Hash":    {xetHash},
	}, nil)
	cdn := resp(http.Header{"Etag": {`"` + xetHash + `"`}}, hf)

	if got := publishedDigest(cdn); got != contentSHA {
		t.Errorf("across a Xet redirect, digest = %q; want the content sha256 %q", got, contentSHA)
	}

	valid := strings.Repeat("ab", 32)
	cases := []struct {
		name string
		in   *http.Response
		want string
	}{
		{"direct", resp(http.Header{"X-Linked-Etag": {`"` + valid + `"`}}, nil), valid},
		{"weak validator", resp(http.Header{"X-Linked-Etag": {`W/"` + valid + `"`}}, nil), valid},
		// A bare ETag is never trusted: on a Xet file it is the wrong hash, and
		// on a git-stored file it is a blob sha1.
		{"bare etag ignored", resp(http.Header{"Etag": {`"` + valid + `"`}}, nil), ""},
		{"git sha1", resp(http.Header{"Etag": {`"da39a3ee5e6b4b0d3255bfef95601890afd80709"`}}, nil), ""},
		{"nothing", resp(http.Header{}, nil), ""},
	}
	for _, c := range cases {
		if got := publishedDigest(c.in); got != c.want {
			t.Errorf("%s: publishedDigest = %q, want %q", c.name, got, c.want)
		}
	}
}

// ---- jobs ------------------------------------------------------------------

// Two transfers writing one .part file would interleave their bytes into a file
// of the right length and entirely wrong content.
func TestJobsRefuseDuplicate(t *testing.T) {
	jobs := NewJobs()
	if _, _, err := jobs.Start(context.Background(), "o/n", "m.gguf", "o/n/m.gguf"); err != nil {
		t.Fatal(err)
	}
	_, _, err := jobs.Start(context.Background(), "o/n", "m.gguf", "o/n/m.gguf")
	var dup ErrAlreadyRunning
	if !errors.As(err, &dup) {
		t.Fatalf("want ErrAlreadyRunning, got %v", err)
	}
	// A different file is fine.
	if _, _, err := jobs.Start(context.Background(), "o/n", "b.gguf", "o/n/b.gguf"); err != nil {
		t.Fatal(err)
	}
}

// A job that resumed at 80% must not report the transfer rate of a download
// that fetched all of it.
func TestJobRateExcludesResumedBytes(t *testing.T) {
	jobs := NewJobs()
	job, _, err := jobs.Start(context.Background(), "o/n", "m.gguf", "o/n/m.gguf")
	if err != nil {
		t.Fatal(err)
	}
	jobs.Update(job.ID, func(j *Job) {
		j.TotalBytes = 1000
		j.ResumedBytes = 800
		j.DoneBytes = 800
	})
	time.Sleep(20 * time.Millisecond)
	jobs.Update(job.ID, func(j *Job) { j.DoneBytes = 900 })

	got := jobs.List()
	if len(got) != 1 {
		t.Fatalf("want one job, got %d", len(got))
	}
	elapsed := got[0].UpdatedAt.Sub(got[0].StartedAt).Seconds()
	want := 100 / elapsed
	if got[0].BytesPerSecond > want*1.5 || got[0].BytesPerSecond < want*0.5 {
		t.Errorf("rate = %.0f B/s, want about %.0f (100 bytes moved, not 900)",
			got[0].BytesPerSecond, want)
	}
}

func TestJobsCancelAndFinish(t *testing.T) {
	jobs := NewJobs()
	job, ctx, err := jobs.Start(context.Background(), "o/n", "m.gguf", "o/n/m.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("cancelling a job must cancel its context")
	}
	jobs.Finish(job.ID, JobCancelled, nil)
	if err := jobs.Cancel(job.ID); err == nil {
		t.Fatal("cancelling a finished job must be refused")
	}
	if jobs.Running() != 0 {
		t.Fatal("no jobs should be running")
	}
	if err := jobs.Cancel("no-such-job"); err == nil {
		t.Fatal("cancelling an unknown job must be refused")
	}
}

// Sanity check that the stub is a faithful enough server for the tests above.
func TestStubServesRanges(t *testing.T) {
	stub := &hubStub{body: []byte("0123456789"), allowded: true}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Range", "bytes=4-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "456789" {
		t.Fatalf("range read = %q", got)
	}
}

// A repository the server can see but cannot write to must be reported as the
// operator's problem, not as an internal fault. This is the single most likely
// first failure on a real deployment — a drive owned by another user, or
// systemd's ProtectSystem=strict — and "internal error" is useless against it.
func TestInitialiseReportsUnwritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, which can write to anything")
	}
	repo, root := newTestRepo(t)
	if err := os.Chmod(root, 0o555); err != nil {
		t.Skipf("cannot make the directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	err := repo.Initialise()
	if !errors.Is(err, ErrNotWritable) {
		t.Fatalf("want ErrNotWritable, got %v", err)
	}
	// It must name what to actually do, not merely that something was denied.
	for _, want := range []string{"permission denied", "owned by uid", "ReadWritePaths", root} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message should mention %q:\n%v", want, err)
		}
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("unwritable is a different problem from absent and must not be conflated")
	}
}
