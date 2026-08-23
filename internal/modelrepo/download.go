package modelrepo

// Fetching weights from Hugging Face.
//
// The contract implemented here is the one huggingface_hub settled on, because
// the failure modes it addresses are real and unavoidable at this size: a
// multi-gigabyte transfer over a home connection *will* be interrupted, and a
// downloader that cannot resume turns a flaky link into a file that never
// arrives. So:
//
//   - the transfer streams into a .part file and only becomes a model when it
//     is complete and verified, so an interrupted download can never be
//     mistaken for a usable one;
//   - an interrupted transfer resumes with a Range request from the length
//     already on disk, across restarts of this server as well as across retries
//     within one;
//   - the finished file is checked against the digest Hugging Face published,
//     when it publishes one, before it is given its real name;
//   - the rename is atomic, so a reader either sees the whole model or no file
//     at all.
//
// The digest deserves a note. Hugging Face's ETag is the sha256 only for files
// stored in LFS, which every GGUF of consequence is; for a small file kept in
// git it is a git blob sha1 instead, which says nothing about the content as
// this program would hash it. So verification is conditional and its absence is
// recorded rather than papered over — Entry.Verified is what the UI shows, and
// claiming to have checked something that was never checked would be worse than
// admitting the gap.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wintermute/internal/models"
	"wintermute/internal/store"
)

// maxAttempts bounds retries of one transfer. huggingface_hub gives up after
// five attempts that move no new data, and the reasoning carries: past that
// point the link is not flaky, it is down, and retrying forever hides a fault
// the operator needs to see.
const maxAttempts = 5

// progressInterval is how often a transfer publishes its position. Every chunk
// would lock the registry hundreds of times a second for a number nobody reads
// that fast.
const progressInterval = 500 * time.Millisecond

// copyBuffer is the transfer chunk size. Large enough that syscall overhead is
// irrelevant against a network stream, small enough to stay responsive to
// cancellation.
const copyBuffer = 1 << 20

// freeSpaceMargin is left unused on the drive after a download. Filling a
// filesystem to the last byte breaks things unrelated to this program.
const freeSpaceMargin = 512 << 20

// Downloader fetches model files into the repository.
type Downloader struct {
	repo   *Repo
	token  string
	client *http.Client
	log    *slog.Logger
}

// NewDownloader builds a Downloader. The token is optional and only needed for
// gated repositories.
func NewDownloader(repo *Repo, token string, log *slog.Logger) *Downloader {
	return &Downloader{
		repo:  repo,
		token: token,
		log:   log,
		// No client timeout. A timeout here would bound the whole transfer,
		// and a 24GB model over a domestic line legitimately takes hours; the
		// context handles cancellation, and a stalled connection is caught by
		// the retry loop rather than by a clock that cannot tell the
		// difference between slow and dead.
		client: &http.Client{},
	}
}

// Request names one file in one Hugging Face repository.
type Request struct {
	HubID    string `json:"hub_id"`
	Filename string `json:"filename"`
	// Revision defaults to main. Pinning a revision is what makes a download
	// reproducible, so it is accepted even though the UI does not offer it yet.
	Revision string `json:"revision,omitempty"`
	// Quant and ParamsB are what the caller already learned from the Hub's
	// parsed GGUF header. Passing them through means the index records the
	// authoritative figures rather than re-deriving them from the filename.
	Quant   string  `json:"quant,omitempty"`
	ParamsB float64 `json:"params_b,omitempty"`
}

