package modelrepo

// Turning a safetensors release into a GGUF this repository can hold.
//
// The repository indexes GGUF and nothing else, for the reason weightSuffix
// gives: a GGUF is one file, and a repository of loose files is only coherent
// because that is true. A safetensors release is a directory — shards, an
// index, a config, a tokenizer — and none of those pieces is a model on its
// own.
//
// That is fine for weights somebody has already converted, which is most of
// them. It is not fine on the day a model is released, when the only thing
// published is the original safetensors and the GGUFs appear days later. This
// is the path for that day: fetch the release into a staging directory that
// the index never sees, run llama.cpp's converter over it, and file the single
// GGUF that comes out. The staging directory is then thrown away, so the
// repository ends up holding exactly what it always holds.
//
// Three things are deliberate:
//
//   - **It runs here, on the server.** Handing the work to a fleet node would
//     mean telling an agent to execute something, and the agent cannot be told
//     to do anything — see cmd/wintermute-node. The conversion is a dtype cast
//     and a repack, disk-bound rather than compute-bound, so the machine with
//     the repository on it is the right machine anyway: nothing crosses the
//     network twice.
//   - **The converter is not vendored.** It is llama.cpp's convert_hf_to_gguf.py
//     and it needs Python with torch and numpy. WINTERMUTE_CONVERT_CMD names
//     the command; an unset one turns this feature off and says so, rather
//     than failing at the end of a 60GB download.
//   - **F16 only.** Converting produces the honest intermediate — the same
//     numbers the release shipped, in a container this repository can hold.
//     Quantising is a separate decision with its own trade-offs, and doing it
//     silently would put a lossy file on the drive under a name that did not
//     say so.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"wintermute/internal/models"
	"wintermute/internal/store"
)

// stagingDir is where a release is assembled before it becomes a model. It is
// dot-prefixed and skipped by the walk in List, so a half-fetched release is
// never mistaken for repository contents.
const stagingDir = ".staging"

// convertedSuffix names what came out of a conversion rather than off the Hub.
// A repository holding both should say which is which without anyone having to
// remember.
const convertedSuffix = "-f16"

// ErrNoConverter reports that no converter command is configured.
var ErrNoConverter = errors.New(
	"no converter is configured: set WINTERMUTE_CONVERT_CMD to llama.cpp's " +
		"convert_hf_to_gguf.py, e.g. \"python3 /opt/llama.cpp/convert_hf_to_gguf.py\"")

// ConvertRequest asks for one safetensors release to be converted.
type ConvertRequest struct {
	HubID    string `json:"hub_id"`
	Revision string `json:"revision,omitempty"`
}

// Lister is the Hub's file listing, as this package needs it. Satisfied by
// *models.Hub.
//
// An interface rather than the concrete client because the Hub carries a cache,
// a rate limiter and a token that belong to the catalog, and because the tests
// here have no business talking to Hugging Face.
type Lister interface {
	Tree(ctx context.Context, id, revision, prefix, cursor string) (models.HubTree, error)
}

// stagingRoot is where releases are assembled and converted.
//
// Inside the repository by default, which needs no configuration and makes the
// last step a rename. WINTERMUTE_CONVERT_STAGING moves it to another disk,
// which is worth doing on a spinning repository drive: a conversion reads the
// release and writes the GGUF at the same time, and one head serving both
// streams spends its time seeking between them. The finished model is then
// copied across once, sequentially, which is the transfer the drive is good at.
func (d *Downloader) stagingRoot(repoRoot string) string {
	if custom := strings.TrimSpace(d.ConvertStaging); custom != "" {
		return custom
	}
	return filepath.Join(repoRoot, stagingDir)
}

