package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultHubURL is the Hugging Face Hub API root. Public model metadata needs
// no authentication.
const DefaultHubURL = "https://huggingface.co"

// Errors the Hub can return that are facts about the world rather than faults
// in this server.
//
// Distinguished for the same reason modelrepo's are: answering "this
// repository is gated and you have no token" with "internal error" makes the
// whole feature undiagnosable from the browser, and every one of these is
// something the operator can act on.
var (
	// ErrHubNotFound reports a repository the Hub does not have.
	ErrHubNotFound = errors.New("no such repository on the Hugging Face Hub")

	// ErrHubForbidden reports a repository this server may not read: gated
	// terms not accepted, a private repository, or no token where one is
	// needed.
	ErrHubForbidden = errors.New("the Hugging Face Hub refused access")

	// ErrHubRateLimited reports an exhausted rate-limit window. See RateLimit
	// for why this is a routine condition rather than an exceptional one.
	ErrHubRateLimited = errors.New("the Hugging Face Hub rate limit is exhausted")

	// ErrHubUnavailable reports the Hub being unreachable, or answering 5xx.
	ErrHubUnavailable = errors.New("the Hugging Face Hub is not answering")

	// ErrHubBadRequest reports a request the Hub itself rejected — a malformed
	// repository id, a stale cursor, an expansion it has retired.
	ErrHubBadRequest = errors.New("the Hugging Face Hub rejected the request")
)

// maxHubResponse bounds a single response. The largest thing read here is a
// file listing for a repository with thousands of shards.
const maxHubResponse = 16 << 20

// RateLimit is the Hub's own account of how much budget is left in the current
// window, read from the headers it sets on every response.
//
// Worth carrying rather than discovering by failing. The anonymous allowance is
// 500 API calls per five minutes counted against *this server's* IP and shared
// by every client of it, which a few minutes of browsing will spend; a token
// doubles the allowance and moves it off the shared address. The Hub meters
// three buckets separately and only "api" is reached from here.
type RateLimit struct {
	// Bucket is "api", "resolvers" or "pages".
	Bucket string `json:"bucket,omitempty"`
	// Remaining is how many requests are left in this window.
	Remaining int `json:"remaining"`
	// ResetSeconds is how long until the window rolls over.
	ResetSeconds int `json:"reset_seconds"`
	// Quota and WindowSeconds come from the policy header. They are what tells
	// a token's allowance from an anonymous one, which is the difference an
	// operator wondering why searches fail actually needs to see.
	Quota         int `json:"quota,omitempty"`
	WindowSeconds int `json:"window_seconds,omitempty"`
}

// RateLimitError is a 429 with the wait attached, so a caller can say how long
// rather than only that it happened.
type RateLimitError struct{ RateLimit }

func (e *RateLimitError) Error() string {
	bucket := firstNonEmptyString(e.Bucket, "api")
	if e.ResetSeconds > 0 {
		return fmt.Sprintf("the Hugging Face Hub rate limit for %s requests is exhausted; it resets in %ds",
			bucket, e.ResetSeconds)
	}
	return fmt.Sprintf("the Hugging Face Hub rate limit for %s requests is exhausted", bucket)
}

func (e *RateLimitError) Unwrap() error { return ErrHubRateLimited }

// parseRateLimit reads the two headers the Hub sets on every response. They
// follow draft-ietf-httpapi-ratelimit-headers, which in practice looks like:
//
//	RateLimit: "api";r=499;t=71
//	RateLimit-Policy: "fixed window";"api";q=500;w=300
//
// A quoted field with no "=" is a name; the rest are the numbers.
func parseRateLimit(h http.Header) *RateLimit {
	raw := h.Get("RateLimit")
	if raw == "" {
		return nil
	}
	rl := &RateLimit{}
	for _, field := range strings.Split(raw, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			rl.Bucket = strings.Trim(strings.TrimSpace(key), `"`)
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "r":
			rl.Remaining = n
		case "t":
			rl.ResetSeconds = n
		}
	}
	for _, field := range strings.Split(h.Get("RateLimit-Policy"), ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "q":
			rl.Quota = n
		case "w":
			rl.WindowSeconds = n
		}
	}
	return rl
}

// How long each class of answer is held. A search is a live question and is
// cached only long enough to survive a page redraw; the facts about one
// repository barely move within a session; the tag vocabulary and the token's
// own identity essentially never do.
const (
	searchTTL = 60 * time.Second
	repoTTL   = 5 * time.Minute
	vocabTTL  = time.Hour
)

