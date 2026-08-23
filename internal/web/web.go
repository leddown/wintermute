// Package web serves the embedded browser UI. The assets are compiled into
// the binary so deploying the server is a single file copy.
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed static
var assets embed.FS

// The UI is about 200KB of HTML, CSS and JavaScript, and it is the same 200KB
// on every cold load. Serving it uncompressed is most of the cost of opening
// the app on a phone.
//
// It is compressed once, when the handler is built, rather than per request.
// The assets are embedded and therefore immutable, so a response body computed
// at startup stays correct for the life of the process: every later request is
// a write of bytes that already exist, with no compressor allocated, no CPU
// spent re-deflating the same file, and no garbage produced per hit. The cost
// is one pass at startup and roughly 50KB held for the process's lifetime —
// bounded by what is embedded, so it cannot grow.
//
// Compressing on the fly would have inverted that: cheaper to start, then a
// fresh gzip.Writer and its window buffer for every request of every file.

// compressible lists the extensions worth deflating. Anything already
// compressed (png, ico, woff2) only grows, so it is served as it is.
var compressible = map[string]bool{
	".html": true, ".css": true, ".js": true, ".json": true,
	".svg": true, ".txt": true, ".map": true, ".webmanifest": true,
	// The FAQ is served as Markdown and rendered in the browser, so it is
	// text like the rest and compresses like the rest.
	".md": true,
}

// minCompressBytes is the size below which compression is not worth it: the
// gzip header and trailer are 18 bytes before any data, and a tiny file often
// comes out larger than it went in.
const minCompressBytes = 512

// asset is one file, ready to write in either encoding.
type asset struct {
	contentType string
	plain       []byte
	// gzipped is nil when the file was not worth compressing, or when the
	// result was not actually smaller.
	gzipped []byte
	// etag identifies the content, so a returning browser gets a 304 instead
	// of the body again. It is derived from the bytes rather than a modtime:
	// embedded files all carry the zero time, which is why http.FileServer
	// cannot do this for them.
	etag string
}

// Handler serves the UI at the root path.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		// Only reachable if the embed directive and this path disagree,
		// which is a build-time mistake.
		panic("web: " + err.Error())
	}
	return &staticHandler{files: load(sub), fallback: http.FileServer(http.FS(sub))}
}

// load reads and compresses every embedded file once.
func load(fsys fs.FS) map[string]*asset {
	out := map[string]*asset{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		ext := strings.ToLower(path.Ext(p))
		ctype := mime.TypeByExtension(ext)
		if ctype == "" {
			ctype = http.DetectContentType(raw)
		}
		sum := sha256.Sum256(raw)
		a := &asset{
			contentType: ctype,
			plain:       raw,
			etag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
		}
		if compressible[ext] && len(raw) >= minCompressBytes {
			// Best compression: this runs once, and the result is written
			// many times, so the slowest setting is also the cheapest one.
			var buf bytes.Buffer
			zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			if err != nil {
				return err
			}
			if _, err := zw.Write(raw); err != nil {
				return err
			}
			if err := zw.Close(); err != nil {
				return err
			}
			// A file that did not actually shrink is served as it is, rather
			// than paying the encoding round trip to send more bytes.
			if buf.Len() < len(raw) {
				a.gzipped = buf.Bytes()
			}
		}
		out[p] = a
		return nil
	})
	if err != nil {
		panic("web: loading assets: " + err.Error())
	}
	return out
}

type staticHandler struct {
	files map[string]*asset
	// fallback serves anything load() did not capture, and keeps directory
	// redirects and range requests behaving as they did.
	fallback http.Handler
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.fallback.ServeHTTP(w, r)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	a := h.files[name]
	if a == nil {
		h.fallback.ServeHTTP(w, r)
		return
	}
	// Ranges are served from the original file. Nothing here is big enough to
	// be fetched in pieces, and a partial response over a compressed body is
	// a different contract than the one the fallback already implements.
	if r.Header.Get("Range") != "" {
		h.fallback.ServeHTTP(w, r)
		return
	}

	// Always, even on the identity response: a cache that stored one encoding
	// must not hand it to a client that asked for the other.
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("ETag", a.etag)
	w.Header().Set("Content-Type", a.contentType)

	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := a.plain
	if a.gzipped != nil && acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		body = a.gzipped
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(body); err != nil {
		// The client went away mid-write. Nothing to recover, and nothing
		// worth logging at this level.
		_ = err
	}
}

// acceptsGzip reports whether the client will take a gzipped body. It looks
// for the token rather than the substring, so an "Accept-Encoding: gzip;q=0"
// — which means "do not send me gzip" — is honoured.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(token), "gzip") {
			continue
		}
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") && strings.TrimSpace(v) == "0" {
				return false
			}
		}
		return true
	}
	return false
}

// etagMatches handles the comma-separated list If-None-Match may carry, and
// the weak prefix a proxy may have added.
func etagMatches(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		c := strings.TrimSpace(candidate)
		c = strings.TrimPrefix(c, "W/")
		if c == etag {
			return true
		}
	}
	return false
}

// compile-time assertion that the handler still satisfies the interface the
// server wires it into.
var _ http.Handler = (*staticHandler)(nil)