// StartConvert fetches a safetensors release and converts it, returning the job
// to watch. The work continues after the request that asked for it returns.
func (d *Downloader) StartConvert(ctx context.Context, hub Lister, req ConvertRequest) (*Job, error) {
	if strings.TrimSpace(d.ConvertCommand) == "" {
		return nil, ErrNoConverter
	}
	if hub == nil {
		return nil, fmt.Errorf("%w: no hub client", ErrInvalidRequest)
	}
	// Ready rather than resolve: it also requires the marker, which is what
	// tells a mounted drive from a bare mount point. A download has always
	// checked it and a conversion writes far more, so it cannot be the one
	// path that fills a server's root filesystem with an unmounted drive's
	// worth of weights.
	root, err := d.repo.Ready()
	if err != nil {
		return nil, err
	}
	// Probed before a job exists, so a repository the service cannot write to
	// is a message at the button rather than a failure four seconds into a
	// transfer. The staging directory is the first thing the job would create.
	stagingBase := d.stagingRoot(root)
	if err := os.MkdirAll(stagingBase, 0o755); err != nil {
		return nil, writeFailure(stagingBase, err)
	}
	hubID, err := cleanHubID(req.HubID)
	if err != nil {
		return nil, err
	}
	if req.Revision, err = cleanRevision(req.Revision); err != nil {
		return nil, err
	}

	// Named for what will be on the drive when this finishes, not for what is
	// being fetched: the job is watched by somebody waiting for a model.
	outName := path.Base(hubID) + convertedSuffix + weightSuffix
	relPath := path.Join(hubID, outName)
	if _, err := d.repo.safeJoin(root, relPath); err != nil {
		return nil, err
	}
	// Refused before the download rather than after it. Converting onto a name
	// that is already taken would either overwrite a model somebody is serving
	// or waste hours discovering it could not.
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relPath))); err == nil {
		return nil, fmt.Errorf("%w: %s already exists in the repository", ErrInvalidRequest, relPath)
	}

	job, jobCtx, err := d.repo.jobs.Start(context.WithoutCancel(ctx), hubID, outName, relPath)
	if err != nil {
		return nil, err
	}
	d.repo.jobs.Update(job.ID, func(j *Job) { j.Kind = "convert" })
	go d.runConvert(jobCtx, job.ID, root, relPath, hub, req, hubID)
	return job, nil
}

