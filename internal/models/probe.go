package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// probeTimeout bounds a single backend probe. A backend that is loading a
// model can be slow to answer, but the UI waits on this, so it stays short.
const probeTimeout = 8 * time.Second

// maxProbeBytes bounds a probe response. /api/tags on a machine with many
// models is the largest of these and is still tiny.
const maxProbeBytes = 4 << 20

// Prober queries backends for their inventory.
type Prober struct {
	client *http.Client
}

// NewProber builds a Prober.
func NewProber() *Prober {
	return &Prober{client: &http.Client{Timeout: probeTimeout}}
}

// Probe returns what a backend is currently serving.
//
// An unreachable backend is not an error from the caller's point of view — a
// local inference server being down is a normal, recoverable state that the UI
// shows rather than something that should fail a request. The error is
// returned so it can be recorded as the health note.
func (p *Prober) Probe(ctx context.Context, b Backend) ([]Model, error) {
	switch b.Kind {
	case KindOllama, KindHailo:
		return p.probeOllama(ctx, b)
	case KindLlamaCPP:
		return p.probeLlamaCPP(ctx, b)
	case KindAnthropic:
		// There is no inventory endpoint worth calling here, and the model is
		// whatever the backend was configured with.
		return []Model{{
			Backend:      b.Name,
			ID:           b.Model,
			Family:       "Claude",
			Capabilities: []Capability{CapTools, CapVision, CapReasoning},
			Loaded:       true,
			LastSeenAt:   time.Now().UTC(),
		}}, nil
	default:
		return p.probeOpenAI(ctx, b)
	}
}

/* ---------- OpenAI-compatible (vLLM, LM Studio, llama-swap, generic) ---------- */

type oaiModelList struct {
	Data []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
		// llama-swap and some others report a description; vLLM reports
		// max_model_len. Both are optional.
		MaxModelLen int `json:"max_model_len"`
	} `json:"data"`
}

func (p *Prober) probeOpenAI(ctx context.Context, b Backend) ([]Model, error) {
	var list oaiModelList
	if err := p.getJSON(ctx, b, apiRoot(b.BaseURL)+"/v1/models", &list); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	out := make([]Model, 0, len(list.Data))
	for _, m := range list.Data {
		out = append(out, Model{
			Backend:    b.Name,
			ID:         m.ID,
			ParamsB:    inferParams(m.ID),
			Quant:      inferQuant(m.ID),
			CtxLen:     m.MaxModelLen,
			LastSeenAt: now,
			// An OpenAI-compatible list says nothing about residency. Naming
			// the configured default as loaded is the best available signal.
			Loaded: m.ID == b.Model,
		})
	}
	return out, nil
}

/* ---------- llama.cpp ---------- */

// llamaProps is the subset of /props that is useful here. llama-server serves
// one model, but llama-swap in front of it serves many and answers /v1/models
// for all of them — so both are consulted and merged.
type llamaProps struct {
	DefaultGenerationSettings struct {
		NCtx int `json:"n_ctx"`
	} `json:"default_generation_settings"`
	ModelPath    string `json:"model_path"`
	ChatTemplate string `json:"chat_template"`
}

func (p *Prober) probeLlamaCPP(ctx context.Context, b Backend) ([]Model, error) {
	list, err := p.probeOpenAI(ctx, b)
	if err != nil {
		return nil, err
	}

	// /props describes the currently loaded model. It fails harmlessly against
	// llama-swap when nothing is loaded yet, so its absence is not an error.
	var props llamaProps
	if err := p.getJSON(ctx, b, apiRoot(b.BaseURL)+"/props", &props); err == nil {
		name := props.ModelPath
		if idx := strings.LastIndexAny(name, "/\\"); idx >= 0 {
			name = name[idx+1:]
		}
		for i := range list {
			// Match the loaded model by path or id; llama-server reports the
			// full path where the OpenAI list reports an alias.
			if name == "" || !strings.Contains(strings.ToLower(name), strings.ToLower(list[i].ID)) {
				continue
			}
			list[i].Loaded = true
			if list[i].CtxLen == 0 {
				list[i].CtxLen = props.DefaultGenerationSettings.NCtx
			}
			if list[i].Quant == "" {
				list[i].Quant = inferQuant(name)
			}
			if list[i].ParamsB == 0 {
				list[i].ParamsB = inferParams(name)
			}
			if templateSupportsTools(props.ChatTemplate) {
				list[i].Capabilities = append(list[i].Capabilities, CapTools)
			}
		}
	}

	enrichFromSeed(list)
	return list, nil
}

/* ---------- Ollama (and hailo-ollama) ---------- */

