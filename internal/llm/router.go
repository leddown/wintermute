package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
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
//
// The backend set is not fixed for the process's lifetime: backends can also
// be declared in the UI, and Replace swaps the whole set in when one is added
// or removed. The mutex guards that swap against turns already in flight —
// every read below takes it, and Backend hands out a *Backend that a caller
// then uses without the lock, which is safe because a replaced entry is
// discarded rather than mutated.
type Router struct {
	mu       sync.RWMutex
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
	r := &Router{log: log}
	if err := r.Replace(backends, def, fallback); err != nil {
		return nil, err
	}
	return r, nil
}

// Replace swaps in a new backend set, default and fallback.
//
// It validates the whole set before touching anything, so a rejected change
// leaves the router serving exactly what it served before: adding a backend
// with a bad name must not be able to take the working ones down with it.
func (r *Router) Replace(backends []*Backend, def, fallback string) error {
	if len(backends) == 0 {
		return errors.New("router: no backends configured")
	}

	next := make(map[string]*Backend, len(backends))
	order := make([]string, 0, len(backends))
	for _, b := range backends {
		if b.Name == "" {
			return errors.New("router: backend has no name")
		}
		if _, dup := next[b.Name]; dup {
			return fmt.Errorf("router: duplicate backend %q", b.Name)
		}
		next[b.Name] = b
		order = append(order, b.Name)
	}
	sort.Strings(order)

	if def == "" {
		def = order[0]
	}
	if _, ok := next[def]; !ok {
		return fmt.Errorf("router: default backend %q: %w", def, ErrNoBackend)
	}
	if fallback != "" {
		if _, ok := next[fallback]; !ok {
			return fmt.Errorf("router: fallback backend %q: %w", fallback, ErrNoBackend)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends, r.order, r.def, r.fallback = next, order, def, fallback
	return nil
}

// Default reports the backend used when a session names none.
func (r *Router) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.def
}

// Fallback reports the backend used when the selected one fails, if any.
func (r *Router) Fallback() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fallback
}

// Names lists configured backends in a stable order.
func (r *Router) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Backend looks up one backend by name. An empty name means the default.
func (r *Router) Backend(name string) (*Backend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	// Resolved through the guarded accessors rather than the fields, so a
	// Replace running concurrently with this turn cannot be read half-applied.
	fallback := r.Fallback()
	if fallback == "" || fallback == b.Name {
		return nil, err
	}
	fb, ok := r.Backend(fallback)
	if !ok {
		// Undeclared between the two lookups. Report the real failure rather
		// than inventing one about the fallback.
		return nil, err
	}
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