// runConvert is the whole pipeline: list, fetch, convert, hash, file.
func (d *Downloader) runConvert(ctx context.Context, jobID, root, relPath string,
	hub Lister, req ConvertRequest, hubID string) {

	jobs := d.repo.jobs

	jobs.Update(jobID, func(j *Job) { j.Phase = "listing" })
	files, err := releaseFiles(ctx, hub, hubID, req.Revision)
	if err != nil {
		jobs.Finish(jobID, JobFailed, err)
		return
	}

	stagingBase := d.stagingRoot(root)
	stage := filepath.Join(stagingBase, filepath.FromSlash(hubID))
	if err := os.MkdirAll(stage, 0o755); err != nil {
		jobs.Finish(jobID, JobFailed, writeFailure(stagingBase, err))
		return
	}

	var grand int64
	for _, f := range files {
		grand += f.Size
	}
	// Staging holds the release and the GGUF at the same time. The repository
	// only ever holds the GGUF — and when staging is elsewhere, that is a
	// second drive with its own free space to check. Both said now rather than
	// at 90%.
	if err := d.checkSpace(filepath.Join(stage, "x"), grand*2, 0); err != nil {
		jobs.Finish(jobID, JobFailed, err)
		return
	}
	if stagingBase != filepath.Join(root, stagingDir) {
		if err := d.checkSpace(filepath.Join(root, "x"), grand, 0); err != nil {
			jobs.Finish(jobID, JobFailed, err)
			return
		}
	}

	var done int64
	for _, f := range files {
		if ctx.Err() != nil {
			jobs.Finish(jobID, JobCancelled, nil)
			return
		}
		dest := filepath.Join(stage, filepath.Base(f.Path))
		if info, statErr := os.Stat(dest); statErr == nil && (f.Size == 0 || info.Size() == f.Size) {
			// Already here from an earlier attempt. A release is fetched in
			// pieces and a retry should cost only the pieces that are missing.
			done += info.Size()
			jobs.Update(jobID, func(j *Job) { j.DoneBytes = done })
			continue
		}
		url := fmt.Sprintf("%s/%s/resolve/%s/%s", d.hubBase, hubID, req.Revision, f.Path)
		if err := d.fetchInto(ctx, jobID, url, dest, span{base: done, grand: grand}); err != nil {
			if ctx.Err() != nil {
				jobs.Finish(jobID, JobCancelled, nil)
				return
			}
			jobs.Finish(jobID, JobFailed, fmt.Errorf("fetch %s: %w", f.Path, err))
			return
		}
		done += f.Size
	}

	// Written inside the staging directory and moved only once it is whole, for
	// the same reason a download lands in a .part file: a converter killed
	// half way through leaves a GGUF that is a plausible file and not a model.
	out := filepath.Join(stage, path.Base(relPath))
	// The bar now measures the conversion rather than the fetch. F16 out of
	// BF16 is a cast, so the GGUF lands within a few percent of the weights
	// that went in — near enough to be worth showing, and shown as an estimate
	// rather than a promise.
	var weights int64
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Path), ".safetensors") {
			weights += f.Size
		}
	}
	jobs.Update(jobID, func(j *Job) {
		j.Phase = "converting"
		j.DoneBytes = 0
		j.TotalBytes = weights
		// Both belong to the transfer that just ended. Left set, "resumed from
		// 6.2 MB" sits under a bar that is measuring something else entirely.
		j.ResumedBytes = 0
		j.Attempt = 0
	})
	if err := d.convert(ctx, jobID, stage, out); err != nil {
		_ = os.Remove(out)
		if ctx.Err() != nil {
			jobs.Finish(jobID, JobCancelled, nil)
			return
		}
		jobs.Finish(jobID, JobFailed, err)
		return
	}

	// Hashed because nothing upstream published a digest for a file this
	// server invented. A fleet node verifies what it fetches against this, so
	// the alternative is a model that can only ever be checked by its length.
	dest, err := d.repo.safeJoin(root, relPath)
	if err != nil {
		jobs.Finish(jobID, JobFailed, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		jobs.Finish(jobID, JobFailed, writeFailure(root, err))
		return
	}
	sum, err := d.fileConverted(ctx, jobID, out, dest)
	if err != nil {
		jobs.Finish(jobID, JobFailed, err)
		return
	}
	// The shards are the one thing here that is reproducible from the Hub, and
	// they are the bulk of the disk. Kept only until the model they produced
	// exists.
	if err := os.RemoveAll(stage); err != nil {
		d.log.Warn("could not clear staging directory", "path", stage, "error", err)
	}
	pruneStaging(stagingBase, stage)

	size := int64(0)
	if info, err := os.Stat(dest); err == nil {
		size = info.Size()
	}
	paramsB, _ := models.Describe(path.Base(relPath))
	if err := d.repo.store.RecordRepoFile(context.WithoutCancel(ctx), store.RepoFile{
		RelPath:   relPath,
		HubID:     hubID,
		SourceURL: "https://huggingface.co/" + hubID,
		Quant:     "F16",
		ParamsB:   paramsB,
		SizeBytes: size,
		SHA256:    sum,
	}); err != nil {
		d.log.Error("could not record converted model", "rel_path", relPath, "error", err)
	}
	jobs.Update(jobID, func(j *Job) { j.Phase = "" })
	jobs.Finish(jobID, JobDone, nil)
	d.log.Info("model converted", "hub_id", hubID, "rel_path", relPath, "bytes", size)
}

// pruneStaging removes the empty directories a finished conversion leaves
// behind, stopping below the staging root.
//
// The root itself is left alone. By default it is ours to delete, but when an
// operator has pointed WINTERMUTE_CONVERT_STAGING at a directory on another
// disk it is theirs — possibly a mount point, possibly one they created with
// particular ownership — and a program that tidies away a directory it was
// handed is a program nobody can predict.
//
// os.Remove refuses a directory that is not empty, which is exactly right when
// a second conversion is running: its files stop the prune where they are, and
// nothing has to check or lock anything.
func pruneStaging(stagingRoot, from string) {
	for dir := from; dir != stagingRoot && strings.HasPrefix(dir, stagingRoot); dir = filepath.Dir(dir) {
		// Already gone is not a reason to stop: the model's own directory was
		// removed wholesale a moment ago, and its parents are what is left.
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			return
		}
	}
}

