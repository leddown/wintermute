package lookup

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// OMDb queries omdbapi.com, which is IMDb-derived. It is registered for movies
// only: TMDB and TheTVDB cover television better, and OMDb's value here is a
// second opinion on film titles and years from a different data lineage.
type OMDb struct {
	key    string
	client *http.Client
	base   string
}

// NewOMDb builds a provider, or returns nil when no API key is configured.
func NewOMDb(key string) *OMDb {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	return &OMDb{
		key:    key,
		client: newHTTPClient(),
		base:   "https://www.omdbapi.com/",
	}
}

// Name implements Provider.
func (o *OMDb) Name() string { return "omdb" }

// Supports implements Provider.
func (o *OMDb) Supports(k Kind) bool { return k == KindMovie }

// omdbSearchResponse is the shape returned by the `s=` (search) parameter.
// OMDb signals failure in the body with Response:"False" and HTTP 200, so the
// Error field must be checked explicitly.
type omdbSearchResponse struct {
	Search []struct {
		Title  string `json:"Title"`
		Year   string `json:"Year"`
		IMDbID string `json:"imdbID"`
		Type   string `json:"Type"`
	} `json:"Search"`
	Response string `json:"Response"`
	Error    string `json:"Error"`
}

// Search implements Provider.
func (o *OMDb) Search(ctx context.Context, q Query) ([]Match, error) {
	if q.Kind != KindMovie {
		return nil, fmt.Errorf("unsupported kind %q", q.Kind)
	}

	params := url.Values{
		"apikey": {o.key},
		"s":      {q.Title},
		"type":   {"movie"},
	}
	if q.Year > 0 {
		params.Set("y", strconv.Itoa(q.Year))
	}

	var res omdbSearchResponse
	if err := getJSON(ctx, o.client, o.base+"?"+params.Encode(), nil, &res); err != nil {
		return nil, err
	}
	if strings.EqualFold(res.Response, "False") {
		// "Movie not found!" is an answer, not a failure. Anything else —
		// an invalid key, an exhausted quota — is worth surfacing.
		if strings.Contains(strings.ToLower(res.Error), "not found") {
			return nil, nil
		}
		return nil, fmt.Errorf("omdb: %s", res.Error)
	}

	out := make([]Match, 0, len(res.Search))
	for _, r := range res.Search {
		out = append(out, Match{
			Source: o.Name(),
			ID:     r.IMDbID,
			Kind:   KindMovie,
			Title:  r.Title,
			// OMDb returns ranges like "2019–2021" for some entries; only the
			// leading year is meaningful for a movie.
			Year: yearOf(r.Year),
		})
	}
	return limit(out, 5), nil
}
