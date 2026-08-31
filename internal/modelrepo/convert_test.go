package modelrepo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wintermute/internal/models"
)

// stubHub answers the file listing a release would have.
type stubHub struct {
	files []models.HubFile
	err   error
}

func (s stubHub) Tree(context.Context, string, string, string, string) (models.HubTree, error) {
	if s.err != nil {
		return models.HubTree{}, s.err
	}
	return models.HubTree{Files: s.files}, nil
}

func file(path string, size int64) models.HubFile {
	return models.HubFile{Path: path, Type: "file", Size: size}
}

// A release is a directory of pieces, and only some of them are ingredients.
// The rule that earns its place is the one about subdirectories: repositories
// routinely carry a second copy of the weights under original/, and fetching
// both would double the largest transfer this server makes.
func TestReleaseFilesTakesTheRootLevelIngredients(t *testing.T) {
	hub := stubHub{files: []models.HubFile{
		file("config.json", 1200),
		file("model-00001-of-00002.safetensors", 4_000_000),
		file("model-00002-of-00002.safetensors", 4_000_000),
		file("model.safetensors.index.json", 900),
		file("tokenizer.json", 5000),
		file("tokenizer.model", 800),
		file("merges.txt", 400),
		file("README.md", 9000),
		file("LICENSE", 1000),
		file(".gitattributes", 100),
		file("figures/benchmark.png", 200000),
		file("original/consolidated.safetensors", 8_000_000),
	}}

	got, err := releaseFiles(context.Background(), hub, "Qwen/Qwen3-8B", "main")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, f.Path)
	}
	want := []string{
		"config.json", "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors",
		"model.safetensors.index.json", "tokenizer.json", "tokenizer.model", "merges.txt",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("selected %v,\n want %v", names, want)
	}
}

// A repository with no safetensors is not a conversion candidate, and saying so
// before the fetch is the difference between a message and an hour.
func TestReleaseFilesRefusesARepositoryWithNoWeights(t *testing.T) {
	hub := stubHub{files: []models.HubFile{
		file("README.md", 9000),
		file("model-q4_k_m.gguf", 4_000_000),
	}}

	_, err := releaseFiles(context.Background(), hub, "someone/gguf-only", "main")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
}

// Conversion is off until a converter is named, and it must say so up front
// rather than after fetching sixty gigabytes.
func TestStartConvertNeedsAConverter(t *testing.T) {
	repo, _ := newTestRepo(t)
	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}
	_, err := repo.down.StartConvert(context.Background(), stubHub{},
		ConvertRequest{HubID: "Qwen/Qwen3-8B"})
	if !errors.Is(err, ErrNoConverter) {
		t.Fatalf("want ErrNoConverter, got %v", err)
	}
}

// A conversion writes more than anything else this server does, so it may not
// be the one path that skips the marker check and fills a root filesystem with
// an unmounted drive's worth of weights.
func TestStartConvertRequiresAnInitialisedRepository(t *testing.T) {
	repo, _ := newTestRepo(t)
	repo.down.ConvertCommand = "sh /nonexistent"

	_, err := repo.down.StartConvert(context.Background(), stubHub{},
		ConvertRequest{HubID: "Qwen/Qwen3-8B"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable for a directory with no marker, got %v", err)
	}
}

// A repository the service cannot write to is the commonest deployment fault
// there is — ProtectSystem=strict makes every path outside StateDirectory
// read-only — and it must be a message before the job rather than a raw mkdir
// error four seconds into one.
func TestStartConvertReportsAnUnwritableRepository(t *testing.T) {
	repo, root := newTestRepo(t)
	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}
	repo.down.ConvertCommand = "sh /nonexistent"
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	_, err := repo.down.StartConvert(context.Background(), stubHub{},
		ConvertRequest{HubID: "Qwen/Qwen3-8B"})
	if !errors.Is(err, ErrNotWritable) {
		t.Fatalf("want ErrNotWritable, got %v", err)
	}
	// The message has to name the fix. Both causes are outside this server and
	// the operator cannot guess between them.
	if !strings.Contains(err.Error(), "ReadWritePaths") {
		t.Errorf("the error must name the systemd fix, got %q", err)
	}
	if len(repo.jobs.List()) != 0 {
		t.Error("no job should exist for a repository that cannot be written to")
	}
}

