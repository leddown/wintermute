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

// Fetcher pulls assignments into the store, from a mounted copy of the
// server's repository when this node has one and over HTTP otherwise.
type Fetcher struct {
	store  *Store
	server string
	token  string
	client *http.Client
	// RepoMount is a local, usually read-only, mount of the server's model
	// repository — a Samba or NFS export of the same directory the HTTP
	// endpoint serves. When set, an assignment found there is copied instead of
	// downloaded, which on a LAN is several times faster than the same bytes
	// over HTTP and does not occupy the server's own transfer path.
	//
	// It is an optimisation and never a requirement: an unset mount, one whose
	// disk is not mounted, one missing this particular file, and one whose copy
	// fails its digest all fall back to the server. That is what lets a new
	// node work before anyone has configured a share, and lets an existing node
	// carry on when the share is down.
	RepoMount string
	// Progress is called as bytes arrive, for the agent's log. Optional.
	Progress func(relPath string, done, total int64)
	// Notice reports something worth an operator's attention that did not stop
	// the fetch — a mount that was there and could not be used. Optional, and
	// deliberately separate from the returned error: falling back to HTTP
	// succeeds, and a silent success would leave a broken share undiagnosed
	// behind transfers that merely got slower.
	Notice func(relPath, msg string)
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

	// The mount first, when there is one holding this file. A failure here is
	// not fatal: the partial is discarded so nothing resumes onto bytes of
	// unknown provenance, and the server is asked for the same file.
	if src := f.MountSource(a); src != "" {
		err := f.copyFromMount(ctx, src, part, a)
		if err == nil {
			return f.install(ctx, a, part, dest)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = os.Remove(part)
		if f.Notice != nil {
			f.Notice(a.RelPath, fmt.Sprintf(
				"%s could not be used (%v); falling back to the server", src, err))
		}
	}

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
	return f.install(ctx, a, part, dest)
}

// install puts a verified partial in its place and makes it servable. Both
// sources end here, so a copy from the mount is indistinguishable afterwards
// from a download — the same name, the same import, the same report.
func (f *Fetcher) install(ctx context.Context, a node.Assignment, part, dest string) error {
	if err := os.Rename(part, dest); err != nil {
		return fmt.Errorf("finalise %s: %w", a.RelPath, err)
	}
	return f.ingest(ctx, a.RelPath, dest)
}

// MountSource returns the file the repository mount holds for this assignment,
// or "" when there is nothing to copy.
//
// Empty covers every ordinary reason a node has no local copy: no mount
// configured, the disk not mounted, the share up but without this model, or a
// name that does not resolve inside the mount. All of them mean "ask the
// server", which is why none of them is an error.
//
// The name comes from the server, so it is confined exactly as it is for the
// store: the mount is a second directory an assignment can name, and a
// traversal out of it would read whatever the node can see.
func (f *Fetcher) MountSource(a node.Assignment) string {
	if strings.TrimSpace(f.RepoMount) == "" {
		return ""
	}
	src, err := resolveUnder(f.RepoMount, a.RelPath)
	if err != nil {
		return ""
	}
	info, err := os.Stat(src)
	if err != nil || info.IsDir() {
		return ""
	}
	return src
}

// copyFromMount copies the file out of the mount and verifies it.
//
// The copy is checked against the assignment exactly as a download is, and for
// a better reason: a share is a directory some other process writes, so what is
// there may be a half-written file, a different quantisation with the same
// name, or a model the server has since replaced. Trusting it because it is
// local would put weights on this node that the server never published.
func (f *Fetcher) copyFromMount(ctx context.Context, src, part string, a node.Assignment) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	// Truncated rather than resumed. A partial from an interrupted HTTP attempt
	// is a prefix of the server's file, which says nothing about this one, and
	// a local copy is fast enough that starting over costs little.
	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open partial copy: %w", err)
	}
	_, copyErr := f.stream(ctx, out, in, a, 0)
	syncErr := out.Sync()
	closeErr := out.Close()
	switch {
	case copyErr != nil:
		return copyErr
	case syncErr != nil:
		return fmt.Errorf("flush %s: %w", a.RelPath, syncErr)
	case closeErr != nil:
		return fmt.Errorf("close %s: %w", a.RelPath, closeErr)
	}
	return f.verify(part, a)
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

	done, copyErr := f.stream(ctx, file, resp.Body, a, resumeFrom)

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

// stream copies src into file, reporting progress and stopping on
// cancellation. Shared by both sources so a copy from the mount is watched the
// same way a download is: these are the transfers an operator sits through.
func (f *Fetcher) stream(ctx context.Context, file *os.File, src io.Reader, a node.Assignment, done int64) (int64, error) {
	buf := make([]byte, fetchBuffer)
	last := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := file.Write(buf[:n]); writeErr != nil {
				return done, writeErr
			}
			done += int64(n)
			if f.Progress != nil && time.Since(last) > 2*time.Second {
				last = time.Now()
				f.Progress(a.RelPath, done, a.SizeBytes)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return done, nil
			}
			return done, readErr
		}
	}
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
