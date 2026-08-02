package lookup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// TMDB queries themoviedb.org. It accepts either a v3 API key (passed as a
// query parameter) or a v4 read access token (passed as a bearer header);
// which one you have depends on when the account was created, so both are
// supported and told apart by shape.
type TMDB struct {
	key    string
	bearer bool
	client *http.Client
	base   string
}

// NewTMDB builds a provider. It returns nil when no key is configured, so the
// caller can register it unconditionally.
func NewTMDB(key string) *TMDB {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	// v4 tokens are JWTs; v3 keys are 32 hex characters.
	return &TMDB{
		key:    key,
		bearer: strings.HasPrefix(key, "ey"),
		client: newHTTPClient(),
		base:   "https://api.themoviedb.org/3",
	}
}

// Name implements Provider.
func (t *TMDB) Name() string { return "tmdb" }

// Supports implements Provider.
func (t *TMDB) Supports(k Kind) bool { return k == KindMovie || k == KindSeries || k == KindEpisode }

func (t *TMDB) request(ctx context.Context, path string, q url.Values, dst any) error {
	if q == nil {
		q = url.Values{}
	}
	header := http.Header{}
	if t.bearer {
		header.Set("Authorization", "Bearer "+t.key)
	} else {
		q.Set("api_key", t.key)
	}
	return getJSON(ctx, t.client, t.base+path+"?"+q.Encode(), header, dst)
}

// Search implements Provider.
func (t *TMDB) Search(ctx context.Context, q Query) ([]Match, error) {
	switch q.Kind {
	case KindMovie:
		return t.searchMovie(ctx, q)
	case KindSeries:
		return t.searchSeries(ctx, q)
	case KindEpisode:
		return t.searchEpisode(ctx, q)
	default:
		return nil, fmt.Errorf("unsupported kind %q", q.Kind)
	}
}

type tmdbMovieResults struct {
	Results []struct {
		ID          int    `json:"id"`
		Title       string `json:"title"`
		ReleaseDate string `json:"release_date"`
		Overview    string `json:"overview"`
	} `json:"results"`
}

func (t *TMDB) searchMovie(ctx context.Context, q Query) ([]Match, error) {
	params := url.Values{"query": {q.Title}, "include_adult": {"false"}}
	if q.Year > 0 {
		params.Set("year", strconv.Itoa(q.Year))
	}
	var res tmdbMovieResults
	if err := t.request(ctx, "/search/movie", params, &res); err != nil {
		return nil, err
	}

	out := make([]Match, 0, len(res.Results))
	for _, r := range res.Results {
		out = append(out, Match{
			Source:   t.Name(),
			ID:       strconv.Itoa(r.ID),
			Kind:     KindMovie,
			Title:    r.Title,
			Year:     yearOf(r.ReleaseDate),
			Overview: truncate(r.Overview, 400),
		})
	}
	return limit(out, 5), nil
}

type tmdbSeriesResults struct {
	Results []struct {
		ID           int    `json:"id"`
		Name         string `json:"name"`
		FirstAirDate string `json:"first_air_date"`
		Overview     string `json:"overview"`
	} `json:"results"`
}

func (t *TMDB) searchSeries(ctx context.Context, q Query) ([]Match, error) {
	res, err := t.seriesResults(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]Match, 0, len(res.Results))
	for _, r := range res.Results {
		out = append(out, Match{
			Source:   t.Name(),
			ID:       strconv.Itoa(r.ID),
			Kind:     KindSeries,
			Title:    r.Name,
			Year:     yearOf(r.FirstAirDate),
			Overview: truncate(r.Overview, 400),
		})
	}
	return limit(out, 5), nil
}

func (t *TMDB) seriesResults(ctx context.Context, q Query) (*tmdbSeriesResults, error) {
	params := url.Values{"query": {q.Title}, "include_adult": {"false"}}
	if q.Year > 0 {
		params.Set("first_air_date_year", strconv.Itoa(q.Year))
	}
	var res tmdbSeriesResults
	if err := t.request(ctx, "/search/tv", params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

type tmdbEpisode struct {
	Name          string `json:"name"`
	Overview      string `json:"overview"`
	AirDate       string `json:"air_date"`
	SeasonNumber  int    `json:"season_number"`
	EpisodeNumber int    `json:"episode_number"`
}

// searchEpisode resolves the series first, then the specific episode. Only the
// best-matching series is followed: querying every candidate would multiply
// requests against the rate limit for results the model would discard anyway.
func (t *TMDB) searchEpisode(ctx context.Context, q Query) ([]Match, error) {
	series, err := t.seriesResults(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(series.Results) == 0 {
		return nil, nil
	}
	best := series.Results[0]

	var ep tmdbEpisode
	path := fmt.Sprintf("/tv/%d/season/%d/episode/%d", best.ID, q.Season, q.Episode)
	if err := t.request(ctx, path, nil, &ep); err != nil {
		// A missing episode is a legitimate answer, not a failure: the model
		// should be told the numbering doesn't exist rather than shown an error.
		var he *httpError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}

	return []Match{{
		Source:       t.Name(),
		ID:           strconv.Itoa(best.ID),
		Kind:         KindEpisode,
		Title:        best.Name,
		Year:         yearOf(best.FirstAirDate),
		Season:       q.Season,
		Episode:      q.Episode,
		EpisodeTitle: ep.Name,
		Overview:     truncate(ep.Overview, 400),
	}}, nil
}

func limit[T any](s []T, n int) []T {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
