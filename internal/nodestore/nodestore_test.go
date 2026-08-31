package nodestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wintermute/internal/node"
)

// The retry path is exercised by several tests here, and at the real backoff
// that is half a minute of sleeping for no added coverage.
func TestMain(m *testing.M) {
	retryBackoff = time.Millisecond
	os.Exit(m.Run())
}

func newStore(t *testing.T, rt Runtime, ing Ingester) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir, rt, ing)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// An assignment is text from the server. It is the one place server-supplied
// text becomes a path on this host, so it is treated as hostile.
func TestPathConfinesAssignments(t *testing.T) {
	s, root := newStore(t, RuntimeNone, nil)

	for _, bad := range []string{
		"../escape.gguf",
		"../../etc/passwd",
		"a/../../out.gguf",
		"/etc/passwd",
		"",
		"   ",
	} {
		if got, err := s.Path(bad); err == nil {
			t.Errorf("Path(%q) = %q, must be refused", bad, got)
		}
	}

	got, err := s.Path("Qwen/Qwen3-8B-GGUF/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "Qwen", "Qwen3-8B-GGUF", "model.gguf"); got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestPathRefusesSymlinkOut(t *testing.T) {
	s, root := newStore(t, RuntimeNone, nil)
	outside := filepath.Join(t.TempDir(), "elsewhere.gguf")
	if err := os.WriteFile(outside, []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.gguf")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := s.Path("link.gguf"); err == nil {
		t.Fatal("a link out of the store must be refused")
	}
}

// A partial download is not a model. Counting it as present would have the node
// report weights it cannot serve, and the server then load what is not there.
func TestScanSeparatesPartialsFromModels(t *testing.T) {
	s, root := newStore(t, RuntimeNone, nil)
	write := func(name, body string) {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/done.gguf", "weights")
	write("a/half.gguf.part", "part")
	write("a/notes.txt", "x")

	report := s.Scan()
	if report.Path != root {
		t.Errorf("Path = %q", report.Path)
	}
	if len(report.Files) != 2 {
		t.Fatalf("want two entries (one model, one partial), got %+v", report.Files)
	}
	byPath := map[string]node.StoreFile{}
	for _, f := range report.Files {
		byPath[f.RelPath] = f
	}
	if f, ok := byPath["a/done.gguf"]; !ok || f.Partial || f.SizeBytes != 7 {
		t.Errorf("completed file = %+v", f)
	}
	if f, ok := byPath["a/half.gguf"]; !ok || !f.Partial {
		t.Errorf("partial = %+v", f)
	}
	if !s.Has("a/done.gguf") {
		t.Error("a completed file should count as held")
	}
	if s.Has("a/half.gguf") {
		t.Error("a partial download must not count as held")
	}
}

// A completed file beside its own leftover .part must be reported once, as
// present rather than as a transfer still running.
func TestScanPrefersCompletedOverPartial(t *testing.T) {
	s, root := newStore(t, RuntimeNone, nil)
	for _, n := range []string{"m.gguf", "m.gguf.part"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	report := s.Scan()
	if len(report.Files) != 1 {
		t.Fatalf("want one entry, got %+v", report.Files)
	}
	if report.Files[0].Partial {
		t.Error("a completed file must not be reported as a partial")
	}
}

func TestPendingIsWhatIsAbsent(t *testing.T) {
	s, root := newStore(t, RuntimeNone, nil)
	if err := os.WriteFile(filepath.Join(root, "have.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pending := s.Pending([]node.Assignment{
		{RelPath: "have.gguf"}, {RelPath: "want.gguf"},
	})
	if len(pending) != 1 || pending[0].RelPath != "want.gguf" {
		t.Fatalf("Pending = %+v; a store with no runtime has nothing to import", pending)
	}
}

// Weights can be here without this agent having fetched them: copied in by
// hand, or on a share the store points at. The import is then the whole of the
// work, and selecting on absence alone skipped it — leaving the file assigned,
// present, unservable and never retried, which the deploy screen showed as a
// transfer that would not finish.
func TestPendingIncludesPresentButUnservable(t *testing.T) {
	ing := stubIngester{names: map[string]bool{"imported": true}}
	s, root := newStore(t, RuntimeOllama, ing)
	for _, name := range []string{"imported.gguf", "copied-in.gguf"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("weights"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pending := s.Pending([]node.Assignment{
		{RelPath: "imported.gguf"}, {RelPath: "copied-in.gguf"}, {RelPath: "absent.gguf"},
	})

	got := map[string]bool{}
	for _, a := range pending {
		got[a.RelPath] = true
	}
	if got["imported.gguf"] {
		t.Error("a model the runtime already serves has nothing outstanding")
	}
	if !got["copied-in.gguf"] {
		t.Error("a model that is here but not servable still needs importing")
	}
	if !got["absent.gguf"] {
		t.Error("a model that is not here still needs fetching")
	}
}

// An unreachable runtime is exactly when an import cannot succeed, and Ollama's
// takes the digest of every byte of a file before it finds that out. So a
// runtime that will not answer is left alone rather than treated as serving
// nothing, which would re-hash every model on the host once a minute for as
// long as the outage lasted.
func TestPendingLeavesAnUnreachableRuntimeAlone(t *testing.T) {
	ing := stubIngester{err: errors.New("connection refused")}
	s, root := newStore(t, RuntimeOllama, ing)
	if err := os.WriteFile(filepath.Join(root, "here.gguf"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}

	pending := s.Pending([]node.Assignment{{RelPath: "here.gguf"}, {RelPath: "absent.gguf"}})
	if len(pending) != 1 || pending[0].RelPath != "absent.gguf" {
		t.Fatalf("Pending = %+v, want the fetch alone", pending)
	}
}

func TestNewRejectsBadRuntime(t *testing.T) {
	if _, err := New(t.TempDir(), Runtime("vllm"), nil); err == nil {
		t.Fatal("an unknown runtime must be refused at startup, not at first use")
	}
	if _, err := New("", RuntimeNone, nil); err == nil {
		t.Fatal("an empty directory must report ErrNoStore")
	}
}

func TestModelName(t *testing.T) {
	cases := map[string]string{
		"Qwen/Qwen3-8B-GGUF/Qwen3-8B-Q4_K_M.gguf": "qwen3-8b-q4_k_m",
		"model.gguf":   "model",
		"a/b/C.D.gguf": "c.d",
	}
	for in, want := range cases {
		if got := ModelName(in); got != want {
			t.Errorf("ModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- fetching --------------------------------------------------------------

// repoStub serves one file the way the server's repo endpoint does, honouring
// Range so resume can be exercised.
type repoStub struct {
	body      []byte
	failAfter int
	token     string
	gotAuth   string
}

func (h *repoStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.gotAuth = r.Header.Get("Authorization")
	start := int64(0)
	if rng := r.Header.Get("Range"); rng != "" {
		if n, err := strconv.ParseInt(
			strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"), 10, 64); err == nil {
			start = n
		}
	}
	if start >= int64(len(h.body)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	rest := h.body[start:]
	if start > 0 {
		w.Header().Set("Content-Range",
			"bytes "+strconv.FormatInt(start, 10)+"-"+strconv.Itoa(len(h.body)-1)+"/"+strconv.Itoa(len(h.body)))
		w.WriteHeader(http.StatusPartialContent)
	}
	if h.failAfter > 0 && h.failAfter < len(rest) {
		_, _ = w.Write(rest[:h.failAfter])
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(rest)
}

func TestFetchResumesAndVerifies(t *testing.T) {
	s, root := newStore(t, RuntimeNone, nil)
	body := make([]byte, 48<<10)
	for i := range body {
		body[i] = byte(i % 253)
	}
	sum := sha256.Sum256(body)

	stub := &repoStub{body: body, failAfter: 6 << 10}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	f := NewFetcher(s, srv.URL, "wm_token")
	assignment := node.Assignment{
		RelPath:   "Qwen/Q.gguf",
		SizeBytes: int64(len(body)),
		SHA256:    hex.EncodeToString(sum[:]),
	}

	// Interrupted: the partial survives and the model does not appear.
	if err := f.Fetch(context.Background(), assignment); err == nil {
		t.Fatal("an interrupted fetch should report an error")
	}
	if s.Has(assignment.RelPath) {
		t.Fatal("an interrupted fetch must not produce a usable model")
	}
	part := filepath.Join(root, "Qwen", "Q.gguf"+partSuffix)
	info, err := os.Stat(part)
	if err != nil {
		t.Fatalf("the partial must survive for a resume: %v", err)
	}
	if info.Size() == 0 || info.Size() >= int64(len(body)) {
		t.Fatalf("partial is %d bytes, want a prefix of %d", info.Size(), len(body))
	}

	// Resumed: completes, verifies, and is byte-identical.
	stub.failAfter = 0
	if err := f.Fetch(context.Background(), assignment); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if !s.Has(assignment.RelPath) {
		t.Fatal("the model should be held after a successful fetch")
	}
	got, err := os.ReadFile(filepath.Join(root, "Qwen", "Q.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatal("the resumed file does not match the original")
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Error("the partial should be gone once the file is complete")
	}
	if stub.gotAuth != "Bearer wm_token" {
		t.Errorf("the fetch must authenticate, got %q", stub.gotAuth)
	}
}

// A digest mismatch must discard the partial, or every later resume starts from
// known-bad bytes and fails identically forever.
func TestFetchDiscardsCorruptDownload(t *testing.T) {
	s, root := newStore(t, RuntimeNone, nil)
	body := []byte(strings.Repeat("z", 4096))
	srv := httptest.NewServer(&repoStub{body: body})
	defer srv.Close()

	wrong := sha256.Sum256([]byte("different"))
	err := NewFetcher(s, srv.URL, "t").Fetch(context.Background(), node.Assignment{
		RelPath: "bad.gguf", SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(wrong[:]),
	})
	if err == nil {
		t.Fatal("a digest mismatch must be reported")
	}
	if _, statErr := os.Stat(filepath.Join(root, "bad.gguf"+partSuffix)); !os.IsNotExist(statErr) {
		t.Error("a corrupt partial must be discarded")
	}
	if s.Has("bad.gguf") {
		t.Error("a file that failed verification must not be installed")
	}
}

// A truncated transfer that somehow reports success must still be caught by
// length, which is the only check available when no digest was published.
func TestFetchChecksLengthWithoutDigest(t *testing.T) {
	s, _ := newStore(t, RuntimeNone, nil)
	srv := httptest.NewServer(&repoStub{body: []byte("short")})
	defer srv.Close()

	err := NewFetcher(s, srv.URL, "t").Fetch(context.Background(), node.Assignment{
		RelPath: "m.gguf", SizeBytes: 999,
	})
	if err == nil {
		t.Fatal("a file shorter than promised must be refused")
	}
	if s.Has("m.gguf") {
		t.Error("a short file must not be installed")
	}
}

// An assignment naming a traversal must be refused before any request is made.
func TestFetchRefusesTraversalAssignment(t *testing.T) {
	s, _ := newStore(t, RuntimeNone, nil)
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	defer srv.Close()

	if err := NewFetcher(s, srv.URL, "t").Fetch(context.Background(),
		node.Assignment{RelPath: "../../escape.gguf"}); err == nil {
		t.Fatal("a traversal in an assignment must be refused")
	}
	if reached {
		t.Error("a refused assignment must not reach the network")
	}
}

// ---- llama.cpp ingestion ---------------------------------------------------

func TestLlamaCPPWritesConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "llama-swap.yaml")
	ing := NewLlamaCPPIngester(cfg, "", "/usr/bin/llama-server", []string{"--n-gpu-layers", "99"})

	if err := ing.Ingest(context.Background(), "Qwen/Qwen3-8B-Q4_K_M.gguf",
		"/store/Qwen/Qwen3-8B-Q4_K_M.gguf"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		"models:", "qwen3-8b-q4_k_m:", "/usr/bin/llama-server",
		"${PORT}", `"/store/Qwen/Qwen3-8B-Q4_K_M.gguf"`, "--n-gpu-layers 99",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config missing %q:\n%s", want, out)
		}
	}

	names, err := ing.ServableNames()
	if err != nil {
		t.Fatal(err)
	}
	if !names["qwen3-8b-q4_k_m"] {
		t.Errorf("ServableNames = %v", names)
	}
}

// A path with a space in it, unquoted, would silently truncate the command and
// serve the wrong model — or nothing.
func TestLlamaCPPQuotesAwkwardPaths(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.yaml")
	ing := NewLlamaCPPIngester(cfg, "", "llama-server", nil)
	if err := ing.Ingest(context.Background(), "a b.gguf", "/my models/a b.gguf"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(cfg)
	if !strings.Contains(string(raw), `"/my models/a b.gguf"`) {
		t.Errorf("a path with a space must be quoted:\n%s", raw)
	}
}

// With no config path there is nothing to generate, and the agent must not
// claim the host can serve anything.
func TestLlamaCPPWithoutConfigClaimsNothing(t *testing.T) {
	ing := NewLlamaCPPIngester("", "", "llama-server", nil)
	if err := ing.Ingest(context.Background(), "m.gguf", "/store/m.gguf"); err != nil {
		t.Fatal(err)
	}
	names, err := ing.ServableNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("nothing is generated, so nothing is servable; got %v", names)
	}
}

// The generated config is written in full from what this agent knows, and what
// it knows starts empty on every start. So the first import after a restart
// used to produce a config naming one model and dropping every other — taking
// them out of service on a host that still had the weights, and reporting them
// as not servable while the files sat right there.
func TestLlamaCPPConfigSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "llama-swap.yaml")
	for _, name := range []string{"first.gguf", "second.gguf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("weights"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Opening over a store that already holds weights describes them, whether
	// or not this agent is the one that fetched them.
	if _, err := New(dir, RuntimeLlamaCPP, NewLlamaCPPIngester(cfg, "", "llama-server", nil)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("a store with weights in it must be described at startup: %v", err)
	}
	for _, want := range []string{"first:", "second:"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config missing %q:\n%s", want, raw)
		}
	}

	// The agent restarts, and a third model arrives afterwards.
	restarted := NewLlamaCPPIngester(cfg, "", "llama-server", nil)
	if _, err := New(dir, RuntimeLlamaCPP, restarted); err != nil {
		t.Fatal(err)
	}
	third := filepath.Join(dir, "third.gguf")
	if err := os.WriteFile(third, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Ingest(context.Background(), "third.gguf", third); err != nil {
		t.Fatal(err)
	}

	raw, err = os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first:", "second:", "third:"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("an import after a restart dropped %q from the config:\n%s", want, raw)
		}
	}

	names, err := restarted.ServableNames()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first", "second", "third"} {
		if !names[want] {
			t.Errorf("ServableNames = %v, missing %q", names, want)
		}
	}
}

// An empty store is left alone. A config with no models in it is how an
// operator who pointed this at a file they maintain themselves would lose it,
// and holding nothing yet is the ordinary state of a new node.
func TestLlamaCPPLeavesAnEmptyStoreAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "llama-swap.yaml")
	if err := os.WriteFile(cfg, []byte("models:\n  hand-written:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir, RuntimeLlamaCPP, NewLlamaCPPIngester(cfg, "", "llama-server", nil)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "hand-written") {
		t.Errorf("an empty store must not rewrite the config:\n%s", raw)
	}
}

// stubIngester stands in for a runtime that can serve some names and not
// others, and that knows where it listens.
type stubIngester struct {
	names    map[string]bool
	endpoint string
	// err stands in for a runtime that is not answering.
	err error
}

func (s stubIngester) ServableNames() (map[string]bool, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.names, nil
}
func (s stubIngester) Ingest(context.Context, string, string) error { return nil }
func (s stubIngester) Describe() string                             { return "stub" }
func (s stubIngester) Endpoint() string                             { return s.endpoint }

// The server declares a backend from what a scan reports, so two fields have to
// survive the trip: where the runtime listens, and what it calls each file.
// Deriving either on the server would mean guessing at a vocabulary that
// belongs to whichever runtime is installed here.
func TestScanReportsRuntimeAddressAndServeNames(t *testing.T) {
	ing := stubIngester{
		names:    map[string]bool{"done": true},
		endpoint: "http://127.0.0.1:11434",
	}
	s, root := newStore(t, RuntimeOllama, ing)
	for _, name := range []string{"done.gguf", "waiting.gguf"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("weights"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	report := s.Scan()
	if report.RuntimeURL != "http://127.0.0.1:11434" {
		t.Errorf("RuntimeURL = %q", report.RuntimeURL)
	}
	byPath := map[string]node.StoreFile{}
	for _, f := range report.Files {
		byPath[f.RelPath] = f
	}
	if f := byPath["done.gguf"]; !f.Ingested || f.ServeName != "done" {
		t.Errorf("an imported file reported %+v", f)
	}
	// The name is reported before the import as well as after: a server waiting
	// on one needs to know what it will be called when it arrives.
	if f := byPath["waiting.gguf"]; f.Ingested || f.ServeName != "waiting" {
		t.Errorf("a file the runtime cannot serve yet reported %+v", f)
	}
}

// A node that keeps weights and runs nothing has no address to report, and must
// not invent one — the server would offer it as a backend that answers nothing.
func TestScanWithoutRuntimeReportsNoAddress(t *testing.T) {
	s, root := newStore(t, RuntimeNone, nil)
	if err := os.WriteFile(filepath.Join(root, "m.gguf"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if report := s.Scan(); report.RuntimeURL != "" {
		t.Errorf("RuntimeURL = %q for a node with no runtime", report.RuntimeURL)
	}
}