// Start validates a request, registers a job and begins the transfer in the
// background. It returns as soon as the job exists.
//
// The background context is deliberate. The caller is an HTTP handler whose
// context is cancelled the moment it writes its response, and a download
// inheriting it would die instantly.
func (d *Downloader) Start(ctx context.Context, req Request) (*Job, error) {
	root, err := d.repo.Ready()
	if err != nil {
		return nil, err
	}
	hubID, err := cleanHubID(req.HubID)
	if err != nil {
		return nil, err
	}
	filename := store.RepoKey(req.Filename)
	if filename == "" {
		return nil, fmt.Errorf("%w: a filename is required", ErrInvalidRequest)
	}
	// The filename is a path inside somebody else's repository, and it is used
	// twice: to build the URL fetched, and — via path.Base — to name a file on
	// the operator's drive. safeJoin below would confine the second use, but a
	// traversal here is not something to quietly flatten and carry on with; it
	// means the caller sent something this server should not act on.
	for _, seg := range strings.Split(filename, "/") {
		if seg == ".." || seg == "." {
			return nil, fmt.Errorf("%w: %q contains a path traversal", ErrInvalidRequest, req.Filename)
		}
	}
	if !strings.HasSuffix(strings.ToLower(filename), weightSuffix) {
		return nil, fmt.Errorf("%w: %s is not a %s file; this repository holds GGUF weights",
			ErrInvalidRequest, filename, weightSuffix)
	}
	// The repository mirrors Hugging Face's own layout, so a drive browsed in a
	// file manager reads the same way as the site it came from.
	relPath := path.Join(hubID, path.Base(filename))
	if _, err := d.repo.safeJoin(root, relPath); err != nil {
		return nil, err
	}

	job, jobCtx, err := d.repo.jobs.Start(context.WithoutCancel(ctx), hubID, filename, relPath)
	if err != nil {
		return nil, err
	}
	go d.run(jobCtx, job.ID, root, relPath, req, hubID, filename)
	return job, nil
}

// run performs the transfer, retrying with resume, and records the result.
func (d *Downloader) run(ctx context.Context, jobID, root, relPath string, req Request,
	hubID, filename string) {

	jobs := d.repo.jobs
	dest, err := d.repo.safeJoin(root, relPath)
	if err != nil {
		jobs.Finish(jobID, JobFailed, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		jobs.Finish(jobID, JobFailed, fmt.Errorf("make repository directory: %w", err))
		return
	}

	revision := strings.TrimSpace(req.Revision)
	if revision == "" {
		revision = "main"
	}
	url := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", hubID, revision, filename)
	part := dest + partSuffix

	var digest string
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			jobs.Update(jobID, func(j *Job) { j.Attempt = attempt })
			// Back off between attempts, but stay cancellable: an operator who
			// pressed cancel should not wait out a sleep.
			select {
			case <-ctx.Done():
				jobs.Finish(jobID, JobCancelled, nil)
				return
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		digest, lastErr = d.transfer(ctx, jobID, url, part)
		if lastErr == nil {
			break
		}
		if ctx.Err() != nil {
			// Cancelled rather than failed. The .part file stays, so starting
			// the same download again picks up where this left off.
			jobs.Finish(jobID, JobCancelled, nil)
			return
		}
		d.log.Warn("model download attempt failed",
			"hub_id", hubID, "file", filename, "attempt", attempt, "error", lastErr)
	}
	if lastErr != nil {
		jobs.Finish(jobID, JobFailed, lastErr)
		return
	}

	// Verify before the rename, so a corrupted transfer never occupies the
	// name of a working model.
	sum, err := d.verify(ctx, jobID, part, digest)
	if err != nil {
		jobs.Finish(jobID, JobFailed, err)
		return
	}
	if err := os.Rename(part, dest); err != nil {
		jobs.Finish(jobID, JobFailed, fmt.Errorf("finalise download: %w", err))
		return
	}

	size := int64(0)
	if info, err := os.Stat(dest); err == nil {
		size = info.Size()
	}
	quant, paramsB := req.Quant, req.ParamsB
	if quant == "" || paramsB == 0 {
		p, q := models.Describe(filename)
		if quant == "" {
			quant = q
		}
		if paramsB == 0 {
			paramsB = p
		}
	}
	// A recording failure does not fail the download: the file is on the disk
	// and usable, and the listing walks the disk rather than this table. It
	// costs the provenance, which is worth a loud log and not a lost model.
	if err := d.repo.store.RecordRepoFile(context.WithoutCancel(ctx), store.RepoFile{
		RelPath:   relPath,
		HubID:     hubID,
		SourceURL: url,
		Quant:     quant,
		ParamsB:   paramsB,
		SizeBytes: size,
		SHA256:    sum,
	}); err != nil {
		d.log.Error("could not record downloaded model", "rel_path", relPath, "error", err)
	}
	jobs.Update(jobID, func(j *Job) { j.Phase = "" })
	jobs.Finish(jobID, JobDone, nil)
	d.log.Info("model downloaded", "hub_id", hubID, "file", filename,
		"bytes", size, "verified", sum != "")
}

// transfer runs one attempt, resuming from whatever is already in the part
// file. It returns the digest the server published, when it published one.
func (d *Downloader) transfer(ctx context.Context, jobID, url, part string) (string, error) {
	var resumeFrom int64
	if info, err := os.Stat(part); err == nil {
		resumeFrom = info.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// The server ignored the Range header, so what follows is the whole
		// file from byte zero and anything already on disk is not a prefix of
		// it. Starting over is the only safe reading.
		resumeFrom = 0
	case http.StatusPartialContent:
		// Resuming as asked.
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("hugging face refused this file (%s) — it may be gated, "+
			"which needs HUGGINGFACE_TOKEN set to an account that has accepted its licence",
			resp.Status)
	case http.StatusNotFound:
		return "", fmt.Errorf("hugging face has no such file (%s)", resp.Status)
	case http.StatusRequestedRangeNotSatisfiable:
		// The part file is already the whole file: a previous attempt
		// transferred everything and was interrupted before verifying.
		return publishedDigest(resp), nil
	default:
		return "", fmt.Errorf("hugging face: %s", resp.Status)
	}

	total := totalSize(resp, resumeFrom)
	if err := d.checkSpace(part, total, resumeFrom); err != nil {
		return "", err
	}

	flags := os.O_CREATE | os.O_WRONLY
	if resumeFrom > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return "", fmt.Errorf("open partial download: %w", err)
	}

	d.repo.jobs.Update(jobID, func(j *Job) {
		j.Phase = "downloading"
		j.TotalBytes = total
		j.DoneBytes = resumeFrom
		j.ResumedBytes = resumeFrom
	})

	written, copyErr := d.copy(ctx, f, resp.Body, jobID, resumeFrom)

	// Flush to the platter before calling the attempt good. Without this a
	// power cut between here and the rename leaves a file the index believes
	// in and the disk never received.
	syncErr := f.Sync()
	closeErr := f.Close()
	switch {
	case copyErr != nil:
		return "", copyErr
	case syncErr != nil:
		return "", fmt.Errorf("flush partial download: %w", syncErr)
	case closeErr != nil:
		return "", fmt.Errorf("close partial download: %w", closeErr)
	}
	if total > 0 && written != total {
		return "", fmt.Errorf("transfer ended early: %d of %d bytes", written, total)
	}
	return publishedDigest(resp), nil
}

