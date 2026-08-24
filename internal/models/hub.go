package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
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
	// DownloadsAllTime separates a model that is popular now from one that was
	// popular a year ago, which the 30-day figure alone cannot say.
	DownloadsAllTime int `json:"downloads_all_time,omitempty"`
	// Gated is "auto" or "manual" for a repository that requires accepting
	// terms before it can be fetched, and empty otherwise. Without a token a
	// download of one fails, and it is better to know that before starting.
	Gated string `json:"gated,omitempty"`
	// BaseModel is the model a quantization was made from, when the repository
	// says. It is the fastest way to tell eight near-identical repackagings
	// apart, and to find the original.
	BaseModel string `json:"base_model,omitempty"`
	// QuantCount is how many GGUF files the repository holds. A repository
	// with one quantization and one with twenty are different propositions,
	// and the list should not have to be opened to see which this is.
	QuantCount int `json:"quant_count,omitempty"`

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

// HubQuant is one quantization in a repository: usually a single GGUF file,
// sometimes a set of them.
type HubQuant struct {
	// Filename is the file to fetch, and for a split quantization the first
	// part — which is the one llama.cpp is pointed at.
	Filename string `json:"filename"`
	Quant    string `json:"quant"`
	// Parts lists every file of a split quantization, in order, and is empty
	// for the ordinary single-file case. Weights past about 50GB ship as
	// "-00001-of-00002" shards, and a shard on its own is not a model: all of
	// them have to arrive, in the same directory, or none should.
	Parts []string `json:"parts,omitempty"`
	// Incomplete marks a split quantization whose parts the repository does
	// not all list — an upload still in progress. Fetching what is there would
	// produce something that looks like a model and cannot be loaded, so this
	// says so instead.
	Incomplete bool `json:"incomplete,omitempty"`
	// SizeBytes is what this quantization occupies on the drive, summed across
	// its shards. Unlike the fit estimate it is a measurement rather than a
	// prediction, and it is the number that decides whether a download is
	// started at all.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	Fit       *Fit  `json:"fit,omitempty"`
}

// splitPart matches one shard of a split GGUF: the convention llama.cpp
// writes and every quantizer on the Hub follows.
var splitPart = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// hubRecord is one repository as the Hub returns it. Search and the detail
// endpoint produce the same shape once the search is asked to expand its
// fields, so they are decoded into one type and mapped by one function —
// otherwise the two paths drift and a fact shown on one page is missing on
// the other for no reason a reader could discover.
type hubRecord struct {
	ID               string       `json:"id"`
	Author           string       `json:"author"`
	Downloads        int          `json:"downloads"`
	DownloadsAllTime int          `json:"downloadsAllTime"`
	Likes            int          `json:"likes"`
	Tags             []string     `json:"tags"`
	LastModified     string       `json:"lastModified"`
	PipelineTag      string       `json:"pipeline_tag"`
	Gated            hubGated     `json:"gated"`
	Siblings         []hubSibling `json:"siblings"`

	// GGUF is the Hub's own parse of the model's GGUF header. It is the single
	// most useful field here: an authoritative parameter count and context
	// length without downloading gigabytes.
	//
	// ChatTemplate is read for one bit of information — whether the model can
	// be called with tools — and then dropped. It is a full Jinja template of
	// several kilobytes, it dominates the response, and no part of this
	// program has any use for it beyond that question.
	GGUF *struct {
		Total        int64  `json:"total"`
		Architecture string `json:"architecture"`
		ContextLen   int    `json:"context_length"`
		ChatTemplate string `json:"chat_template"`
	} `json:"gguf"`

	CardData struct {
		License string `json:"license"`
	} `json:"cardData"`

	// BaseModels names what a repackaged model was made from.
	BaseModels struct {
		Relation string `json:"relation"`
		Models   []struct {
			ID string `json:"id"`
		} `json:"models"`
	} `json:"baseModels"`
}

