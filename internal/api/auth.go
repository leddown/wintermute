package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"wintermute/internal/store"
)

type contextKey struct{ name string }

var clientContextKey = contextKey{"client"}

// clientFrom returns the authenticated client. It panics if called from an
// unauthenticated handler, which is a wiring bug, not a runtime condition.
func clientFrom(ctx context.Context) *store.Client {
	c, ok := ctx.Value(clientContextKey).(*store.Client)
	if !ok {
		panic("api: no authenticated client in context")
	}
	return c
}

// authenticate resolves a bearer token to a registered client. Tokens are
// issued by `wintermuted -add-client` and stored hashed.
func (s *Server) authenticate(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wintermute"`)
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}

		client, err := s.store.ClientByToken(r.Context(), token)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if err != nil {
			s.fail(w, "authenticate", err)
			return
		}

		// Best-effort; a failed touch must not block the request.
		if err := s.store.TouchClient(r.Context(), client.ID); err != nil {
			s.log.Warn("touch client failed", "client", client.Name, "error", err)
		}

		ctx := context.WithValue(r.Context(), clientContextKey, client)
		next(w, r.WithContext(ctx))
	})
}

// bearerToken extracts the token from the Authorization header, falling back
// to a cookie so the browser UI can authenticate without scripting the header
// onto every request.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if token, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
	}
	if c, err := r.Cookie("wintermute_token"); err == nil {
		return c.Value
	}
	return ""
}

// logRequests emits one line per request at debug level, and warns on 5xx.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start)}
		if rec.status >= 500 {
			s.log.Warn("request failed", attrs...)
			return
		}
		s.log.Debug("request", attrs...)
	})
}

// recoverPanic keeps one bad turn from taking the server down.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}