// The whole pipeline against a stub Hub and a converter that is a shell script:
// the release is fetched, converted, filed under a name that says it was
// converted, and the staging directory is gone afterwards.
func TestConvertFetchesConvertsAndFiles(t *testing.T) {
	repo, root := newTestRepo(t)
	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}

	shard := strings.Repeat("w", 4096)
	config := `{"architectures":["Qwen3ForCausalLM"]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".safetensors"):
			_, _ = w.Write([]byte(shard))
		default:
			_, _ = w.Write([]byte(config))
		}
	}))
	defer srv.Close()

	// A converter that reads the staged directory and writes a file, which is
	// all this package needs to be true of the real one.
	script := filepath.Join(t.TempDir(), "convert.sh")
	body := "#!/bin/sh\n" +
		"# args: <dir> --outfile <out> --outtype f16\n" +
		"test -f \"$1/config.json\" || { echo 'no config staged' >&2; exit 1; }\n" +
		"test -f \"$1/model.safetensors\" || { echo 'no weights staged' >&2; exit 1; }\n" +
		"printf 'GGUF-converted' > \"$3\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.down.ConvertCommand = "sh " + script
	repo.down.hubBase = srv.URL

	hub := stubHub{files: []models.HubFile{
		file("config.json", int64(len(config))),
		file("model.safetensors", int64(len(shard))),
	}}

	job, err := repo.down.StartConvert(context.Background(), hub,
		ConvertRequest{HubID: "Qwen/Qwen3-8B"})
	if err != nil {
		t.Fatal(err)
	}
	final := waitForJob(t, repo, job.ID)
	if final.State != JobDone {
		t.Fatalf("job %s: %s", final.State, final.Error)
	}

	out := filepath.Join(root, "Qwen", "Qwen3-8B", "Qwen3-8B-f16.gguf")
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the converted model was not filed: %v", err)
	}
	if string(got) != "GGUF-converted" {
		t.Errorf("filed %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, stagingDir)); !os.IsNotExist(err) {
		t.Error("the staging directory should be gone once the model is filed")
	}
	// The digest is this server's own: nothing upstream published one for a
	// file it never had.
	files, err := repo.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].SHA256 == "" {
		t.Errorf("listed %+v; want one entry carrying a digest", files)
	}
}

// A converter that fails must fail the job with what it said, and must not
// leave a plausible-looking GGUF behind.
func TestConvertReportsConverterFailure(t *testing.T) {
	repo, root := newTestRepo(t)
	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	script := filepath.Join(t.TempDir(), "fail.sh")
	if err := os.WriteFile(script,
		[]byte("#!/bin/sh\necho 'Model Qwen3MoeForCausalLM is not supported' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.down.ConvertCommand = "sh " + script
	repo.down.hubBase = srv.URL

	job, err := repo.down.StartConvert(context.Background(),
		stubHub{files: []models.HubFile{file("model.safetensors", 1), file("config.json", 1)}},
		ConvertRequest{HubID: "Qwen/Qwen3-Next"})
	if err != nil {
		t.Fatal(err)
	}
	final := waitForJob(t, repo, job.ID)
	if final.State != JobFailed {
		t.Fatalf("want a failed job, got %s", final.State)
	}
	if !strings.Contains(final.Error, "not supported") {
		t.Errorf("the converter's own words are the only explanation there is, got %q", final.Error)
	}
	if _, err := os.Stat(filepath.Join(root, "Qwen", "Qwen3-Next", "Qwen3-Next-f16.gguf")); !os.IsNotExist(err) {
		t.Error("a failed conversion must not leave a model behind")
	}
}

// The converter logs a line per tensor to stderr and then says what went wrong,
// so a job error built from the head of that stream is hundreds of INFO lines
// and none of the reason. This is the regression that cost an afternoon.
func TestConvertKeepsTheEndOfTheConverterLog(t *testing.T) {
	repo, _ := newTestRepo(t)
	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	// Chattier than the 8KB the job keeps, with the only line that matters at
	// the very end — exactly the shape of a real conversion.
	script := filepath.Join(t.TempDir(), "chatty.sh")
	body := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ $i -lt 400 ]; do\n" +
		"  echo \"INFO:hf-to-gguf:blk.$i.ffn_down.weight, torch.bfloat16 --> F16\" >&2\n" +
		"  i=$((i+1))\n" +
		"done\n" +
		"echo 'ERROR:hf-to-gguf:Model Qwen3_5ForConditionalGeneration is not supported' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.down.ConvertCommand = "sh " + script
	repo.down.hubBase = srv.URL

	job, err := repo.down.StartConvert(context.Background(),
		stubHub{files: []models.HubFile{file("model.safetensors", 1), file("config.json", 1)}},
		ConvertRequest{HubID: "Qwen/Qwen3.5-9B"})
	if err != nil {
		t.Fatal(err)
	}
	final := waitForJob(t, repo, job.ID)
	if final.State != JobFailed {
		t.Fatalf("want a failed job, got %s", final.State)
	}
	if !strings.Contains(final.Error, "is not supported") {
		t.Errorf("the last thing the converter said is the whole explanation, got %q", final.Error)
	}
}

// Staging is inside the repository, so it is walked by the listing unless it is
// deliberately skipped. A GGUF part-way through being written is a real .gguf
// on disk, and showing it would offer a model that cannot be loaded.
func TestListSkipsTheStagingDirectory(t *testing.T) {
	repo, root := newTestRepo(t)
	if err := repo.Initialise(); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, stagingDir, "Qwen", "Qwen3-8B")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "half.gguf"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := repo.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("listed %+v; staging is not repository contents", files)
	}
}

// Dismissing is not cancelling: a finished job can be cleared from the panel,
// a running one has to be stopped first, and neither touches the drive.
func TestForgetClearsOnlyFinishedJobs(t *testing.T) {
	repo, _ := newTestRepo(t)

	job, _, err := repo.jobs.Start(context.Background(), "owner/name", "m.gguf", "owner/name/m.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.jobs.Forget(job.ID); err == nil {
		t.Error("a running download must not be dismissible — that is what cancel is for")
	}

	repo.jobs.Finish(job.ID, JobFailed, errors.New("converter failed"))
	if err := repo.jobs.Forget(job.ID); err != nil {
		t.Fatalf("a finished job should be dismissible: %v", err)
	}
	if len(repo.jobs.List()) != 0 {
		t.Errorf("still listed: %+v", repo.jobs.List())
	}
	if err := repo.jobs.Forget(job.ID); err == nil {
		t.Error("dismissing the same job twice should say it is gone")
	}
}

func waitForJob(t *testing.T, repo *Repo, id string) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range repo.jobs.List() {
			if j.ID == id && j.State != JobRunning {
				return j
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("job %s did not finish", id))
	return Job{}
}
