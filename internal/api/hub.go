package api

// Browsing the Hugging Face Hub.
//
// Every route here is a GET that proxies a read. Nothing in this file writes to
// the Hub, and nothing in it starts a download — the download endpoints live in
// modelrepo.go and stay where they are, for the reason set out at the top of
// that file: fetching gigabytes over somebody's connection is an operator
// decision, and reading about a model is not.
//
// Proxied rather than called from the browser, for three reasons. The token
// must not reach the client. The results are graded against this host's
// hardware, which only the server can do. And the Hub meters requests against
// the caller's address, so one server making cached requests on behalf of
// several browsers is the arrangement that stays inside the allowance — see
// models.RateLimit.
//
// The path shape is verb-first — /hub/tree/{id...} rather than
// /hub/models/{id...}/tree — because a repository id contains a slash, which
// makes it a trailing wildcard, and Go's mux requires those to be last.

import (
	"errors"
	"net/http"
	"strconv"

	"wintermute/internal/models"
)

func (s *Server) registerHubRoutes(authed func(string, http.HandlerFunc)) {
	if s.catalog == nil {
		return
	}
	authed("GET /api/v1/hub/search", s.handleHubSearch)
	authed("GET /api/v1/hub/tags", s.handleHubTags)
	authed("GET /api/v1/hub/status", s.handleHubStatus)
	authed("GET /api/v1/hub/whoami", s.handleHubWhoAmI)
	authed("GET /api/v1/hub/detail/{id...}", s.handleHubDetail)
	authed("GET /api/v1/hub/tree/{id...}", s.handleHubTree)
	authed("GET /api/v1/hub/refs/{id...}", s.handleHubRefs)
	authed("GET /api/v1/hub/commits/{id...}", s.handleHubCommits)
	authed("GET /api/v1/hub/scan/{id...}", s.handleHubScan)
	authed("GET /api/v1/hub/card/{id...}", s.handleHubCard)
}

// handleHubSearch browses the Hub.
//
// Unlike /api/v1/models/search, which exists to answer one question for the
// Repository screen, this one carries the whole filter set and pages.
func (s *Server) handleHubSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// GGUF-only defaults to on. Everything this server can actually load is a
	// GGUF, so an unfiltered search is mostly results the operator cannot use;
	// the browser turns it off explicitly to go looking at original weights.
	opts := models.SearchOptions{
		Query:             q.Get("q"),
		Limit:             queryInt(r, "limit", 20),
		GGUFOnly:          q.Get("gguf") != "false",
		Sort:              q.Get("sort"),
		Author:            q.Get("author"),
		Library:           q.Get("library"),
		PipelineTag:       q.Get("pipeline_tag"),
		InferenceProvider: q.Get("inference_provider"),
		Filters:           q["filter"],
		Cursor:            q.Get("cursor"),
		Hosts:             s.catalog.Hosts(r.Context()),
		ContextTokens:     queryInt(r, "context", defaultPlanContext),
	}
	// A search with no terms at all is a browse of the whole Hub by whatever
	// sort was asked for, which is a legitimate thing to want. It is only empty
	// of meaning when nothing narrows it in any direction.
	if opts.Query == "" && opts.Author == "" && opts.PipelineTag == "" &&
		opts.Library == "" && opts.InferenceProvider == "" && len(opts.Filters) == 0 && opts.Sort == "" {
		opts.Sort = "trendingScore"
	}

	page, err := s.catalog.Hub().Search(r.Context(), opts)
	if err != nil {
		s.failHub(w, "hub search", err)
		return
	}
	results := page.Models
	if results == nil {
		results = []models.HubModel{}
	}
	// next is an opaque cursor, never a URL — see models.nextCursor. The
	// browser hands it back verbatim and there is nothing in it this server
	// would follow.
	s.writeHub(w, map[string]any{"results": results, "next": page.Next})
}

func (s *Server) handleHubDetail(w http.ResponseWriter, r *http.Request) {
	detail, err := s.catalog.Hub().Detail(r.Context(), r.PathValue("id"),
		s.catalog.Hosts(r.Context()), queryInt(r, "context", defaultPlanContext))
	if err != nil {
		s.failHub(w, "hub detail", err)
		return
	}
	s.writeHub(w, map[string]any{"model": detail})
}

// handleHubTree lists every file in a repository, not only the weights.
func (s *Server) handleHubTree(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tree, err := s.catalog.Hub().Tree(r.Context(), r.PathValue("id"),
		q.Get("revision"), q.Get("path"), q.Get("cursor"))
	if err != nil {
		s.failHub(w, "hub tree", err)
		return
	}
	s.writeHub(w, map[string]any{"files": tree.Files, "next": tree.Next})
}

