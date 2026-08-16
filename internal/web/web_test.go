package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The gzipped body must decompress to exactly what the identity response
// returns. A compression layer that changes the bytes is worse than none.
func TestGzipMatchesIdentity(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/", "/app.js", "/style.css", "/index.html"} {
		plain := get(t, h, path, nil)
		if plain.Code != http.StatusOK {
			t.Fatalf("%s: identity status %d", path, plain.Code)
		}
		if enc := plain.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("%s: identity response was encoded as %q", path, enc)
		}

		zipped := get(t, h, path, map[string]string{"Accept-Encoding": "gzip"})
		if zipped.Code != http.StatusOK {
			t.Fatalf("%s: gzip status %d", path, zipped.Code)
		}
		if zipped.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s: expected a gzip response, got %q",
				path, zipped.Header().Get("Content-Encoding"))
		}

		// Sizes are taken before anything reads the buffers: decompressing
		// through zipped.Body consumes it, which would leave the assertions
		// below comparing against zero and passing for the wrong reason.
		plainBody := plain.Body.String()
		gzBytes := zipped.Body.Bytes()
		gzLen := len(gzBytes)

		zr, err := gzip.NewReader(bytes.NewReader(gzBytes))
		if err != nil {
			t.Fatalf("%s: not valid gzip: %v", path, err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("%s: decompress: %v", path, err)
		}
		if string(got) != plainBody {
			t.Errorf("%s: decompressed body differs from the identity body", path)
		}
		if gzLen >= len(plainBody) {
			t.Errorf("%s: gzip (%d) did not beat identity (%d)", path, gzLen, len(plainBody))
		}
		// A wrong Content-Length is worse than no compression: the client
		// either truncates the body or waits for bytes that never arrive.
		if cl := zipped.Header().Get("Content-Length"); cl != strconv.Itoa(gzLen) {
			t.Errorf("%s: Content-Length %s but wrote %d bytes", path, cl, gzLen)
		}
		t.Logf("%s: %d -> %d bytes (%.0f%% smaller)",
			path, len(plainBody), gzLen, 100*(1-float64(gzLen)/float64(len(plainBody))))
		if v := zipped.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
			t.Errorf("%s: Vary is %q, must name Accept-Encoding", path, v)
		}
	}
}

// "gzip;q=0" means the client does not want it.
func TestGzipRefused(t *testing.T) {
	h := Handler()
	for _, header := range []string{"", "identity", "gzip;q=0", "br"} {
		rec := get(t, h, "/app.js", map[string]string{"Accept-Encoding": header})
		if enc := rec.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("Accept-Encoding %q: got encoding %q, want none", header, enc)
		}
	}
}

func TestContentTypes(t *testing.T) {
	h := Handler()
	for path, want := range map[string]string{
		"/":          "text/html",
		"/app.js":    "javascript",
		"/style.css": "text/css",
	} {
		got := get(t, h, path, nil).Header().Get("Content-Type")
		if !strings.Contains(got, want) {
			t.Errorf("%s: Content-Type %q, want it to contain %q", path, got, want)
		}
	}
}

// A returning browser should get 304 rather than the body again, in either
// encoding.
func TestETagRoundTrip(t *testing.T) {
	h := Handler()
	first := get(t, h, "/app.js", map[string]string{"Accept-Encoding": "gzip"})
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the first response")
	}
	for _, sent := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		rec := get(t, h, "/app.js", map[string]string{"If-None-Match": sent})
		if rec.Code != http.StatusNotModified {
			t.Errorf("If-None-Match %q: status %d, want 304", sent, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("If-None-Match %q: 304 carried %d bytes", sent, rec.Body.Len())
		}
	}
	if rec := get(t, h, "/app.js", map[string]string{"If-None-Match": `"stale"`}); rec.Code != http.StatusOK {
		t.Errorf("a stale ETag should get 200, got %d", rec.Code)
	}
}

func TestMissingFileStillFallsThrough(t *testing.T) {
	if rec := get(t, Handler(), "/does-not-exist.js", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
}

// HEAD must carry the headers of the GET without the body, or a client sizing
// a request from it gets the wrong answer.
func TestHeadHasHeadersButNoBody(t *testing.T) {
	h := Handler()
	req := httptest.NewRequest(http.MethodHead, "/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned %d bytes of body", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") == "" || rec.Header().Get("ETag") == "" {
		t.Error("HEAD dropped Content-Length or ETag")
	}
}