// hubCacheMax bounds the cache. Entries are metadata and model cards, so a few
// hundred is small — the cap exists so that a long browsing session cannot grow
// the server's heap without limit.
const hubCacheMax = 256

// hubCache is a small time-bounded cache over GET responses.
//
// Opening one repository costs several requests — the file list, the refs, the
// card, the scan — and comparing four repositories does that four times. Without
// this, ordinary browsing is what exhausts the window described on RateLimit.
type hubCache struct {
	mu      sync.Mutex
	entries map[string]hubCacheEntry
}

type hubCacheEntry struct {
	payload []byte
	meta    hubMeta
	expires time.Time
}

func newHubCache() *hubCache { return &hubCache{entries: map[string]hubCacheEntry{}} }

func (c *hubCache) get(key string) ([]byte, hubMeta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		return nil, hubMeta{}, false
	}
	return e.payload, e.meta, true
}

func (c *hubCache) put(key string, payload []byte, meta hubMeta, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= hubCacheMax {
		now := time.Now()
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
		// Still full: drop entries until there is room. Which ones go is
		// arbitrary, and deliberately so — this is a cache, and the cost of a
		// wrong eviction is one more request.
		for k := range c.entries {
			if len(c.entries) < hubCacheMax {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[key] = hubCacheEntry{payload: payload, meta: meta, expires: time.Now().Add(ttl)}
}

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
	cache   *hubCache

	// rate is the last allowance the Hub reported. Guarded because a browser
	// polling downloads and a tool call can be in flight together.
	mu   sync.Mutex
	rate *RateLimit
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
		cache:   newHubCache(),
	}
}

// HasToken reports whether a token is configured, without revealing it. The
// answer changes what a failure means and what the allowance is, so the UI is
// entitled to it; the token itself never leaves this process.
func (h *Hub) HasToken() bool { return h.token != "" }

// RateLimit returns the allowance the Hub reported on the most recent request,
// or nil if none has been made yet.
func (h *Hub) RateLimit() *RateLimit {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rate == nil {
		return nil
	}
	snapshot := *h.rate
	return &snapshot
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
	// PipelineTag is the Hub's own classification of what the model does —
	// "text-generation", "image-text-to-text" and so on. It is the field the
	// Hub's own filters are built on, so a browser that offers those filters
	// has to be able to show what it filtered by.
	PipelineTag string `json:"pipeline_tag,omitempty"`
	// Providers lists the hosted inference providers serving this model. It is
	// reported, never used: nothing here calls them. It answers "is there a way
	// to try this without downloading 30GB first", which is a fair question to
	// ask of a repository before fetching it.
	Providers []HubProvider `json:"providers,omitempty"`

	// The following are populated by Detail, from the Hub's parsed GGUF
	// metadata. They are what makes a fit estimate possible.
	ParamsB      float64      `json:"params_b,omitempty"`
	CtxLen       int          `json:"ctx_len,omitempty"`
	Architecture string       `json:"architecture,omitempty"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	// Quants lists the quantized files in the repository.
	Quants []HubQuant `json:"quants,omitempty"`
	// HasWeights reports whether the repository actually carries weights a
	// server could load or convert: a root-level GGUF or safetensors file.
	//
	// It is not the same question as "does it have a parameter count". A count
	// is guessed from the name when nothing better is published, and a name is
	// exactly what a repository of something-else-entirely also has: Qwen's
	// interpretability suite publishes SAE-Res-Qwen3-8B-Base-…, which parses as
	// an 8B model and is a directory of .pt autoencoder checkpoints. Offering
	// to convert one wastes a listing to find out there is nothing to convert.
	HasWeights bool `json:"has_weights,omitempty"`
	// Fit is the best verdict across every machine that could run this model,
	// naming the one that earned it. On a server with no fleet that is this
	// host and Fit.Host is empty, exactly as before.
	Fit *Fit `json:"fit,omitempty"`
	// HostFits is that grading per machine, at the default quantization. It is
	// what turns "fits" into an answer someone can act on — which box to load
	// it on — and is short: a home fleet is a handful of machines.
	HostFits []Fit `json:"host_fits,omitempty"`
}

// HubProvider is one hosted provider serving a model.
type HubProvider struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
	Task   string `json:"task,omitempty"`
}

// hubProviderMapping decodes the inferenceProviderMapping field, which the Hub
// returns in two different shapes depending on which endpoint answered.
//
// A search sends an array of objects, each naming its own provider; the detail
// endpoint sends an object keyed by provider name. Both are current, both turn
// up in ordinary use, and a decoder written for one fails outright on the
// other — which is how a single unserved model in a page of results took the
// whole search down with it.
//
// The array form carries more besides: pricing, context length and measured
// throughput per provider. None of it is read here. Calling hosted inference is
// not something this file does, and the fact worth reporting is only that there
// is a way to try the model without fetching thirty gigabytes first.
type hubProviderMapping []HubProvider

func (m *hubProviderMapping) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	type entry struct {
		Provider string `json:"provider"`
		Status   string `json:"status"`
		Task     string `json:"task"`
	}

	if raw[0] == '[' {
		var list []entry
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		for _, e := range list {
			*m = append(*m, HubProvider{Name: e.Provider, Status: e.Status, Task: e.Task})
		}
		return nil
	}

	var byName map[string]entry
	if err := json.Unmarshal(raw, &byName); err != nil {
		return err
	}
	for name, e := range byName {
		*m = append(*m, HubProvider{Name: firstNonEmptyString(e.Provider, name), Status: e.Status, Task: e.Task})
	}
	return nil
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
	// HostFits is that grading per machine. A quantization is chosen *in order
	// to* land on a particular box — the whole reason there are eight of them —
	// so the best verdict alone throws away the answer being looked for. It is
	// carried only when there is more than one machine to choose between.
	HostFits []Fit `json:"host_fits,omitempty"`
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

	// Safetensors is the Hub's parse of the original weights, for a repository
	// that ships those rather than a GGUF requantization. It carries the same
	// authoritative parameter count the GGUF header does.
	Safetensors *struct {
		Total int64 `json:"total"`
	} `json:"safetensors"`

	// InferenceProviderMapping names the hosted providers serving the model.
	InferenceProviderMapping hubProviderMapping `json:"inferenceProviderMapping"`

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
func (r hubRecord) model(hosts []*Hardware, contextTokens int) HubModel {
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
		PipelineTag:      r.PipelineTag,
	}
	if len(r.BaseModels.Models) > 0 {
		m.BaseModel = r.BaseModels.Models[0].ID
	}
	m.HasWeights = hasLoadableWeights(r.Siblings)

	if r.GGUF != nil {
		// total is a parameter count, not a byte count, despite the name.
		m.ParamsB = float64(r.GGUF.Total) / 1e9
		m.CtxLen = r.GGUF.ContextLen
		m.Architecture = r.GGUF.Architecture
		if templateSupportsTools(r.GGUF.ChatTemplate) {
			m.Capabilities = append(m.Capabilities, CapTools)
		}
	}
	if m.ParamsB == 0 && r.Safetensors != nil && r.Safetensors.Total > 0 {
		// The same authoritative count, for a repository shipping the original
		// weights rather than a requantization. Without this every safetensors
		// repository fell through to the guess below, which reads the number
		// out of the *name* and is wrong whenever the name lies.
		m.ParamsB = float64(r.Safetensors.Total) / 1e9
	}
	if m.ParamsB == 0 {
		// A guess from the name, and marked as one everywhere it is used. The
		// Hub reports the real figure for anything with a parsed GGUF header.
		m.ParamsB = inferParams(r.ID)
	}
	m.Providers = append(m.Providers, r.InferenceProviderMapping...)
	// Sorted because map iteration order is random, and without this two
	// identical requests return JSON that differs for no reason — which makes
	// the cache above look broken and any diff of two results unreadable.
	sort.Slice(m.Providers, func(i, j int) bool { return m.Providers[i].Name < m.Providers[j].Name })
	if tagsMentionVision(r.Tags, r.PipelineTag) {
		m.Capabilities = append(m.Capabilities, CapVision)
	}

	quants := groupQuants(r.Siblings)
	m.QuantCount = len(quants)
	for _, q := range quants {
		if len(hosts) > 0 && m.ParamsB > 0 {
			// One verdict per machine for this quantization, which is not the
			// same set of answers as the repository's default: a smaller quant
			// can fit a card the default one does not, and that is the whole
			// reason this list is worth reading file by file.
			graded := EstimateFleetFit(FitInput{
				ParamsB:       m.ParamsB,
				Quant:         q.Quant,
				ContextTokens: contextTokens,
			}, hosts)
			best := BestFit(graded)
			q.Fit = &best
			if len(graded) > 1 {
				q.HostFits = graded
			}
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

	if len(hosts) > 0 && m.ParamsB > 0 {
		in := FitInput{
			ParamsB:       m.ParamsB,
			Quant:         DefaultQuant,
			ContextTokens: contextTokens,
		}
		graded := EstimateFleetFit(in, hosts)
		best := BestFit(graded)
		m.Fit = &best
		// Only worth carrying when there is a choice to report. With one
		// machine the badge already says everything the list would.
		if len(graded) > 1 {
			m.HostFits = graded
		}
	}
	return m
}

// SearchOptions narrows a Hub query.
//
// Every field below maps to a parameter the Hub search endpoint already
// accepts. Together they are the difference between a search box and a
// browser: without an author or a pipeline filter, the only way to reach a
// specific publisher's work is to guess words that appear in its name.
type SearchOptions struct {
	Query string
	// Limit caps results; zero means 20 and anything above maxSearchLimit is
	// clamped to it.
	Limit int
	// GGUFOnly restricts to repositories carrying GGUF files, which is what
	// llama.cpp and Ollama can actually load.
	GGUFOnly bool
	// Sort is "downloads", "downloadsAllTime", "likes", "lastModified",
	// "createdAt" or "trendingScore".
	Sort string
	// Author restricts to one owner, e.g. "unsloth" or "Qwen".
	Author string
	// Library restricts by the framework the weights are for, e.g.
	// "transformers" or "gguf".
	Library string
	// PipelineTag restricts by task, e.g. "text-generation".
	PipelineTag string
	// Filters are raw Hub tag filters — "license:apache-2.0", "language:en".
	// The Hub ANDs them, and accepts any tag it indexes, so this stays a list
	// of strings rather than growing a field per tag namespace.
	Filters []string
	// InferenceProvider restricts to models a named hosted provider serves.
	InferenceProvider string
	// Cursor continues a previous page. It is the opaque value from a prior
	// SearchPage.Next and nothing else; see nextCursor.
	Cursor string
	// Hosts are the machines each result is graded against, the same way the
	// detail view does it. A search that cannot say whether a model runs here
	// is most of the way to useless — and on a fleet "here" is several
	// machines, none of them necessarily this one.
	Hosts         []*Hardware
	ContextTokens int
}

// maxSearchLimit caps a page. The Hub itself will return far more, but every
// result carries its whole file list, so a large page is megabytes for a screen
// that shows a dozen rows.
const maxSearchLimit = 100

// SearchPage is one page of results and the means to ask for the next.
type SearchPage struct {
	Models []HubModel `json:"models"`
	// Next is empty on the last page.
	Next string `json:"next,omitempty"`
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
	"pipeline_tag", "baseModels", "safetensors", "inferenceProviderMapping",
}

// Search queries the Hub.
func (h *Hub) Search(ctx context.Context, opts SearchOptions) (SearchPage, error) {
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	if opts.Limit > maxSearchLimit {
		opts.Limit = maxSearchLimit
	}
	if opts.Sort == "" {
		opts.Sort = "downloads"
	}

	q := url.Values{}
	if opts.Query != "" {
		q.Set("search", opts.Query)
	}
	// A fresh slice: appending the GGUF filter onto the caller's would mutate
	// an argument, and this one is routinely a literal reused across calls.
	filters := make([]string, 0, len(opts.Filters)+1)
	if opts.GGUFOnly {
		filters = append(filters, "gguf")
	}
	filters = append(filters, opts.Filters...)
	for _, f := range filters {
		if f = strings.TrimSpace(f); f != "" {
			q.Add("filter", f)
		}
	}
	for key, value := range map[string]string{
		"author":             opts.Author,
		"library":            opts.Library,
		"pipeline_tag":       opts.PipelineTag,
		"inference_provider": opts.InferenceProvider,
		"cursor":             opts.Cursor,
	} {
		if v := strings.TrimSpace(value); v != "" {
			q.Set(key, v)
		}
	}
	q.Set("sort", opts.Sort)
	q.Set("direction", "-1")
	q.Set("limit", fmt.Sprint(opts.Limit))
	for _, e := range searchExpand {
		q.Add("expand[]", e)
	}

	var raw []hubRecord
	meta, err := h.get(ctx, h.baseURL+"/api/models?"+q.Encode(), searchTTL, &raw)
	if err != nil {
		return SearchPage{}, err
	}

	out := make([]HubModel, 0, len(raw))
	for _, r := range raw {
		m := r.model(opts.Hosts, opts.ContextTokens)
		// The per-file list is what the detail request is for: it is long,
		// most of it is scrolled past, and only there does it carry sizes.
		// The count of it survives, because that is a fact worth seeing.
		m.Quants = nil
		out = append(out, m)
	}
	return SearchPage{Models: out, Next: meta.next}, nil
}

// Detail fetches one repository, including its GGUF metadata and the list of
// quantized files. Each quantization is graded against every host given.
func (h *Hub) Detail(ctx context.Context, id string, hosts []*Hardware, contextTokens int) (*HubModel, error) {
	// blobs=true is what puts a size on every file. It is the number that
	// decides whether a download is worth starting, and the only place the Hub
	// offers it — the fit estimate next to it is a prediction from the
	// parameter count, not a measurement of what will land on the drive.
	repo, err := repoPath(id)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("blobs", "true")
	// The same expansion the search asks for. Without it the detail response
	// carries neither baseModels nor downloadsAllTime, so a fact shown on the
	// search card vanished the moment the repository was opened — precisely the
	// drift the shared hubRecord exists to prevent.
	for _, e := range searchExpand {
		q.Add("expand[]", e)
	}

	var raw hubRecord
	if _, err := h.get(ctx, h.baseURL+"/api/models/"+repo+"?"+q.Encode(), repoTTL, &raw); err != nil {
		return nil, err
	}

	m := raw.model(hosts, contextTokens)
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

// hasLoadableWeights reports whether a file list contains weights this server
// could do something with.
//
// Root level only, and the same two extensions the rest of the program knows:
// a GGUF it can serve, or safetensors it can convert. A shard in a
// subdirectory is a second copy of something, and a .pt, .bin or .onnx is a
// format nothing here reads.
func hasLoadableWeights(siblings []hubSibling) bool {
	for _, s := range siblings {
		name := strings.ToLower(s.Filename)
		if strings.Contains(name, "/") {
			continue
		}
		if strings.HasSuffix(name, ".safetensors") || strings.HasSuffix(name, ".gguf") {
			return true
		}
	}
	return false
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

// hubMeta is what a response carries besides its body.
type hubMeta struct {
	// next is the opaque cursor for the following page, empty on the last one.
	next string
}

// get fetches JSON and decodes it, returning what the response said about
// paging. dst may be nil to fetch for the side effects alone.
func (h *Hub) get(ctx context.Context, rawURL string, ttl time.Duration, dst any) (hubMeta, error) {
	payload, meta, err := h.fetch(ctx, rawURL, "application/json", ttl)
	if err != nil {
		return meta, err
	}
	if dst != nil {
		if err := json.Unmarshal(payload, dst); err != nil {
			return meta, fmt.Errorf("hub: decode: %w", err)
		}
	}
	return meta, nil
}

// fetch performs one GET, serving from the cache when a fresh copy is held.
func (h *Hub) fetch(ctx context.Context, rawURL, accept string, ttl time.Duration) ([]byte, hubMeta, error) {
	if payload, meta, ok := h.cache.get(rawURL); ok {
		return payload, meta, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, hubMeta{}, err
	}
	req.Header.Set("Accept", accept)
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, hubMeta{}, fmt.Errorf("%w: %v", ErrHubUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read off every response, the failures included: a 429 is exactly when the
	// remaining budget is worth knowing.
	if rl := parseRateLimit(resp.Header); rl != nil {
		h.mu.Lock()
		h.rate = rl
		h.mu.Unlock()
	}

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxHubResponse))
	if err != nil {
		return nil, hubMeta{}, fmt.Errorf("%w: read: %v", ErrHubUnavailable, err)
	}
	meta := hubMeta{next: nextCursor(resp.Header.Get("Link"))}

	if err := h.classify(resp, rawURL, payload); err != nil {
		return nil, meta, err
	}
	h.cache.put(rawURL, payload, meta, ttl)
	return payload, meta, nil
}

// classify turns a status code into an error a caller can act on.
func (h *Hub) classify(resp *http.Response, rawURL string, payload []byte) error {
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil

	case resp.StatusCode == http.StatusTooManyRequests:
		err := &RateLimitError{}
		if rl := parseRateLimit(resp.Header); rl != nil {
			err.RateLimit = *rl
		}
		return err

	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrHubNotFound, hubErrorText(payload, resp.Status))

	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// Whether a token was sent decides what the operator has to do about
		// this, and only this process knows which it was.
		//
		// Non-existence is named as a possibility in both cases because the Hub
		// deliberately will not distinguish it: asked anonymously for a
		// repository that is not there, it answers 401 rather than 404, so that
		// probing this endpoint cannot enumerate private repositories. A
		// mistyped name and a gated one are genuinely the same response.
		hint := "no Hugging Face token is configured — the repository may not exist, " +
			"or may be private or gated"
		if h.token != "" {
			hint = "a token is configured, so the repository may not exist, or may be " +
				"private, or gated on terms that account has not accepted"
		}
		if page := h.repoPage(rawURL); page != "" {
			hint += ". Terms can only be accepted in a browser, at " + page
		}
		return fmt.Errorf("%w: %s (%s)", ErrHubForbidden, hubErrorText(payload, resp.Status), hint)

	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %s", ErrHubUnavailable, resp.Status)

	default:
		return fmt.Errorf("%w: %s", ErrHubBadRequest, hubErrorText(payload, resp.Status))
	}
}

// repoPage turns the API URL a request failed on into the address of that
// repository's page on the web.
//
// Worth the trouble because of what the operator has to do next: the Hub has no
// API for accepting a gated repository's terms — that is a form in a browser,
// and only there — so the link is the entire remedy for the commonest cause of
// a 403 here. Reconstructing it by hand from a failed download is exactly the
// step where an owner or a quantisation suffix gets mistyped.
//
// Empty for the URLs that name no single repository: search, whoami, the tag
// vocabulary. Segments are taken from the escaped path, so what comes back is
// as safe to print as what went out.
func (h *Hub) repoPage(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	path := strings.Trim(u.EscapedPath(), "/")
	if rest, ok := strings.CutPrefix(path, "api/models/"); ok {
		path = rest
	} else if strings.HasPrefix(path, "api/") {
		return ""
	}
	// Whatever follows "owner/name" — a tree, a revision, a file — is detail
	// the page itself does not want.
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return h.baseURL + "/" + parts[0] + "/" + parts[1]
}

// hubErrorText prefers the Hub's own explanation over the bare status line. It
// says things like "Error parsing pagination cursor", which is the whole
// diagnosis; the status alone is "400 Bad Request", which is none of it.
func hubErrorText(payload []byte, status string) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &body); err == nil && strings.TrimSpace(body.Error) != "" {
		return strings.TrimSpace(body.Error)
	}
	return status
}

// nextCursor reads the cursor out of a Link: <...>; rel="next" header.
//
// Only the cursor survives, never the URL. The header is a string from off the
// machine, and re-requesting it verbatim would let whatever answered for the
// Hub point this server's next request at an address of its choosing. The
// cursor is fed back into a URL this package builds itself.
func nextCursor(link string) string {
	for _, part := range strings.Split(link, ",") {
		target, rest, ok := strings.Cut(strings.TrimSpace(part), ">")
		if !ok || !strings.Contains(rest, `rel="next"`) {
			continue
		}
		u, err := url.Parse(strings.TrimPrefix(strings.TrimSpace(target), "<"))
		if err != nil {
			continue
		}
		if cursor := u.Query().Get("cursor"); cursor != "" {
			return cursor
		}
	}
	return ""
}

// repoPath escapes a repository id for use in a Hub URL.
//
// The id arrives from a search result, a tool call or a browser URL — which is
// to say from outside — and it is interpolated into a path. Unescaped, a ".."
// in it addresses a different endpoint entirely. One segment is allowed as well
// as two, because the Hub's oldest models ("gpt2", "bert-base-uncased") have no
// owner.
func repoPath(id string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(id), "/")
	parts := strings.Split(trimmed, "/")
	if trimmed == "" || len(parts) > 2 {
		return "", fmt.Errorf("%w: %q is not an \"owner/name\" repository id", ErrHubBadRequest, id)
	}
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: %q is not an \"owner/name\" repository id", ErrHubBadRequest, id)
		}
		escaped = append(escaped, url.PathEscape(part))
	}
	return strings.Join(escaped, "/"), nil
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