// copy streams the body into the file, publishing progress as it goes and
// stopping promptly on cancellation.
func (d *Downloader) copy(ctx context.Context, dst io.Writer, src io.Reader,
	jobID string, startAt int64) (int64, error) {

	buf := make([]byte, copyBuffer)
	done := startAt
	last := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return done, fmt.Errorf("write: %w", writeErr)
			}
			done += int64(n)
			if time.Since(last) >= progressInterval {
				last = time.Now()
				at := done
				d.repo.jobs.Update(jobID, func(j *Job) { j.DoneBytes = at })
			}
		}
		if readErr != nil {
			d.repo.jobs.Update(jobID, func(j *Job) { j.DoneBytes = done })
			if errors.Is(readErr, io.EOF) {
				return done, nil
			}
			return done, fmt.Errorf("read: %w", readErr)
		}
	}
}

// verify hashes the completed part file and compares it with what the server
// published. It returns the digest it computed, or empty when there was nothing
// to check against.
//
// The whole file is read back rather than hashed during the transfer, because a
// transfer can be resumed — across retries and across restarts of this server —
// and a hash accumulated over one attempt's bytes would be a hash of part of
// the file. Reading a completed file once is slow; being wrong about whether
// weights are intact is worse.
func (d *Downloader) verify(ctx context.Context, jobID, part, published string) (string, error) {
	if published == "" {
		// Nothing to check against. Recorded honestly as unverified rather
		// than hashed to produce a number that proves nothing.
		return "", nil
	}
	d.repo.jobs.Update(jobID, func(j *Job) { j.Phase = "verifying" })

	f, err := os.Open(part)
	if err != nil {
		return "", fmt.Errorf("verify: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	buf := make([]byte, copyBuffer)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", fmt.Errorf("verify: %w", readErr)
		}
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(sum, published) {
		// The partial file is removed here, unlike everywhere else in this
		// package. A resume is only ever correct when the bytes on disk are a
		// prefix of the real file, and a digest mismatch is proof they are
		// not — keeping it would make every later attempt resume onto corrupt
		// data and fail the same way forever.
		if err := os.Remove(part); err != nil {
			d.log.Warn("could not remove corrupt partial download", "path", part, "error", err)
		}
		return "", fmt.Errorf("downloaded file does not match the published digest "+
			"(expected %s, got %s); the partial file has been discarded so a retry starts clean",
			published, sum)
	}
	return sum, nil
}

// checkSpace refuses a download that plainly will not fit.
//
// Checked before the transfer rather than discovered at the end of one, because
// the alternative is spending an hour of bandwidth to fill a disk and then fail.
func (d *Downloader) checkSpace(part string, total, resumeFrom int64) error {
	if total <= 0 {
		return nil
	}
	_, free, err := diskSpace(filepath.Dir(part))
	if err != nil {
		// Not knowing is not a reason to refuse; the transfer will report a
		// write failure if it really runs out.
		return nil
	}
	needed := total - resumeFrom
	if free < needed+freeSpaceMargin {
		return fmt.Errorf("not enough room: %s needs %s and the drive has %s free",
			filepath.Base(part), formatBytes(needed), formatBytes(free))
	}
	return nil
}

// totalSize works out the full length of the file from the response.
//
// With a range request Content-Length is only what remains, so Content-Range's
// total is the figure that matters; X-Linked-Size is what Hugging Face reports
// for an LFS file behind the redirect.
func totalSize(resp *http.Response, resumeFrom int64) int64 {
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil {
				return n
			}
		}
	}
	if v := resp.Header.Get("X-Linked-Size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength + resumeFrom
	}
	return 0
}

