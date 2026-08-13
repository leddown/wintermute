package models

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"wintermute/internal/store"
)

// hardwareTTL is how long a hardware probe is reused. Free VRAM moves as
// models load and unload, so it is short — but shelling out to nvidia-smi on
// every request would be wasteful when the UI polls.
const hardwareTTL = 15 * time.Second

// Catalog is the server's view of the model landscape: which backends exist,
// what they serve, and what the host can run.
//
// It owns no inference and mutates nothing outside its own cache tables.
type Catalog struct {
	backends []Backend
	prober   *Prober
	hub      *Hub
	store    *store.Store
	log      *slog.Logger

	mu       sync.Mutex
	hardware *Hardware
	probedAt time.Time
}

// NewCatalog builds a Catalog over the configured backends.
func NewCatalog(backends []Backend, st *store.Store, hub *Hub, log *slog.Logger) *Catalog {
	return &Catalog{
		backends: backends,
		prober:   NewProber(),
		hub:      hub,
		store:    st,
		log:      log,
	}
}

// Backends returns the configured backends.
func (c *Catalog) Backends() []Backend { return c.backends }

// Hub exposes the Hugging Face client for discovery endpoints.
func (c *Catalog) Hub() *Hub { return c.hub }

// Hardware returns the host profile, re-probing when the cache has expired.
func (c *Catalog) Hardware(ctx context.Context) *Hardware {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hardware != nil && time.Since(c.probedAt) < hardwareTTL {
		return c.hardware
	}
	c.hardware = DetectHardware(ctx)
	c.hardware.RunsInference = runsInferenceLocally(c.backends)
	if !c.hardware.RunsInference {
		c.hardware.Warnings = append(c.hardware.Warnings,
			"No configured backend runs on this host, so these figures describe the "+
				"machine serving the API rather than the one running the models. VRAM "+
				"fit estimates are reported as unknown rather than computed from the "+
				"wrong hardware.")
	}
	c.probedAt = time.Now()
	return c.hardware
}

// runsInferenceLocally reports whether any non-cloud backend is served from
// this host, which is what makes the local hardware profile evidence about
// anything.
//
// The test is loopback, not "reachable" or "same subnet": a backend addressed
// as localhost is unambiguously here, and one addressed by hostname or IP may
// or may not be — resolving that would mean comparing against every local
// interface, with DNS in the path, to answer a question whose wrong answer is
// silent. A host that serves its own models through its external name is
// therefore read as remote, and the symptom is fit estimates going unknown
// rather than going wrong. Point the backend at localhost to get them back.
func runsInferenceLocally(backends []Backend) bool {
	for _, b := range backends {
		// A cloud backend runs on somebody else's hardware by definition, so it
		// is not evidence either way.
		if b.Cloud {
			continue
		}
		if isLoopbackURL(b.BaseURL) {
			return true
		}
	}
	return false
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		// No scheme, so url.Parse put everything in Path. Try again with one.
		if u2, err2 := url.Parse("http://" + strings.TrimSpace(raw)); err2 == nil {
			host = u2.Hostname()
		}
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Refresh probes every backend and replaces the cached catalog.
//
// One backend being down never fails the refresh: its health is recorded and
// the others are still updated. A local inference server that is not running
// is an ordinary state to be displayed, not an error.
func (c *Catalog) Refresh(ctx context.Context) error {
	for _, b := range c.backends {
		row := store.BackendRow{
			Name:    b.Name,
			Kind:    string(b.Kind),
			BaseURL: b.BaseURL,
			Model:   b.Model,
			Cloud:   b.Cloud,
		}
		now := time.Now().UTC()
		row.ProbedAt = &now

		found, err := c.prober.Probe(ctx, b)
		if err != nil {
			row.Status = store.BackendUnreachable
			row.StatusNote = err.Error()
			c.log.Warn("backend probe failed", "backend", b.Name, "kind", b.Kind, "error", err)
			if err := c.store.UpsertBackend(ctx, row); err != nil {
				return err
			}
			continue
		}

		row.Status = store.BackendOK
		if err := c.store.UpsertBackend(ctx, row); err != nil {
			return err
		}

		rows := make([]store.CatalogRow, 0, len(found))
		for _, m := range found {
			caps := make([]string, 0, len(m.Capabilities))
			for _, cap := range m.Capabilities {
				caps = append(caps, string(cap))
			}
			rows = append(rows, store.CatalogRow{
				Backend:      b.Name,
				ModelID:      m.ID,
				Family:       m.Family,
				ParamsB:      m.ParamsB,
				Quant:        m.Quant,
				CtxLen:       m.CtxLen,
				SizeBytes:    m.SizeBytes,
				Capabilities: caps,
				Loaded:       m.Loaded,
				VRAMBytes:    m.VRAMBytes,
			})
		}
		if err := c.store.ReplaceCatalog(ctx, b.Name, rows); err != nil {
			return err
		}
	}
	return nil
}

// Models returns the cached catalog with a fit estimate attached to each entry.
//
// Fit is computed at read time rather than stored because free VRAM changes
// whenever a model loads — a verdict cached an hour ago would be a lie.
func (c *Catalog) Models(ctx context.Context, contextTokens int) ([]Model, error) {
	rows, err := c.store.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	if contextTokens <= 0 {
		contextTokens = 8192
	}
	hw := c.Hardware(ctx)

	cloud := map[string]bool{}
	for _, b := range c.backends {
		cloud[b.Name] = b.Cloud
	}

	out := make([]Model, 0, len(rows))
	for _, r := range rows {
		m := Model{
			Backend:    r.Backend,
			ID:         r.ModelID,
			Family:     r.Family,
			ParamsB:    r.ParamsB,
			Quant:      r.Quant,
			CtxLen:     r.CtxLen,
			SizeBytes:  r.SizeBytes,
			Loaded:     r.Loaded,
			VRAMBytes:  r.VRAMBytes,
			LastSeenAt: r.LastSeenAt,
		}
		for _, cap := range r.Capabilities {
			m.Capabilities = append(m.Capabilities, Capability(cap))
		}

		// A cloud model runs on someone else's hardware, so a local VRAM
		// verdict would be meaningless — leave Fit nil rather than wrong.
		if !cloud[r.Backend] && m.ParamsB > 0 {
			quant := m.Quant
			if quant == "" {
				quant = DefaultQuant
			}
			ctxLen := m.CtxLen
			if ctxLen == 0 || ctxLen > contextTokens {
				ctxLen = contextTokens
			}
			fit := EstimateFit(FitInput{
				ParamsB:       m.ParamsB,
				Quant:         quant,
				ContextTokens: ctxLen,
			}, hw)
			m.Fit = &fit
		}
		out = append(out, m)
	}
	return out, nil
}

// BackendHealth returns the stored health of every backend.
func (c *Catalog) BackendHealth(ctx context.Context) ([]store.BackendRow, error) {
	return c.store.Backends(ctx)
}

// Recommend answers a planning request against live hardware and the models
// already installed.
func (c *Catalog) Recommend(ctx context.Context, req PlanRequest) (*Plan, error) {
	installed, err := c.Models(ctx, req.ContextTokens)
	if err != nil {
		return nil, err
	}
	if req.RequireLocal {
		cloud := map[string]bool{}
		for _, b := range c.backends {
			cloud[b.Name] = b.Cloud
		}
		filtered := installed[:0]
		for _, m := range installed {
			if !cloud[m.Backend] {
				filtered = append(filtered, m)
			}
		}
		installed = filtered
	}
	return Recommend(req, c.Hardware(ctx), installed), nil
}
