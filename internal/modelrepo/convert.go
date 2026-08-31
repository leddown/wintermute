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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

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
	if err := os.MkdirAll(filepath.Join(root, stagingDir), 0o755); err != nil {
		return nil, writeFailure(root, err)
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

	stage := filepath.Join(root, stagingDir, filepath.FromSlash(hubID))
	if err := os.MkdirAll(stage, 0o755); err != nil {
		jobs.Finish(jobID, JobFailed, writeFailure(root, err))
		return
	}

	var grand int64
	for _, f := range files {
		grand += f.Size
	}
	// The conversion writes a GGUF about the size of what it read, so the disk
	// has to hold both at once. Said now rather than at 90%.
	if err := d.checkSpace(stage, grand*2, 0); err != nil {
		jobs.Finish(jobID, JobFailed, err)
		return
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
	jobs.Update(jobID, func(j *Job) {
		j.Phase = "converting"
		j.DoneBytes = grand
	})
	if err := d.convert(ctx, stage, out); err != nil {
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
	jobs.Update(jobID, func(j *Job) { j.Phase = "hashing" })
	sum, err := fileSHA256(ctx, out)
	if err != nil {
		jobs.Finish(jobID, JobFailed, err)
		return
	}

	jobs.Update(jobID, func(j *Job) { j.Phase = "filing" })
	dest, err := d.repo.safeJoin(root, relPath)
	if err != nil {
		jobs.Finish(jobID, JobFailed, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		jobs.Finish(jobID, JobFailed, writeFailure(root, err))
		return
	}
	if err := os.Rename(out, dest); err != nil {
		jobs.Finish(jobID, JobFailed, fmt.Errorf("file the converted model: %w", err))
		return
	}
	// The shards are the one thing here that is reproducible from the Hub, and
	// they are the bulk of the disk. Kept only until the model they produced
	// exists.
	if err := os.RemoveAll(stage); err != nil {
		d.log.Warn("could not clear staging directory", "path", stage, "error", err)
	}
	pruneStaging(filepath.Join(root, stagingDir), stage)

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
// behind, up to and including the staging root.
//
// os.Remove refuses a directory that is not empty, which is exactly the wanted
// behaviour when a second conversion is running: its files stop the prune where
// they are, and nothing checks or locks anything.
func pruneStaging(stagingRoot, from string) {
	for dir := from; strings.HasPrefix(dir, stagingRoot); dir = filepath.Dir(dir) {
		// Already gone is not a reason to stop: the model's own directory was
		// removed wholesale a moment ago, and its parents are what is left.
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			return
		}
		if dir == stagingRoot {
			return
		}
	}
}

// convert runs the configured converter over the staged release.
//
// The command is split on spaces so a virtualenv's interpreter and the script
// can be given together, which is how llama.cpp is actually installed. It is
// operator configuration and never model output or anything off the Hub — no
// part of an assignment, a filename or a repository id reaches it as anything
// but the two path arguments below.
func (d *Downloader) convert(ctx context.Context, srcDir, outPath string) error {
	fields := strings.Fields(d.ConvertCommand)
	if len(fields) == 0 {
		return ErrNoConverter
	}
	args := append(fields[1:], srcDir, "--outfile", outPath, "--outtype", "f16")

	cmd := exec.CommandContext(ctx, fields[0], args...)
	// Only stderr is kept. The converter writes a tensor-by-tensor log to
	// stdout that is megabytes for a large model and says nothing a failure
	// does not say better.
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, limit: 8 << 10}

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
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

// limitedWriter keeps the first limit bytes and discards the rest, so a
// converter that fails after printing a great deal still fits in a job error.
type limitedWriter struct {
	w     *strings.Builder
	limit int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if room := l.limit - l.w.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		_, _ = l.w.Write(p)
	}
	return len(p), nil
}