// publishedDigest extracts the sha256 Hugging Face published for a file, if it
// published one.
//
// Only X-Linked-Etag counts, and only from the huggingface.co hop. Both halves
// of that sentence are load-bearing, and getting either wrong produces a
// verifier that rejects perfectly good downloads:
//
//   - Most large repositories are now backed by Xet rather than plain LFS. On a
//     Xet file the CDN's own ETag is the *Xet* hash — a Merkle hash over the
//     chunked content, which is 64 hex characters and so looks exactly like a
//     sha256 while being a completely different number. Trusting a bare ETag
//     therefore fails every Xet download at 100%, after the whole transfer.
//   - X-Linked-Etag carries the real content sha256, but it is set by
//     huggingface.co and not repeated by the CDN it redirects to. net/http
//     follows redirects transparently and hands back only the final response,
//     so the earlier hops have to be walked explicitly to find it.
//
// Anything that is not 64 hex characters is a git blob sha1 — the ETag of a
// small file kept in git rather than LFS — and says nothing about the content
// as this program would hash it, so it is discarded rather than guessed at.
func publishedDigest(resp *http.Response) string {
	// resp.Request.Response is the redirect that led here, and its own Request
	// carries the one before it. Walking that chain back reaches the original
	// huggingface.co response.
	for r := resp; r != nil; r = previousResponse(r) {
		v := strings.TrimSpace(r.Header.Get("X-Linked-Etag"))
		v = strings.TrimPrefix(v, "W/")
		v = strings.Trim(v, `"`)
		if len(v) == 64 && isHex(v) {
			return strings.ToLower(v)
		}
	}
	return ""
}

// previousResponse steps one hop back up a redirect chain.
func previousResponse(r *http.Response) *http.Response {
	if r.Request == nil {
		return nil
	}
	return r.Request.Response
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// cleanHubID validates a "owner/name" repository id.
//
// It becomes a directory path on the operator's drive and part of a URL, so it
// is checked against a conservative character set rather than trusted. Anything
// with a path traversal, a scheme or a query in it is rejected outright.
func cleanHubID(id string) (string, error) {
	id = strings.Trim(strings.TrimSpace(id), "/")
	if id == "" {
		return "", fmt.Errorf("%w: a Hugging Face repository id is required", ErrInvalidRequest)
	}
	parts := strings.Split(id, "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("%w: %q is not a Hugging Face repository id — expected owner/name",
			ErrInvalidRequest, id)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", fmt.Errorf("%w: %q is not a valid repository id", ErrInvalidRequest, id)
		}
		for _, c := range p {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case c == '-' || c == '_' || c == '.':
			default:
				return "", fmt.Errorf("%w: %q contains characters a repository id cannot have",
					ErrInvalidRequest, id)
			}
		}
	}
	return id, nil
}

// formatBytes renders a size the way an operator reads one.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
