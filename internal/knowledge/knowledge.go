// Package knowledge holds agents and the documents they can consult.
//
// An *agent* here is not a second agent loop. It is a named configuration of
// the one in internal/agent: a system prompt layered over the base one, an
// optional model pin, a set of enabled sources, and a library of uploaded
// documents. The loop, the transcript and the approval model are unchanged.
//
// The reason it exists is scoping. A conversation about one client's GRC
// engagement should reach that client's documents and the GRC application's
// catalogs; a conversation about the firm's finances should reach neither.
// Before agents, every session saw every tool and no documents at all, so the
// only way to give a model context was to paste it into the message — which is
// exactly the failure this was built to fix: asked "how many Security NFRs
// concern network segmentation?", a model with no access to the catalog
// explains how it would answer if someone pasted the catalog in.
//
// # Sources
//
// A source is a family of tools an agent may use:
//
//   - documents — search and read this agent's own uploaded library.
//   - grc — query a GRC installation's catalogs (Security NFRs, 800-53
//     controls, regulation coverage, policies, risks) through its read-only
//     knowledge API. See internal/grc.
//   - web — search the operator's SearXNG instance and fetch a page. See
//     internal/websearch.
//
// An agent with no sources is a plain assistant, which is what every session
// was before this package existed.
package knowledge

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Source names. These are stored on the agent and decide which tools the loop
// offers a session.
const (
	SourceDocuments = "documents"
	SourceGRC       = "grc"
	SourceWeb       = "web"
)

// Sources lists every known source.
func Sources() []string { return []string{SourceDocuments, SourceGRC, SourceWeb} }

// Agent is a named configuration of the assistant.
type Agent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// SystemPrompt is layered over the base prompt, not substituted for it: the
	// rules about never claiming an action was performed hold for every agent.
	SystemPrompt string   `json:"system_prompt,omitempty"`
	Backend      string   `json:"backend,omitempty"`
	Model        string   `json:"model,omitempty"`
	Sources      []string `json:"sources"`
	// DocumentCount is filled on read, so a picker can say what an agent knows.
	DocumentCount int       `json:"document_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Has reports whether the agent may use a source.
func (a *Agent) Has(source string) bool {
	if a == nil {
		return false
	}
	for _, s := range a.Sources {
		if s == source {
			return true
		}
	}
	return false
}

// Document is one file in an agent's library.
type Document struct {
	ID        int64  `json:"id"`
	AgentID   string `json:"agent_id"`
	Title     string `json:"title"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	SourceURL string `json:"source_url,omitempty"`
	SHA256    string `json:"sha256"`
	ByteSize  int64  `json:"byte_size"`
	TextChars int    `json:"text_chars"`
	// ExtractVia names what read the text, which is the first thing to check
	// when a document searches badly.
	ExtractVia string    `json:"extract_via"`
	ChunkCount int       `json:"chunk_count"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// Chunk is a passage of a document, with the heading it sits under.
type Chunk struct {
	ID         int64  `json:"id"`
	DocumentID int64  `json:"document_id"`
	Ordinal    int    `json:"ordinal"`
	Heading    string `json:"heading"`
	Body       string `json:"body"`
	// Title and Filename are joined in on search, so a hit can cite its source
	// without a second query.
	Title    string `json:"title,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// ---- validation ----

var (
	// One to forty characters, no leading or trailing hyphen. A single
	// character is a legitimate id ("a"), so the middle and last groups are
	// optional rather than required.
	slugPattern    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	slugUnsafeRune = regexp.MustCompile(`[^a-z0-9]+`)
)

// Slug turns a name into an id: lowercase, hyphenated, bounded.
func Slug(name string) string {
	s := slugUnsafeRune.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

// Validate checks an agent before it is stored, and normalises its sources.
func (a *Agent) Validate() error {
	a.ID = strings.TrimSpace(a.ID)
	a.Name = strings.TrimSpace(a.Name)
	a.Description = strings.TrimSpace(a.Description)
	a.SystemPrompt = strings.TrimSpace(a.SystemPrompt)
	a.Backend = strings.TrimSpace(a.Backend)
	a.Model = strings.TrimSpace(a.Model)

	if a.Name == "" {
		return Invalidf("a name is required")
	}
	if a.ID == "" {
		a.ID = Slug(a.Name)
	}
	if !slugPattern.MatchString(a.ID) {
		return Invalidf("id %q must be lowercase letters, digits and hyphens, 1-40 characters, "+
			"not starting or ending with a hyphen", a.ID)
	}

	known := map[string]bool{}
	for _, s := range Sources() {
		known[s] = true
	}
	seen := map[string]bool{}
	normalised := make([]string, 0, len(a.Sources))
	for _, s := range a.Sources {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		if !known[s] {
			return Invalidf("unknown source %q (known: %s)", s, strings.Join(Sources(), ", "))
		}
		seen[s] = true
		normalised = append(normalised, s)
	}
	sort.Strings(normalised)
	a.Sources = normalised
	return nil
}

func encodeSources(sources []string) string { return strings.Join(sources, ",") }

func decodeSources(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// ---- errors ----

// ErrNotFound reports a missing agent or document.
type ErrNotFound struct{ What string }

func (e ErrNotFound) Error() string { return e.What + " not found" }

// ErrInvalid reports a caller mistake.
type ErrInvalid struct{ Message string }

func (e ErrInvalid) Error() string { return e.Message }

// Invalidf builds an ErrInvalid.
func Invalidf(format string, args ...any) error {
	return ErrInvalid{Message: fmt.Sprintf(format, args...)}
}

// NotFound builds an ErrNotFound.
func NotFound(what string) error { return ErrNotFound{What: what} }
