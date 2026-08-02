// Package lookup queries external metadata databases so the assistant can
// confirm what a file actually is instead of guessing from its name.
//
// Providers are registered only when their credentials are configured, so a
// server with no API keys simply exposes no lookup tools and the model is told
// nothing it cannot use.
package lookup

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Kind is the category of work being looked up.
type Kind string

const (
	KindMovie   Kind = "movie"
	KindSeries  Kind = "series"
	KindEpisode Kind = "episode"
)

// Valid reports whether k is a recognised kind.
func (k Kind) Valid() bool {
	switch k {
	case KindMovie, KindSeries, KindEpisode:
		return true
	}
	return false
}

// Query describes what to search for. Season and Episode are only meaningful
// for KindEpisode.
type Query struct {
	Kind    Kind   `json:"kind"`
	Title   string `json:"title"`
	Year    int    `json:"year,omitempty"`
	Season  int    `json:"season,omitempty"`
	Episode int    `json:"episode,omitempty"`
}

// Validate checks a query is answerable before any network call is made.
func (q Query) Validate() error {
	if !q.Kind.Valid() {
		return fmt.Errorf("kind must be one of movie, series, episode (got %q)", q.Kind)
	}
	if strings.TrimSpace(q.Title) == "" {
		return errors.New("title is required")
	}
	if q.Kind == KindEpisode && (q.Season <= 0 || q.Episode <= 0) {
		return errors.New("season and episode are required for kind=episode")
	}
	return nil
}

// Match is one candidate result. Fields the source did not supply are zero;
// the model is expected to treat them as unknown rather than empty.
type Match struct {
	Source       string `json:"source"`
	ID           string `json:"id"`
	Kind         Kind   `json:"kind"`
	Title        string `json:"title"`
	Year         int    `json:"year,omitempty"`
	Season       int    `json:"season,omitempty"`
	Episode      int    `json:"episode,omitempty"`
	EpisodeTitle string `json:"episode_title,omitempty"`
	Overview     string `json:"overview,omitempty"`
}

// Provider is one metadata database.
type Provider interface {
	// Name identifies the source in results and logs.
	Name() string
	// Supports reports whether this provider can answer a kind of query.
	Supports(k Kind) bool
	// Search returns candidates, best first. Returning no matches is not an
	// error.
	Search(ctx context.Context, q Query) ([]Match, error)
}

// Registry fans a query out to every provider that supports its kind.
type Registry struct {
	mu        sync.RWMutex
	providers []Provider
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds a provider.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = append(r.providers, p)
}

// Len reports how many providers are configured.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providers)
}

// Names lists the configured provider names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p.Name())
	}
	sort.Strings(out)
	return out
}

// ErrNoProvider is returned when nothing configured can answer a query kind.
var ErrNoProvider = errors.New("no metadata provider configured for that kind")

// Search queries every supporting provider concurrently and merges the
// results. A provider that fails is reported alongside the results that did
// succeed rather than failing the whole lookup — partial metadata still beats
// a hallucinated episode title.
func (r *Registry) Search(ctx context.Context, q Query) ([]Match, []error, error) {
	if err := q.Validate(); err != nil {
		return nil, nil, err
	}

	r.mu.RLock()
	var selected []Provider
	for _, p := range r.providers {
		if p.Supports(q.Kind) {
			selected = append(selected, p)
		}
	}
	r.mu.RUnlock()

	if len(selected) == 0 {
		return nil, nil, ErrNoProvider
	}

	type outcome struct {
		matches []Match
		err     error
	}
	results := make([]outcome, len(selected))

	var wg sync.WaitGroup
	for i, p := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := p.Search(ctx, q)
			if err != nil {
				results[i] = outcome{err: fmt.Errorf("%s: %w", p.Name(), err)}
				return
			}
			results[i] = outcome{matches: m}
		}()
	}
	wg.Wait()

	var matches []Match
	var errs []error
	for _, o := range results {
		if o.err != nil {
			errs = append(errs, o.err)
			continue
		}
		matches = append(matches, o.matches...)
	}
	return matches, errs, nil
}
