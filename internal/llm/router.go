package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// ErrNoBackend is returned when a named backend is not configured.
var ErrNoBackend = errors.New("backend not configured")

// Backend is one named model source the router can send a turn to.
type Backend struct {
	// Name is how a session refers to this backend, e.g. "local" or "claude".
	Name string
	// Provider speaks to it.
	Provider Provider
	// Model is the default model for this backend. A session may override it.
	Model string
	// Cloud marks a backend that sends the transcript off the local network.
	// The router never selects a cloud backend implicitly except as the
	// configured fallback, and every such use is reported to the caller.
	Cloud bool
}

// Complete runs one request against this backend, supplying its default model
// when the request names none.
func (b *Backend) Complete(ctx context.Context, req Request) (*Response, error) {
	if req.Model == "" {
		req.Model = b.Model
	}
	return b.Provider.Complete(ctx, req)
}

// Router dispatches a turn to one of several backends.
//
// The design goal is that open-weight models served on the LAN are the normal
// path, and a cloud model is available deliberately — chosen per session, or
// reached automatically only when the local backend has actually failed. A
// fallback is never silent: Result reports which backend answered and why, so
// the UI can show that a turn left the network.
type Router struct {
	backends map[string]*Backend
	order    []string
	def      string
	fallback string
	log      *slog.Logger
}

// NewRouter builds a router. def names the default backend; fallback names an
// optional backend to retry against when the selected one fails, and may be
// empty. Both must exist among backends.
func NewRouter(backends []*Backend, def, fallback string, log *slog.Logger) (*Router, error) {
	if len(backends) == 0 {
		return nil, errors.New("router: no backends configured")
	}

	r := &Router{
		backends: make(map[string]*Backend, len(backends)),
		log:      log,
	}
	for _, b := range backends {
		if b.Name == "" {
			return nil, errors.New("router: backend has no name")
		}
		if _, dup := r.backends[b.Name]; dup {
			return nil, fmt.Errorf("router: duplicate backend %q", b.Name)
		}
		r.backends[b.Name] = b
		r.order = append(r.order, b.Name)
	}
	sort.Strings(r.order)

	if def == "" {
		def = r.order[0]
	}
	if _, ok := r.backends[def]; !ok {
		return nil, fmt.Errorf("router: default backend %q: %w", def, ErrNoBackend)
	}
	if fallback != "" {
		if _, ok := r.backends[fallback]; !ok {
			return nil, fmt.Errorf("router: fallback backend %q: %w", fallback, ErrNoBackend)
		}
	}
	r.def = def
	r.fallback = fallback
	return r, nil
}

// Default reports the backend used when a session names none.
func (r *Router) Default() string { return r.def }

// Fallback reports the backend used when the selected one fails, if any.
func (r *Router) Fallback() string { return r.fallback }

// Names lists configured backends in a stable order.
func (r *Router) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Backend looks up one backend by name.
func (r *Router) Backend(name string) (*Backend, bool) {
	if name == "" {
		name = r.def
	}
	b, ok := r.backends[name]
	return b, ok
}

// Result is one completed turn, annotated with where it was served.
type Result struct {
	*Response
	// Backend and Model record what actually answered — which is not always
	// what was asked for, when a fallback fired.
	Backend string
	Model   string
	// FellBackFrom names the backend that failed, empty when none did.
	FellBackFrom string
	// FallbackReason carries the original failure, for display and logs.
	FallbackReason string
}

// Complete runs one turn against the named backend, falling back if it fails.
//
// backend may be empty for the default. req.Model overrides the backend's
// default model. A cancelled context is never retried: that is the user
// stopping the turn, not the backend failing.
func (r *Router) Complete(ctx context.Context, backend string, req Request) (*Result, error) {
	b, ok := r.Backend(backend)
	if !ok {
		return nil, fmt.Errorf("%q: %w", backend, ErrNoBackend)
	}

	res, err := r.complete(ctx, b, req)
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}
	if r.fallback == "" || r.fallback == b.Name {
		return nil, err
	}

	fb := r.backends[r.fallback]
	r.log.Warn("backend failed, falling back",
		"from", b.Name, "to", fb.Name, "cloud", fb.Cloud, "error", err)

	// The fallback serves different models, so the caller's model override is
	// dropped — keeping it would name a model the fallback does not have.
	fbReq := req
	fbReq.Model = ""

	res, fbErr := r.complete(ctx, fb, fbReq)
	if fbErr != nil {
		return nil, fmt.Errorf("%s failed (%v); fallback %s also failed: %w", b.Name, err, fb.Name, fbErr)
	}
	res.FellBackFrom = b.Name
	res.FallbackReason = trimError(err)
	return res, nil
}

// CompleteOn runs one turn against exactly the named backend, with no
// fallback.
//
// This is for testing a backend rather than getting an answer: Complete's
// fallback is what you want in a conversation and precisely what you do not
// want here, because a broken backend would report someone else's success.
func (r *Router) CompleteOn(ctx context.Context, backend string, req Request) (*Result, error) {
	b, ok := r.Backend(backend)
	if !ok {
		return nil, fmt.Errorf("%q: %w", backend, ErrNoBackend)
	}
	return r.complete(ctx, b, req)
}

func (r *Router) complete(ctx context.Context, b *Backend, req Request) (*Result, error) {
	if req.Model == "" {
		req.Model = b.Model
	}
	resp, err := b.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return &Result{Response: resp, Backend: b.Name, Model: req.Model}, nil
}

// trimError keeps a failure short enough to show in a UI badge.
func trimError(err error) string {
	const max = 200
	s := strings.TrimSpace(err.Error())
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
