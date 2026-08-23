package nodestore

// Pulling assigned weights from the wintermute server.
//
// This is deliberately its own implementation rather than a reuse of
// internal/modelrepo's downloader, and the reason is that the two are only
// superficially the same job. That one negotiates with Hugging Face: it follows
// redirects to a CDN, has to discover whether a digest was published at all,
// and has to know that a Xet-backed file's ETag is a hash of something other
// than the content. This one talks to a server that already told it the digest
// and the size in the assignment, over one hop, with a bearer token. Bending
// the first to serve the second would make the interesting part — the Hugging
// Face quirks — conditional on which caller was asking, which is how that kind
// of code stops being correct for either.
//
// What is genuinely shared is the shape, because the shape is forced by the
// problem: bytes land in a .part file, an interruption resumes with a Range
// request, and the file only takes its real name once it is complete and
// checked.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wintermute/internal/node"
)

// fetchAttempts bounds retries of one transfer, matching the repository
// downloader's five.
const fetchAttempts = 5

// fetchBuffer is the transfer chunk size.
const fetchBuffer = 1 << 20

// retryBackoff is the base delay between attempts, multiplied by the attempt
// number. A variable rather than a constant so the tests that exercise the
// retry path do not have to spend half a minute sleeping through it; nothing
// outside this package's tests changes it.
var retryBackoff = 2 * time.Second

// Fetcher pulls assignments from the server into the store.
type Fetcher struct {
	store  *Store
	server string
	token  string
	client *http.Client
	// Progress is called as bytes arrive, for the agent's log. Optional.
	Progress func(relPath string, done, total int64)
}

// NewFetcher builds a Fetcher against a wintermute server.
func NewFetcher(store *Store, server, token string) *Fetcher {
	return &Fetcher{
		store:  store,
		server: strings.TrimSuffix(strings.TrimSpace(server), "/"),
		token:  token,
		// No overall timeout: a model is gigabytes and the context is what
		// carries cancellation. A stalled connection is caught by the retry
		// loop, which can tell the difference between slow and dead in a way
		// a single deadline cannot.
		client: &http.Client{},
	}
}

// Fetch brings one assignment into the store and ingests it.
//
// Returns nil and does nothing if the file is already here, so it is safe to
// call for every assignment on every reconcile.
func (f *Fetcher) Fetch(ctx context.Context, a node.Assignment) error {
	dest, err := f.store.Path(a.RelPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dest); err == nil {
		// Already here. Ingestion is still attempted, because a file can
		// arrive and its import fail — a node that had downloaded twelve
		// gigabytes and then never retried the import would look permanently
		// broken for no reason.
		return f.ingest(ctx, a.RelPath, dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("make store directory: %w", err)
	}

	// Refuse a transfer that plainly will not fit, rather than filling the
	// node's disk and taking down whatever else it runs.
	if a.SizeBytes > 0 {
		if _, free, err := diskSpace(f.store.root); err == nil && free < a.SizeBytes {
			return fmt.Errorf("%s needs %d bytes and the store has %d free",
				a.RelPath, a.SizeBytes, free)
		}
	}

	part := dest + partSuffix
	var lastErr error
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * retryBackoff):
			}
		}
		if lastErr = f.transfer(ctx, a, part); lastErr == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if lastErr != nil {
		return lastErr
	}

	if err := f.verify(part, a); err != nil {
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		return fmt.Errorf("finalise %s: %w", a.RelPath, err)
	}
	return f.ingest(ctx, a.RelPath, dest)
}

func (f *Fetcher) ingest(ctx context.Context, relPath, absPath string) error {
	if f.store.ingester == nil {
		return nil
	}
	if err := f.store.ingester.Ingest(ctx, relPath, absPath); err != nil {
		return fmt.Errorf("ingest %s: %w", relPath, err)
	}
	return nil
}

