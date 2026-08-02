package lookup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TVDB queries thetvdb.com's v4 API. TheTVDB has better episode-level data
// than TMDB for long-running series, which is exactly the case where guessing
// a filename goes wrong.
//
// v4 requires exchanging the API key (plus a subscriber PIN, for user-supported
// keys) for a bearer token. The token is long-lived, so it is cached and only
// re-fetched when it expires or the API rejects it.
type TVDB struct {
	apiKey string
	pin    string
	client *http.Client
	base   string

	mu        sync.Mutex
	token     string
	tokenTime time.Time
}

// tokenLifetime is deliberately shorter than TheTVDB's documented one month:
// refreshing early is cheap, and a stale token mid-turn is not.
const tokenLifetime = 7 * 24 * time.Hour

// NewTVDB builds a provider, or returns nil when no API key is configured.
func NewTVDB(apiKey, pin string) *TVDB {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	return &TVDB{
		apiKey: apiKey,
		pin:    strings.TrimSpace(pin),
		client: newHTTPClient(),
		base:   "https://api4.thetvdb.com/v4",
	}
}

// Name implements Provider.
func (t *TVDB) Name() string { return "thetvdb" }

// Supports implements Provider. TheTVDB covers movies too, but its strength is
// television; TMDB is the better movie source, so this provider stays in its lane.
func (t *TVDB) Supports(k Kind) bool { return k == KindSeries || k == KindEpisode }

// Search implements Provider.
func (t *TVDB) Search(ctx context.Context, q Query) ([]Match, error) {
	switch q.Kind {
	case KindSeries:
		series, err := t.searchSeries(ctx, q)
		if err != nil {
			return nil, err
		}
		out := make([]Match, 0, len(series))
		for _, s := range series {
			out = append(out, s.match(t.Name()))
		}
		return limit(out, 5), nil

	case KindEpisode:
		return t.searchEpisode(ctx, q)

	default:
		return nil, fmt.Errorf("unsupported kind %q", q.Kind)
	}
}

type tvdbSeries struct {
	TVDBID   string `json:"tvdb_id"`
	Name     string `json:"name"`
	Year     string `json:"year"`
	Overview string `json:"overview"`
}

func (s tvdbSeries) match(source string) Match {
	year, _ := strconv.Atoi(s.Year)
	return Match{
		Source:   source,
		ID:       s.TVDBID,
		Kind:     KindSeries,
		Title:    s.Name,
		Year:     year,
		Overview: truncate(s.Overview, 400),
	}
}

func (t *TVDB) searchSeries(ctx context.Context, q Query) ([]tvdbSeries, error) {
	params := url.Values{"query": {q.Title}, "type": {"series"}, "limit": {"5"}}
	if q.Year > 0 {
		params.Set("year", strconv.Itoa(q.Year))
	}
	var res struct {
		Data []tvdbSeries `json:"data"`
	}
	if err := t.get(ctx, "/search", params, &res); err != nil {
		return nil, err
	}
	return res.Data, nil
}

func (t *TVDB) searchEpisode(ctx context.Context, q Query) ([]Match, error) {
	series, err := t.searchSeries(ctx, Query{Kind: KindSeries, Title: q.Title, Year: q.Year})
	if err != nil {
		return nil, err
	}
	if len(series) == 0 {
		return nil, nil
	}
	best := series[0]

	// The v4 API validates episodeNumber only alongside season, so both are
	// always sent together.
	params := url.Values{
		"season":        {strconv.Itoa(q.Season)},
		"episodeNumber": {strconv.Itoa(q.Episode)},
		"page":          {"0"},
	}
	var res struct {
		Data struct {
			Episodes []struct {
				Name         string `json:"name"`
				Overview     string `json:"overview"`
				SeasonNumber int    `json:"seasonNumber"`
				Number       int    `json:"number"`
			} `json:"episodes"`
		} `json:"data"`
	}
	if err := t.get(ctx, "/series/"+best.TVDBID+"/episodes/default", params, &res); err != nil {
		var he *httpError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}

	seriesYear, _ := strconv.Atoi(best.Year)
	out := make([]Match, 0, len(res.Data.Episodes))
	for _, ep := range res.Data.Episodes {
		out = append(out, Match{
			Source:       t.Name(),
			ID:           best.TVDBID,
			Kind:         KindEpisode,
			Title:        best.Name,
			Year:         seriesYear,
			Season:       ep.SeasonNumber,
			Episode:      ep.Number,
			EpisodeTitle: ep.Name,
			Overview:     truncate(ep.Overview, 400),
		})
	}
	return limit(out, 5), nil
}

// get performs an authenticated request, retrying once with a fresh token if
// the cached one has been invalidated server-side.
func (t *TVDB) get(ctx context.Context, path string, params url.Values, dst any) error {
	err := t.getOnce(ctx, path, params, dst, false)
	var he *httpError
	if errors.As(err, &he) && he.Status == http.StatusUnauthorized {
		return t.getOnce(ctx, path, params, dst, true)
	}
	return err
}

func (t *TVDB) getOnce(ctx context.Context, path string, params url.Values, dst any, forceLogin bool) error {
	token, err := t.authToken(ctx, forceLogin)
	if err != nil {
		return err
	}
	header := http.Header{"Authorization": {"Bearer " + token}}

	full := t.base + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}
	return getJSON(ctx, t.client, full, header, dst)
}

// authToken returns a cached token, logging in when it is missing, stale, or
// explicitly invalidated.
func (t *TVDB) authToken(ctx context.Context, force bool) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !force && t.token != "" && time.Since(t.tokenTime) < tokenLifetime {
		return t.token, nil
	}

	payload := map[string]string{"apikey": t.apiKey}
	if t.pin != "" {
		payload["pin"] = t.pin
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode login: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/login", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	var res struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := doJSON(t.client, req, &res); err != nil {
		return "", fmt.Errorf("thetvdb login: %w", err)
	}
	if res.Data.Token == "" {
		return "", errors.New("thetvdb login returned no token")
	}

	t.token, t.tokenTime = res.Data.Token, time.Now()
	return t.token, nil
}
