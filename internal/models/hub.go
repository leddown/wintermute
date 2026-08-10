package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// DefaultHubURL is the Hugging Face Hub API root. Public model metadata needs
// no authentication.
const DefaultHubURL = "https://huggingface.co"

// Hub searches the Hugging Face Hub for downloadable models.
//
// This is what keeps the Explore page honest as models are released: the
// curated Seed list ages, the Hub does not. Results are enriched with the fit
// calculator so a search answers "will this run here", not just "does this
// exist".
type Hub struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHub builds a Hub client. token is optional and only needed for gated
// repositories; searching public models works without one.
func NewHub(baseURL, token string) *Hub {
	if baseURL == "" {
		baseURL = DefaultHubURL
	}
	return &Hub{
		client:  &http.Client{Timeout: 20 * time.Second},
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
	}
}

// HubModel is one search result.
type HubModel struct {
	ID        string    `json:"id"`
	Author    string    `json:"author,omitempty"`
	Downloads int       `json:"downloads"`
	Likes     int       `json:"likes"`
	Tags      []string  `json:"tags,omitempty"`
	License   string    `json:"license,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	// The following are populated by Detail, from the Hub's parsed GGUF
	// metadata. They are what makes a fit estimate possible.
	ParamsB      float64      `json:"params_b,omitempty"`
	CtxLen       int          `json:"ctx_len,omitempty"`
	Architecture string       `json:"architecture,omitempty"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	// Quants lists the quantized files in the repository.
	Quants []HubQuant `json:"quants,omitempty"`
	// Fit is attached against the current hardware when requested.
	Fit *Fit `json:"fit,omitempty"`
}

// HubQuant is one quantized file in a repository.
type HubQuant struct {
	Filename string `json:"filename"`
	Quant    string `json:"quant"`
	Fit      *Fit   `json:"fit,omitempty"`
}

type hubSearchResult struct {
	ID           string   `json:"id"`
	Author       string   `json:"author"`
	Downloads    int      `json:"downloads"`
	Likes        int      `json:"likes"`
	Tags         []string `json:"tags"`
	LastModified string   `json:"lastModified"`
	PipelineTag  string   `json:"pipeline_tag"`
}

type hubDetail struct {
	ID           string   `json:"id"`
	Author       string   `json:"author"`
	Downloads    int      `json:"downloads"`
	Likes        int      `json:"likes"`
	Tags         []string `json:"tags"`
	LastModified string   `json:"lastModified"`
	PipelineTag  string   `json:"pipeline_tag"`
	Siblings     []struct {
		Filename string `json:"rfilename"`
	} `json:"siblings"`
	// GGUF is the Hub's own parse of the model's GGUF header. It is the single
	// most useful field here: an authoritative parameter count and context
	// length without downloading gigabytes.
	GGUF *struct {
		Total        int64  `json:"total"`
		Architecture string `json:"architecture"`
		ContextLen   int    `json:"context_length"`
		ChatTemplate string `json:"chat_template"`
	} `json:"gguf"`
	CardData struct {
		License string `json:"license"`
	} `json:"cardData"`
}

// SearchOptions narrows a Hub query.
type SearchOptions struct {
	Query string
	// Limit caps results; zero means 20.
	Limit int
	// GGUFOnly restricts to repositories carrying GGUF files, which is what
	// llama.cpp and Ollama can actually load.
	GGUFOnly bool
	// Sort is "downloads", "likes" or "lastModified".
	Sort string
}

