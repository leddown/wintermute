package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Loading and unloading models on a backend.
//
// This needs no agent on the remote host. Ollama's own HTTP API — the one the
// prober already reads inventory and residency from — takes a request that
// puts a model in memory and a request that evicts it. A machine that is
// already serving models is already reachable; asking it to hold a different
// one is the same conversation.
//
// It is deliberately not offered to the model as a tool. Every server-side tool
// in this program is read-only by construction, which is what lets the agent
// loop auto-approve them without asking anyone (see runServerTool). Evicting a
// model is not read-only: it can take VRAM out from under a turn another
// conversation is in the middle of. Giving the assistant that power is a
// separate decision from giving the operator a button, and it should be made on
// purpose rather than inherited from this change.

// ErrControlUnsupported reports that a backend cannot load or unload on demand.
var ErrControlUnsupported = errors.New("this backend cannot load or unload models on demand")

// controlTimeout bounds one load or unload.
//
// Generous, unlike probeTimeout: loading a 24B model off a spinning disk is
// tens of seconds of real work, and timing out halfway leaves the operator
// staring at an error while the backend carries on loading anyway.
const controlTimeout = 5 * time.Minute

// Controller loads and unloads models on backends that support it.
type Controller struct {
	client *http.Client
}

// NewController builds a Controller.
func NewController() *Controller {
	return &Controller{client: &http.Client{Timeout: controlTimeout}}
}

// Supports reports whether a backend can be controlled.
//
// Ollama and hailo-ollama can. llama.cpp serves whatever it was started with
// and would need its process restarting, or llama-swap in front of it. vLLM is
// one model per process. Anthropic is someone else's hardware. Saying so up
// front is better than offering a button that fails.
func (c *Controller) Supports(b Backend) bool {
	switch b.Kind {
	case KindOllama, KindHailo:
		return true
	default:
		return false
	}
}

// Load puts a model in memory and keeps it there.
//
// keepAlive is how long it stays resident with no requests. Zero means the
// backend's own default, which for Ollama is five minutes; a negative duration
// pins it indefinitely, which is what an operator loading a model deliberately
// almost always means.
func (c *Controller) Load(ctx context.Context, b Backend, modelID string, keepAlive time.Duration) error {
	if !c.Supports(b) {
		return fmt.Errorf("%s: %w", b.Name, ErrControlUnsupported)
	}
	if strings.TrimSpace(modelID) == "" {
		return errors.New("a model id is required")
	}

	// An empty prompt with a model name is Ollama's documented way to preload:
	// it loads the weights and returns without generating anything.
	body := map[string]any{"model": modelID}
	if keepAlive != 0 {
		body["keep_alive"] = keepAliveValue(keepAlive)
	}
	return c.post(ctx, b, apiRoot(b.BaseURL)+"/api/generate", body)
}

// Unload evicts a model from memory.
//
// The same endpoint with keep_alive of zero and no prompt, which Ollama
// documents as the way to release the weights immediately rather than waiting
// out the idle timer.
func (c *Controller) Unload(ctx context.Context, b Backend, modelID string) error {
	if !c.Supports(b) {
		return fmt.Errorf("%s: %w", b.Name, ErrControlUnsupported)
	}
	if strings.TrimSpace(modelID) == "" {
		return errors.New("a model id is required")
	}
	return c.post(ctx, b, apiRoot(b.BaseURL)+"/api/generate",
		map[string]any{"model": modelID, "keep_alive": 0})
}

// Resident is one model currently held in memory on a backend.
type Resident struct {
	Backend string `json:"backend"`
	ModelID string `json:"model_id"`
	// VRAMBytes is what it is actually costing right now, which is the number
	// that decides whether anything else will fit.
	VRAMBytes int64 `json:"vram_bytes"`
	// ExpiresAt is when the backend will evict it on its own. Zero means it is
	// pinned, or the backend did not say.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type ollamaPSDetailed struct {
	Models []struct {
		Name      string `json:"name"`
		SizeVRAM  int64  `json:"size_vram"`
		ExpiresAt string `json:"expires_at"`
	} `json:"models"`
}

// Resident lists what a backend is holding in memory right now.
//
// This is live state, read on demand rather than from the catalog: the catalog
// is refreshed on a probe interval, and after loading a model the operator
// wants to see it appear now, not on the next sweep.
func (c *Controller) Resident(ctx context.Context, b Backend) ([]Resident, error) {
	if !c.Supports(b) {
		return nil, fmt.Errorf("%s: %w", b.Name, ErrControlUnsupported)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiRoot(b.BaseURL)+"/api/ps", nil)
	if err != nil {
		return nil, err
	}
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", b.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: /api/ps: %s", b.Name, resp.Status)
	}

	var ps ollamaPSDetailed
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProbeBytes)).Decode(&ps); err != nil {
		return nil, fmt.Errorf("%s: decode /api/ps: %w", b.Name, err)
	}

	out := make([]Resident, 0, len(ps.Models))
	for _, m := range ps.Models {
		r := Resident{Backend: b.Name, ModelID: m.Name, VRAMBytes: m.SizeVRAM}
		if m.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, m.ExpiresAt); err == nil {
				r.ExpiresAt = t.UTC()
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// post sends a control request and reports what the backend said if it refused.
//
// The response body is read and discarded rather than ignored: leaving it
// unread would keep the connection out of the pool, and on a failure it carries
// the only useful explanation the backend gives.
func (c *Controller) post(ctx context.Context, b Backend, url string, body map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", b.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(payload))
		if detail == "" {
			detail = resp.Status
		}
		return fmt.Errorf("%s refused: %s", b.Name, detail)
	}
	return nil
}

// keepAliveValue renders a duration the way Ollama's API expects it: a number
// of seconds, with any negative value meaning "keep it loaded indefinitely".
func keepAliveValue(d time.Duration) int {
	if d < 0 {
		return -1
	}
	return int(d.Seconds())
}