// fileConverted moves the finished GGUF into the repository and returns its
// digest, taking whichever of two routes the filesystems allow.
//
// A rename when staging is on the repository's own disk: instant, atomic, and
// then the file is read once to hash it. A copy when staging is elsewhere,
// because a rename across filesystems fails — and since those bytes are being
// read anyway, the digest is taken on the way past rather than in a second pass
// over eighteen gigabytes.
//
// Either way the model appears under its real name only once it is whole: the
// copy lands in a .part file beside the destination and is renamed within the
// repository, so a crash mid-copy leaves something the listing ignores rather
// than a truncated model.
func (d *Downloader) fileConverted(ctx context.Context, jobID, out, dest string) (string, error) {
	jobs := d.repo.jobs

	if err := os.Rename(out, dest); err == nil {
		size := int64(0)
		if info, statErr := os.Stat(dest); statErr == nil {
			size = info.Size()
		}
		jobs.Update(jobID, func(j *Job) {
			j.Phase = "hashing"
			j.DoneBytes, j.TotalBytes, j.Note = 0, size, ""
		})
		return fileSHA256(ctx, dest, func(read int64) {
			jobs.Update(jobID, func(j *Job) { j.DoneBytes = read })
		})
	} else if !errors.Is(err, syscall.EXDEV) {
		return "", fmt.Errorf("file the converted model: %w", err)
	}
	return d.copyConverted(ctx, jobID, out, dest)
}

// copyConverted is the cross-filesystem half of fileConverted: stream the file
// into the repository, hashing on the way past, and give it its real name only
// once it is whole.
func (d *Downloader) copyConverted(ctx context.Context, jobID, out, dest string) (string, error) {
	jobs := d.repo.jobs

	src, err := os.Open(out)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	size := int64(0)
	if info, statErr := src.Stat(); statErr == nil {
		size = info.Size()
	}

	part := dest + partSuffix
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", writeFailure(filepath.Dir(dest), err)
	}
	jobs.Update(jobID, func(j *Job) {
		j.Phase = "filing"
		j.DoneBytes, j.TotalBytes, j.Note = 0, size, ""
	})

	h := sha256.New()
	buf := make([]byte, copyBuffer)
	var written int64
	last := time.Now()
	var copyErr error
	for copyErr == nil {
		if copyErr = ctx.Err(); copyErr != nil {
			break
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			if _, writeErr := f.Write(buf[:n]); writeErr != nil {
				copyErr = writeErr
				break
			}
			written += int64(n)
			if time.Since(last) >= progressInterval {
				last = time.Now()
				at := written
				jobs.Update(jobID, func(j *Job) { j.DoneBytes = at })
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				copyErr = readErr
			}
			break
		}
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	switch {
	case copyErr != nil:
		_ = os.Remove(part)
		return "", fmt.Errorf("copy the converted model into the repository: %w", copyErr)
	case syncErr != nil:
		_ = os.Remove(part)
		return "", fmt.Errorf("flush the converted model: %w", syncErr)
	case closeErr != nil:
		return "", closeErr
	}
	if err := os.Rename(part, dest); err != nil {
		return "", fmt.Errorf("file the converted model: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// convert runs the configured converter over the staged release.
//
// The command is split on spaces so a virtualenv's interpreter and the script
// can be given together, which is how llama.cpp is actually installed. It is
// operator configuration and never model output or anything off the Hub — no
// part of an assignment, a filename or a repository id reaches it as anything
// but the two path arguments below.
func (d *Downloader) convert(ctx context.Context, jobID, srcDir, outPath string) error {
	fields := strings.Fields(d.ConvertCommand)
	if len(fields) == 0 {
		return ErrNoConverter
	}
	args := append(fields[1:], srcDir, "--outfile", outPath, "--outtype", "f16")

	cmd := exec.CommandContext(ctx, fields[0], args...)
	// The tail of stderr, not the head. Python's logging goes to stderr, so a
	// conversion writes a line per tensor there — hundreds of them — and
	// whatever went wrong is the last thing said. Keeping the first 8KB
	// captured the banner and the tensor list and discarded the error, which
	// is a worse failure than not capturing anything: it looks like an
	// explanation.
	stderr := &tailWriter{limit: 8 << 10}
	cmd.Stderr = stderr

	// Watched while it runs, because this is the step with nothing else to
	// look at: an hour in which one file grows and one process talks. Both are
	// reported, so a conversion that is working looks different from one that
	// has stopped — which is the whole question anyone has about it.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("converter failed to start: %w", err)
	}
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				written := int64(0)
				if info, err := os.Stat(outPath); err == nil {
					written = info.Size()
				}
				line := stderr.LastLine()
				d.repo.jobs.Update(jobID, func(j *Job) {
					j.DoneBytes = written
					j.Note = line
				})
			}
		}
	}()

	err := cmd.Wait()
	close(done)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		detail := stderr.String()
		if detail == "" {
			return fmt.Errorf("converter failed: %w", err)
		}
		return fmt.Errorf("converter failed: %w: %s", err, detail)
	}
	if info, err := os.Stat(outPath); err != nil || info.Size() == 0 {
		return errors.New("the converter reported success but wrote no model")
	}
	return nil
}