// handleHubRefs lists the branches and tags a download can be pinned to.
func (s *Server) handleHubRefs(w http.ResponseWriter, r *http.Request) {
	refs, err := s.catalog.Hub().Refs(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failHub(w, "hub refs", err)
		return
	}
	s.writeHub(w, map[string]any{"refs": refs})
}

func (s *Server) handleHubCommits(w http.ResponseWriter, r *http.Request) {
	commits, err := s.catalog.Hub().Commits(r.Context(), r.PathValue("id"),
		r.URL.Query().Get("revision"), queryInt(r, "limit", 20))
	if err != nil {
		s.failHub(w, "hub commits", err)
		return
	}
	if commits == nil {
		commits = []models.HubCommit{}
	}
	s.writeHub(w, map[string]any{"commits": commits})
}

// handleHubScan reports Hugging Face's own security scan of a repository.
func (s *Server) handleHubScan(w http.ResponseWriter, r *http.Request) {
	scan, err := s.catalog.Hub().Scan(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failHub(w, "hub scan", err)
		return
	}
	s.writeHub(w, map[string]any{"scan": scan})
}

// handleHubCard returns a repository's model card as Markdown.
//
// It is prose written by whoever published the repository, which is to say it
// is untrusted text this server is merely relaying. It is sent as a string in a
// JSON field rather than as a document, so nothing downstream can mistake it
// for something to render as HTML.
func (s *Server) handleHubCard(w http.ResponseWriter, r *http.Request) {
	card, err := s.catalog.Hub().Card(r.Context(), r.PathValue("id"), r.URL.Query().Get("revision"))
	if err != nil {
		s.failHub(w, "hub card", err)
		return
	}
	s.writeHub(w, map[string]any{"card": card})
}

// handleHubTags fetches the Hub's own filter vocabulary, so the browser offers
// what the Hub actually indexes rather than a list that ages in place.
func (s *Server) handleHubTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.catalog.Hub().Tags(r.Context())
	if err != nil {
		s.failHub(w, "hub tags", err)
		return
	}
	s.writeHub(w, map[string]any{"tags": tags})
}

// handleHubStatus reports whether a token is configured and what allowance is
// left. It makes no upstream request, so it stays answerable when the Hub is
// unreachable or the window is spent — which is exactly when it is worth
// asking.
func (s *Server) handleHubStatus(w http.ResponseWriter, r *http.Request) {
	// Which machines the verdicts on this screen are about. Without it a page
	// of "unknown" badges looks like a broken estimator rather than what it
	// is: nothing has been declared as the machine that runs the models.
	hosts := s.catalog.Hosts(r.Context())
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h.Host == "" {
			names = append(names, "this server")
			continue
		}
		names = append(names, h.Host)
	}
	s.writeHub(w, map[string]any{
		"has_token": s.catalog.Hub().HasToken(),
		"fit_hosts": names,
	})
}

// handleHubWhoAmI reports who the configured token belongs to and what it may
// do. A read token cannot be told from a write one by looking at it.
func (s *Server) handleHubWhoAmI(w http.ResponseWriter, r *http.Request) {
	who, err := s.catalog.Hub().WhoAmI(r.Context())
	if err != nil {
		s.failHub(w, "hub whoami", err)
		return
	}
	s.writeHub(w, map[string]any{"identity": who})
}

// writeHub answers with the payload and the allowance the Hub last reported.
//
// Attached to every response rather than offered on its own endpoint because
// the figure only matters while browsing, and a UI that had to poll for it
// would spend the very budget it was trying to watch.
func (s *Server) writeHub(w http.ResponseWriter, payload map[string]any) {
	if rl := s.catalog.Hub().RateLimit(); rl != nil {
		payload["rate_limit"] = rl
	}
	writeJSON(w, http.StatusOK, payload)
}

// failHub distinguishes what the Hub said from what this server got wrong.
//
// The same reasoning as failRepo: a gated repository, a mistyped name and a
// spent window are all facts about the world that the operator can act on, and
// answering any of them with "internal error" — the right answer for a genuine
// fault — would make the whole screen undiagnosable.
//
// The status codes describe the upstream condition, not this server's own
// authentication; the browser's api() helper shows the message either way and
// does not treat a 403 as being logged out.
func (s *Server) failHub(w http.ResponseWriter, what string, err error) {
	var limited *models.RateLimitError
	if errors.As(err, &limited) {
		// Retry-After is the machine-readable half of the message. The Hub
		// tells us how long the wait is, so nothing downstream has to guess.
		if limited.ResetSeconds > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(limited.ResetSeconds))
		}
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}

	switch {
	case errors.Is(err, models.ErrHubNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, models.ErrHubForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, models.ErrHubBadRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, models.ErrHubUnavailable):
		// A bad gateway, because that is what it is: the far end is down and
		// nothing here is broken.
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		s.fail(w, what, err)
	}
}