type ollamaTags struct {
	Models []struct {
		Name    string `json:"name"`
		Model   string `json:"model"`
		Size    int64  `json:"size"`
		Details struct {
			Family            string `json:"family"`
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

type ollamaPS struct {
	Models []struct {
		Name       string `json:"name"`
		Model      string `json:"model"`
		SizeVRAM   int64  `json:"size_vram"`
		ContextLen int    `json:"context_length"`
	} `json:"models"`
}

type ollamaShow struct {
	Capabilities []string       `json:"capabilities"`
	ModelInfo    map[string]any `json:"model_info"`
}

func (p *Prober) probeOllama(ctx context.Context, b Backend) ([]Model, error) {
	root := apiRoot(b.BaseURL)

	var tags ollamaTags
	if err := p.getJSON(ctx, b, root+"/api/tags", &tags); err != nil {
		return nil, err
	}

	// Residency and live VRAM use. A failure here is not fatal — it only means
	// the loaded flags stay false.
	loaded := map[string]ollamaLoaded{}
	var ps ollamaPS
	if err := p.getJSON(ctx, b, root+"/api/ps", &ps); err == nil {
		for _, m := range ps.Models {
			loaded[m.Name] = ollamaLoaded{vram: m.SizeVRAM, ctx: m.ContextLen}
		}
	}

	now := time.Now().UTC()
	out := make([]Model, 0, len(tags.Models))
	for _, m := range tags.Models {
		mdl := Model{
			Backend:    b.Name,
			ID:         m.Name,
			Family:     m.Details.Family,
			ParamsB:    inferParams(m.Details.ParameterSize),
			Quant:      strings.ToUpper(m.Details.QuantizationLevel),
			SizeBytes:  m.Size,
			LastSeenAt: now,
		}
		if mdl.ParamsB == 0 {
			mdl.ParamsB = inferParams(m.Name)
		}
		if live, ok := loaded[m.Name]; ok {
			mdl.Loaded = true
			mdl.VRAMBytes = live.vram
			mdl.CtxLen = live.ctx
		}

		// /api/show is a POST per model. It is the only source of capability
		// flags, which decide whether a model can be offered for tool calling
		// or vision at all, so it is worth the extra round trips.
		var show ollamaShow
		if err := p.postJSON(ctx, b, root+"/api/show",
			map[string]string{"model": m.Name}, &show); err == nil {
			for _, c := range show.Capabilities {
				switch c {
				case "tools":
					mdl.Capabilities = append(mdl.Capabilities, CapTools)
				case "vision":
					mdl.Capabilities = append(mdl.Capabilities, CapVision)
				case "embedding":
					mdl.Capabilities = append(mdl.Capabilities, CapEmbedding)
				case "thinking":
					mdl.Capabilities = append(mdl.Capabilities, CapReasoning)
				}
			}
			if mdl.CtxLen == 0 {
				mdl.CtxLen = ollamaContextLength(show.ModelInfo)
			}
		}
		out = append(out, mdl)
	}

	enrichFromSeed(out)
	return out, nil
}

type ollamaLoaded struct {
	vram int64
	ctx  int
}

// ollamaContextLength digs the trained context length out of the architecture
// specific key in model_info, e.g. "qwen3.context_length".
func ollamaContextLength(info map[string]any) int {
	for key, value := range info {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		if n, ok := value.(float64); ok {
			return int(n)
		}
	}
	return 0
}

/* ---------- shared helpers ---------- */

// apiRoot strips a trailing /v1 so native endpoints can be addressed too. Both
// "http://host:11434" and "http://host:11434/v1" are accepted in config, since
// people copy whichever their serving stack printed.
func apiRoot(base string) string {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	return strings.TrimSuffix(base, "/v1")
}

// templateSupportsTools reports whether a Jinja chat template renders tools.
// A template that never mentions them was not trained for tool calling, and
// llama.cpp will not produce tool_use blocks from it.
func templateSupportsTools(tmpl string) bool {
	return strings.Contains(tmpl, "tools") || strings.Contains(tmpl, "tool_calls")
}

// enrichFromSeed fills gaps from curated knowledge. It only ever adds: a value
// the backend actually reported is always preferred over a guess.
func enrichFromSeed(list []Model) {
	for i := range list {
		seed, ok := matchSeed(list[i].ID)
		if !ok {
			continue
		}
		if list[i].Family == "" {
			list[i].Family = seed.Family
		}
		if list[i].ParamsB == 0 {
			list[i].ParamsB = seed.ParamsB
		}
		if len(list[i].Capabilities) == 0 {
			list[i].Capabilities = seed.Capabilities
		}
	}
}

// matchSeed finds a curated entry for a backend's model id. Names vary between
// backends for the same weights ("qwen3:8b", "qwen3-8b", "Qwen3-8B-Q4_K_M"),
// so matching normalises separators.
func matchSeed(id string) (SeedModel, bool) {
	norm := normalizeID(id)
	for _, s := range Seed {
		if strings.Contains(norm, normalizeID(s.ID)) {
			return s, true
		}
		if s.OllamaTag != "" && strings.Contains(norm, normalizeID(s.OllamaTag)) {
			return s, true
		}
	}
	return SeedModel{}, false
}

func normalizeID(s string) string {
	s = strings.ToLower(s)
	for _, sep := range []string{":", "_", " ", "."} {
		s = strings.ReplaceAll(s, sep, "-")
	}
	return s
}

func (p *Prober) getJSON(ctx context.Context, b Backend, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return p.do(req, b, dst)
}

func (p *Prober) postJSON(ctx context.Context, b Backend, url string, body, dst any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return p.do(req, b, dst)
}

func (p *Prober) do(req *http.Request, b Backend, dst any) error {
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", req.URL.Path, resp.Status)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, dst)
}