// releaseFiles picks the files a conversion needs out of a repository listing.
//
// Root-level only, and by extension. A release routinely carries a second copy
// of the weights in a subdirectory — original/, or a consolidated single file
// beside the shards — and fetching both would double a download that is already
// the largest thing this server does. Everything the converter reads is at the
// root: the shards, the index that orders them, the config that describes the
// architecture and the tokenizer.
func releaseFiles(ctx context.Context, hub Lister, hubID, revision string) ([]models.HubFile, error) {
	var out []models.HubFile
	cursor := ""
	for {
		tree, err := hub.Tree(ctx, hubID, revision, "", cursor)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", hubID, err)
		}
		for _, f := range tree.Files {
			if f.Type == "directory" || strings.Contains(f.Path, "/") {
				continue
			}
			if wantedForConvert(f.Path) {
				out = append(out, f)
			}
		}
		if tree.Next == "" {
			break
		}
		cursor = tree.Next
	}

	if !hasSafetensors(out) {
		return nil, fmt.Errorf(
			"%w: %s publishes no safetensors at its root, so there is nothing to convert",
			ErrInvalidRequest, hubID)
	}
	return out, nil
}

// wantedForConvert reports whether one root-level file is part of a release.
func wantedForConvert(name string) bool {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".safetensors"),
		strings.HasSuffix(lower, ".json"),
		strings.HasSuffix(lower, ".model"),
		// merges.txt and vocab.txt, which some tokenizers still need. Named
		// rather than matched on .txt so a README or a licence does not ride
		// along.
		lower == "merges.txt", lower == "vocab.txt":
		return true
	}
	return false
}

func hasSafetensors(files []models.HubFile) bool {
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Path), ".safetensors") {
			return true
		}
	}
	return false
}

// tailWriter keeps the last limit bytes written to it.
//
// A ring would be tidier and is not worth it: this holds kilobytes, the trim
// runs once per write, and a write here is one log line from a subprocess.
type tailWriter struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

// LastLine is the most recent complete line written, for reporting progress
// while the process is still running. Read from another goroutine than the one
// the subprocess writes on, hence the lock.
func (t *tailWriter) LastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// String returns what was kept, marked when it is only the tail so nobody
// reads a truncated first line as the start of the story.
func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := strings.TrimSpace(string(t.buf))
	if len(t.buf) >= t.limit {
		return "…" + out
	}
	return out
}