// hubGated decodes the Hub's gated field, which is false for an open
// repository and a string naming the approval flow for a closed one.
type hubGated string

func (g *hubGated) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] != '"' {
		*g = ""
		return nil
	}
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return err
	}
	*g = hubGated(name)
	return nil
}

// model maps a decoded record onto what this program shows. hw grades each
// quantization when it is non-nil.
func (r hubRecord) model(hw *Hardware, contextTokens int) HubModel {
	m := HubModel{
		ID:               r.ID,
		Author:           r.Author,
		Downloads:        r.Downloads,
		DownloadsAllTime: r.DownloadsAllTime,
		Likes:            r.Likes,
		Tags:             r.Tags,
		License:          firstNonEmptyString(r.CardData.License, licenseFromTags(r.Tags)),
		UpdatedAt:        parseHubTime(r.LastModified),
		Gated:            string(r.Gated),
	}
	if len(r.BaseModels.Models) > 0 {
		m.BaseModel = r.BaseModels.Models[0].ID
	}

	if r.GGUF != nil {
		// total is a parameter count, not a byte count, despite the name.
		m.ParamsB = float64(r.GGUF.Total) / 1e9
		m.CtxLen = r.GGUF.ContextLen
		m.Architecture = r.GGUF.Architecture
		if templateSupportsTools(r.GGUF.ChatTemplate) {
			m.Capabilities = append(m.Capabilities, CapTools)
		}
	}
	if m.ParamsB == 0 {
		// A guess from the name, and marked as one everywhere it is used. The
		// Hub reports the real figure for anything with a parsed GGUF header.
		m.ParamsB = inferParams(r.ID)
	}
	if tagsMentionVision(r.Tags, r.PipelineTag) {
		m.Capabilities = append(m.Capabilities, CapVision)
	}

	quants := groupQuants(r.Siblings)
	m.QuantCount = len(quants)
	for _, q := range quants {
		if hw != nil && m.ParamsB > 0 {
			fit := EstimateFit(FitInput{
				ParamsB:       m.ParamsB,
				Quant:         q.Quant,
				ContextTokens: contextTokens,
			}, hw)
			q.Fit = &fit
		}
		m.Quants = append(m.Quants, q)
	}
	// Order by size so the list reads from smallest to largest, which is the
	// order someone shopping under a VRAM limit wants. Stable, because several
	// distinct files can share an inferred label and repository order is a
	// better tiebreak than none.
	sort.SliceStable(m.Quants, func(i, j int) bool {
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
	return m
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
	// Hardware grades each result for fit when it is set, the same way the
	// detail view does. A search that cannot say whether a model runs here is
	// most of the way to useless.
	Hardware      *Hardware
	ContextTokens int
}

// searchExpand is what the search endpoint is asked to include.
//
// Without it a search returns barely more than an id: no author, no date, no
// GGUF header, and so no real parameter count, context length or tool support
// — the facts that decide which of fifteen similar repositories is worth
// downloading. They are all available on the search itself, which is why this
// is one request rather than fifteen follow-ups. The Hub replies with the full
// list of valid values in the body of a 400 if one of these is ever retired.
var searchExpand = []string{
	"gguf", "author", "gated", "tags", "cardData", "downloads",
	"downloadsAllTime", "likes", "lastModified", "siblings",
	"pipeline_tag", "baseModels",
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
	for _, e := range searchExpand {
		q.Add("expand[]", e)
	}

	var raw []hubRecord
	if err := h.get(ctx, h.baseURL+"/api/models?"+q.Encode(), &raw); err != nil {
		return nil, err
	}

	out := make([]HubModel, 0, len(raw))
	for _, r := range raw {
		m := r.model(opts.Hardware, opts.ContextTokens)
		// The per-file list is what the detail request is for: it is long,
		// most of it is scrolled past, and only there does it carry sizes.
		// The count of it survives, because that is a fact worth seeing.
		m.Quants = nil
		out = append(out, m)
	}
	return out, nil
}

// Detail fetches one repository, including its GGUF metadata and the list of
// quantized files. When hw is non-nil each quantization is graded for fit.
func (h *Hub) Detail(ctx context.Context, id string, hw *Hardware, contextTokens int) (*HubModel, error) {
	// blobs=true is what puts a size on every file. It is the number that
	// decides whether a download is worth starting, and the only place the Hub
	// offers it — the fit estimate next to it is a prediction from the
	// parameter count, not a measurement of what will land on the drive.
	var raw hubRecord
	if err := h.get(ctx, h.baseURL+"/api/models/"+id+"?blobs=true", &raw); err != nil {
		return nil, err
	}

	m := raw.model(hw, contextTokens)
	sizes := make(map[string]int64, len(raw.Siblings))
	for _, s := range raw.Siblings {
		sizes[s.Filename] = s.Size
	}
	for i := range m.Quants {
		q := &m.Quants[i]
		if len(q.Parts) == 0 {
			q.SizeBytes = sizes[q.Filename]
			continue
		}
		// A shard set costs what all of its shards cost. Showing the first
		// shard's size here is how a 61GB download came to be presented as a
		// 49GB one.
		for _, part := range q.Parts {
			q.SizeBytes += sizes[part]
		}
	}
	return &m, nil
}

// hubSibling is one file in a repository.
type hubSibling struct {
	Filename string `json:"rfilename"`
	// Size is only returned when the detail endpoint is asked for blobs, and
	// is zero on a plain search.
	Size int64 `json:"size"`
}

// groupQuants turns a repository's file list into one entry per quantization,
// joining the shards of a split GGUF back into the single thing they are.
//
// Grouping is by file name rather than by quantization label. Distinct files
// routinely infer to the same label — Q2_K_L reads as Q2_K — and keeping only
// the first of those hid a download the operator could perfectly well have
// made. Names are compared without their directory because that is how the
// repository stores them, so two that agree there would collide on the drive
// whatever this returned.
func groupQuants(siblings []hubSibling) []HubQuant {
	type group struct {
		quant string
		parts map[int]string
		want  int
	}
	groups := map[string]*group{}
	var order []string

	for _, s := range siblings {
		name := path.Base(s.Filename)
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}
		stem, part, want := splitParts(name)
		g := groups[stem]
		if g == nil {
			quant := inferQuant(s.Filename)
			if quant == "" {
				continue
			}
			g = &group{quant: quant, parts: map[int]string{}, want: want}
			groups[stem] = g
			order = append(order, stem)
		}
		g.parts[part] = s.Filename
	}

	out := make([]HubQuant, 0, len(order))
	for _, stem := range order {
		g := groups[stem]
		q := HubQuant{Quant: g.quant}
		if g.want < 2 {
			q.Filename = g.parts[1]
			out = append(out, q)
			continue
		}
		// The repository says how many shards there are, so a gap is
		// detectable here rather than at load time on the operator's machine.
		for i := 1; i <= g.want; i++ {
			f, ok := g.parts[i]
			if !ok {
				q.Incomplete = true
				continue
			}
			q.Parts = append(q.Parts, f)
		}
		if len(q.Parts) == 0 {
			continue
		}
		q.Filename = q.Parts[0]
		out = append(out, q)
	}
	return out
}

// splitParts reads llama.cpp's shard naming. A file that is not a shard comes
// back as itself, part 1 of 1.
func splitParts(name string) (stem string, part, want int) {
	m := splitPart.FindStringSubmatch(name)
	if m == nil {
		return name, 1, 1
	}
	part, _ = strconv.Atoi(m[2])
	want, _ = strconv.Atoi(m[3])
	if part < 1 || want < 1 || part > want {
		// Nonsense numbering: treat it as an ordinary file rather than
		// inventing a shard set out of it.
		return name, 1, 1
	}
	return m[1], part, want
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