// Search queries the Hub.
func (h *Hub) Search(ctx context.Context, opts SearchOptions) ([]HubModel, error) {
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	if opts.Sort == "" {
		opts.Sort = "downloads"
	}

	q := url.Values{}
	if opts.Query != "" {
		q.Set("search", opts.Query)
	}
	if opts.GGUFOnly {
		q.Set("filter", "gguf")
	}
	q.Set("sort", opts.Sort)
	q.Set("direction", "-1")
	q.Set("limit", fmt.Sprint(opts.Limit))

	var raw []hubSearchResult
	if err := h.get(ctx, h.baseURL+"/api/models?"+q.Encode(), &raw); err != nil {
		return nil, err
	}

	out := make([]HubModel, 0, len(raw))
	for _, r := range raw {
		out = append(out, HubModel{
			ID:        r.ID,
			Author:    r.Author,
			Downloads: r.Downloads,
			Likes:     r.Likes,
			Tags:      r.Tags,
			License:   licenseFromTags(r.Tags),
			UpdatedAt: parseHubTime(r.LastModified),
			ParamsB:   inferParams(r.ID),
		})
	}
	return out, nil
}

// Detail fetches one repository, including its GGUF metadata and the list of
// quantized files. When hw is non-nil each quantization is graded for fit.
func (h *Hub) Detail(ctx context.Context, id string, hw *Hardware, contextTokens int) (*HubModel, error) {
	var raw hubDetail
	if err := h.get(ctx, h.baseURL+"/api/models/"+id, &raw); err != nil {
		return nil, err
	}

	m := &HubModel{
		ID:        raw.ID,
		Author:    raw.Author,
		Downloads: raw.Downloads,
		Likes:     raw.Likes,
		Tags:      raw.Tags,
		License:   firstNonEmptyString(raw.CardData.License, licenseFromTags(raw.Tags)),
		UpdatedAt: parseHubTime(raw.LastModified),
	}

	if raw.GGUF != nil {
		// total is a parameter count, not a byte count, despite the name.
		m.ParamsB = float64(raw.GGUF.Total) / 1e9
		m.CtxLen = raw.GGUF.ContextLen
		m.Architecture = raw.GGUF.Architecture
		if templateSupportsTools(raw.GGUF.ChatTemplate) {
			m.Capabilities = append(m.Capabilities, CapTools)
		}
	}
	if m.ParamsB == 0 {
		m.ParamsB = inferParams(raw.ID)
	}
	if tagsMentionVision(raw.Tags, raw.PipelineTag) {
		m.Capabilities = append(m.Capabilities, CapVision)
	}

	seen := map[string]bool{}
	for _, s := range raw.Siblings {
		if !strings.HasSuffix(strings.ToLower(s.Filename), ".gguf") {
			continue
		}
		quant := inferQuant(s.Filename)
		if quant == "" || seen[quant] {
			continue
		}
		seen[quant] = true
		q := HubQuant{Filename: s.Filename, Quant: quant}
		if hw != nil && m.ParamsB > 0 {
			fit := EstimateFit(FitInput{
				ParamsB:       m.ParamsB,
				Quant:         quant,
				ContextTokens: contextTokens,
			}, hw)
			q.Fit = &fit
		}
		m.Quants = append(m.Quants, q)
	}

	// Order by size so the list reads from smallest to largest, which is the
	// order someone shopping under a VRAM limit wants.
	sort.Slice(m.Quants, func(i, j int) bool {
		return quantBPW[m.Quants[i].Quant] < quantBPW[m.Quants[j].Quant]
	})

	if hw != nil && m.ParamsB > 0 {
		fit := EstimateFit(FitInput{
			ParamsB:       m.ParamsB,
			Quant:         DefaultQuant,
			ContextTokens: contextTokens,
		}, hw)
		m.Fit = &fit
	}
	return m, nil
}

func (h *Hub) get(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("hub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub: %s", resp.Status)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("hub: read: %w", err)
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return fmt.Errorf("hub: decode: %w", err)
	}
	return nil
}

func licenseFromTags(tags []string) string {
	for _, t := range tags {
		if rest, ok := strings.CutPrefix(t, "license:"); ok {
			return rest
		}
	}
	return ""
}

func tagsMentionVision(tags []string, pipeline string) bool {
	if strings.Contains(pipeline, "image") {
		return true
	}
	for _, t := range tags {
		switch t {
		case "vision", "multimodal", "image-text-to-text":
			return true
		}
	}
	return false
}

func parseHubTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