// transfer runs one attempt, resuming from whatever is already in the part file.
func (f *Fetcher) transfer(ctx context.Context, a node.Assignment, part string) error {
	var resumeFrom int64
	if info, err := os.Stat(part); err == nil {
		resumeFrom = info.Size()
	}

	// Each path segment is escaped separately: the separators are structure and
	// must survive, while anything inside a segment is data and must not be
	// able to introduce one.
	segments := strings.Split(a.RelPath, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	endpoint := f.server + "/api/v1/repo/file/" + strings.Join(segments, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", a.RelPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// Range was ignored, so what follows starts at byte zero and whatever
		// is on disk is not a prefix of it.
		resumeFrom = 0
	case http.StatusPartialContent:
	case http.StatusRequestedRangeNotSatisfiable:
		// Already complete from a previous attempt that was interrupted before
		// it could verify.
		return nil
	default:
		return fmt.Errorf("fetch %s: server returned %s", a.RelPath, resp.Status)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if resumeFrom > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return fmt.Errorf("open partial download: %w", err)
	}

	done := resumeFrom
	buf := make([]byte, fetchBuffer)
	var copyErr error
	last := time.Now()
	for {
		if copyErr = ctx.Err(); copyErr != nil {
			break
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := file.Write(buf[:n]); writeErr != nil {
				copyErr = writeErr
				break
			}
			done += int64(n)
			if f.Progress != nil && time.Since(last) > 2*time.Second {
				last = time.Now()
				f.Progress(a.RelPath, done, a.SizeBytes)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				copyErr = readErr
			}
			break
		}
	}

	// Flush before calling the attempt good: a power cut between here and the
	// rename must not leave a file the store believes in and the disk never got.
	syncErr := file.Sync()
	closeErr := file.Close()
	switch {
	case copyErr != nil:
		return fmt.Errorf("fetch %s: %w", a.RelPath, copyErr)
	case syncErr != nil:
		return fmt.Errorf("flush %s: %w", a.RelPath, syncErr)
	case closeErr != nil:
		return fmt.Errorf("close %s: %w", a.RelPath, closeErr)
	}
	if a.SizeBytes > 0 && done != a.SizeBytes {
		return fmt.Errorf("fetch %s ended early: %d of %d bytes", a.RelPath, done, a.SizeBytes)
	}
	return nil
}

// verify checks the transferred file against what the assignment promised.
//
// The digest when there is one; the length otherwise. A repository file whose
// digest was never published — Hugging Face only publishes one for LFS-stored
// files — is checked by size alone, and that limitation is inherited honestly
// rather than papered over with a hash of nothing.
func (f *Fetcher) verify(part string, a node.Assignment) error {
	info, err := os.Stat(part)
	if err != nil {
		return fmt.Errorf("verify %s: %w", a.RelPath, err)
	}
	if a.SizeBytes > 0 && info.Size() != a.SizeBytes {
		return fmt.Errorf("%s is %d bytes, expected %d", a.RelPath, info.Size(), a.SizeBytes)
	}
	if a.SHA256 == "" {
		return nil
	}

	sum, err := fileDigest(part)
	if err != nil {
		return fmt.Errorf("verify %s: %w", a.RelPath, err)
	}
	if !strings.EqualFold(sum, a.SHA256) {
		// Discarded, for the same reason the repository discards a corrupt
		// partial: a resume is only correct when what is on disk is a prefix of
		// the real file, and a mismatch proves it is not. Keeping it would make
		// every later attempt resume onto bad bytes and fail identically.
		if rmErr := os.Remove(part); rmErr != nil {
			return fmt.Errorf("%s failed its digest check and could not be removed: %w",
				a.RelPath, rmErr)
		}
		return fmt.Errorf("%s does not match the digest the server published "+
			"(expected %s, got %s); the partial file was discarded so a retry starts clean",
			a.RelPath, a.SHA256, sum)
	}
	return nil
}

// fileDigest hashes a file on disk.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
