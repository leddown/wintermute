package models

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// hubHandler builds a Hub over a handler of the test's choosing, so a test can
// control status codes and headers rather than only the body.
func hubHandler(t *testing.T, token string, h http.HandlerFunc) *Hub {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewHub(srv.URL, token)
}

// The Hub distinguishes four failures an operator can act on from a fault in
// this server. Collapsing them into one message is what made "hub: 403
// Forbidden" the only diagnosis available for a gated repository, a missing
// token and a typo in a name alike.
func TestHubClassifiesFailuresTheOperatorCanActOn(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"missing", http.StatusNotFound, `{"error":"Repo not found"}`, ErrHubNotFound},
		{"gated", http.StatusForbidden, `{"error":"Access to model is restricted"}`, ErrHubForbidden},
		{"unauthorized", http.StatusUnauthorized, `{"error":"Invalid credentials"}`, ErrHubForbidden},
		{"rejected", http.StatusBadRequest, `{"error":"Error parsing pagination cursor"}`, ErrHubBadRequest},
		{"broken", http.StatusBadGateway, ``, ErrHubUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := hub.Search(context.Background(), SearchOptions{Query: "x"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// The Hub's own explanation is the whole diagnosis; the status line is none of
// it. A stale cursor answered with "400 Bad Request" is unactionable.
func TestHubSurfacesTheHubsOwnExplanation(t *testing.T) {
	hub := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Error parsing pagination cursor"}`))
	})
	_, err := hub.Search(context.Background(), SearchOptions{Query: "x"})
	if err == nil || !strings.Contains(err.Error(), "Error parsing pagination cursor") {
		t.Fatalf("the Hub's message must survive, got %v", err)
	}
}

// Whether a token was sent decides what the operator has to do about a refusal,
// and only this process knows which it was.
func TestHubForbiddenSaysWhetherATokenWasSent(t *testing.T) {
	refuse := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"restricted"}`))
	}

	_, err := hubHandler(t, "", refuse).Search(context.Background(), SearchOptions{Query: "x"})
	if err == nil || !strings.Contains(err.Error(), "no Hugging Face token is configured") {
		t.Errorf("with no token, say so: %v", err)
	}

	_, err = hubHandler(t, "hf_test", refuse).Search(context.Background(), SearchOptions{Query: "x"})
	if err == nil || !strings.Contains(err.Error(), "a token is configured") {
		t.Errorf("with a token, the repository is private or gated: %v", err)
	}
}

// A 429 is routine at 500 calls per five minutes, and the only useful thing to
// say about one is how long the wait is.
func TestHubRateLimitCarriesTheWait(t *testing.T) {
	hub := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("RateLimit", `"api";r=0;t=112`)
		w.Header().Set("RateLimit-Policy", `"fixed window";"api";q=500;w=300`)
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := hub.Search(context.Background(), SearchOptions{Query: "x"})
	if !errors.Is(err, ErrHubRateLimited) {
		t.Fatalf("want a rate limit error, got %v", err)
	}
	var limited *RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("the wait must be recoverable from the error, got %T", err)
	}
	if limited.ResetSeconds != 112 || limited.Bucket != "api" || limited.Quota != 500 {
		t.Errorf("headers not parsed: %+v", limited.RateLimit)
	}
	if !strings.Contains(err.Error(), "112") {
		t.Errorf("the wait belongs in the message: %v", err)
	}
}

// The allowance is read off successful responses too, because the point is to
// show it before it runs out rather than after.
func TestHubRecordsTheAllowanceFromASuccess(t *testing.T) {
	hub := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("RateLimit", `"api";r=497;t=71`)
		w.Header().Set("RateLimit-Policy", `"fixed window";"api";q=500;w=300`)
		_, _ = w.Write([]byte(`[]`))
	})
	if rl := hub.RateLimit(); rl != nil {
		t.Fatalf("nothing has been asked yet, got %+v", rl)
	}
	if _, err := hub.Search(context.Background(), SearchOptions{Query: "x"}); err != nil {
		t.Fatal(err)
	}
	rl := hub.RateLimit()
	if rl == nil || rl.Remaining != 497 || rl.Quota != 500 || rl.WindowSeconds != 300 {
		t.Fatalf("allowance not recorded: %+v", rl)
	}
}

// Only the cursor is taken from the Link header, never the URL in it. The
// header is a string from off the machine, and following it verbatim would let
// whatever answered for the Hub choose this server's next request.
func TestNextCursorTakesTheCursorNotTheURL(t *testing.T) {
	link := `<https://huggingface.co/api/models?limit=2&cursor=eyJhIjoxfQ%3D%3D>; rel="next"`
	if got := nextCursor(link); got != "eyJhIjoxfQ==" {
		t.Errorf("want the decoded cursor value, got %q", got)
	}
	// An attacker-chosen host with no cursor yields nothing to follow.
	if got := nextCursor(`<http://169.254.169.254/latest/meta-data>; rel="next"`); got != "" {
		t.Errorf("a link with no cursor is not a page to fetch, got %q", got)
	}
	if got := nextCursor(`<https://huggingface.co/api/models?cursor=abc>; rel="prev"`); got != "" {
		t.Errorf("only rel=next is a next page, got %q", got)
	}
	if got := nextCursor(""); got != "" {
		t.Errorf("no header, no cursor, got %q", got)
	}
}

// A page is only browsable if the next one can be asked for, and the cursor
// goes back as a parameter this package builds the URL around.
func TestSearchPagesWithTheCursor(t *testing.T) {
	var seen atomic.Value
	hub := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.URL.Query().Get("cursor"))
		w.Header().Set("Link", `<https://huggingface.co/api/models?cursor=page2>; rel="next"`)
		_, _ = w.Write([]byte(`[{"id":"a/b"}]`))
	})

	first, err := hub.Search(context.Background(), SearchOptions{Query: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Next != "page2" {
		t.Fatalf("want the next cursor, got %q", first.Next)
	}
	if _, err := hub.Search(context.Background(), SearchOptions{Query: "x", Cursor: first.Next}); err != nil {
		t.Fatal(err)
	}
	if got := seen.Load(); got != "page2" {
		t.Errorf("the cursor must be sent back, got %v", got)
	}
}

// Without these the only way to reach one publisher's work is to guess words
// that appear in its name.
func TestSearchSendsEveryFilter(t *testing.T) {
	hub, got := hubRecorder(t, `[]`)
	_, err := hub.Search(context.Background(), SearchOptions{
		Query:             "qwen",
		Author:            "unsloth",
		Library:           "gguf",
		PipelineTag:       "text-generation",
		InferenceProvider: "groq",
		Filters:           []string{"license:apache-2.0", "language:en"},
		GGUFOnly:          true,
		Sort:              "trendingScore",
	})
	if err != nil {
		t.Fatal(err)
	}

	q := got.Query()
	for key, want := range map[string]string{
		"search":             "qwen",
		"author":             "unsloth",
		"library":            "gguf",
		"pipeline_tag":       "text-generation",
		"inference_provider": "groq",
		"sort":               "trendingScore",
	} {
		if q.Get(key) != want {
			t.Errorf("%s: want %q, got %q", key, want, q.Get(key))
		}
	}
	for _, want := range []string{"gguf", "license:apache-2.0", "language:en"} {
		if !contains(q["filter"], want) {
			t.Errorf("filter %q missing from %v", want, q["filter"])
		}
	}
}

// GGUFOnly used to be the only filter and appended straight onto the caller's
// slice. A SearchOptions literal is routinely reused, and mutating it would
// have the filter accumulate across calls.
func TestSearchDoesNotMutateTheCallersFilters(t *testing.T) {
	hub, _ := hubRecorder(t, `[]`)
	filters := []string{"license:mit"}
	opts := SearchOptions{Query: "x", GGUFOnly: true, Filters: filters}
	for range 3 {
		if _, err := hub.Search(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
	}
	if len(filters) != 1 || filters[0] != "license:mit" {
		t.Errorf("the caller's slice was written to: %v", filters)
	}
}

// The parameter count decides the fit verdict, and for a repository shipping
// the original weights the only honest source is the safetensors header. The
// fallback reads the number out of the name, which lies whenever the name does.
func TestSearchPrefersSafetensorsOverTheNameGuess(t *testing.T) {
	hub, _ := hubRecorder(t, `[{"id":"someone/not-a-7b-model","safetensors":{"total":8190735360}}]`)
	page, err := hub.Search(context.Background(), SearchOptions{Query: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := page.Models[0].ParamsB; got < 8.1 || got > 8.3 {
		t.Errorf("want the safetensors count of about 8.19B, got %v", got)
	}
}

// The Hub sends this field as an array from a search and as an object keyed by
// provider from the detail endpoint. A decoder written for one fails outright
// on the other, so a single unserved model in a page of results took the whole
// search down with it. Both shapes, decoded by one path.
func TestProvidersDecodeFromBothHubShapes(t *testing.T) {
	array := `[{"id":"a/b","inferenceProviderMapping":[
	  {"provider":"groq","status":"live","task":"conversational"},
	  {"provider":"novita","status":"error","task":"conversational"}]}]`
	hub, _ := hubRecorder(t, array)
	page, err := hub.Search(context.Background(), SearchOptions{Query: "x"})
	if err != nil {
		t.Fatalf("the search shape must decode: %v", err)
	}
	got := page.Models[0].Providers
	if len(got) != 2 || got[0].Name != "groq" || got[1].Status != "error" {
		t.Errorf("array form not mapped: %+v", got)
	}

	// An empty array is what an unserved model sends, and is not an error.
	hub, _ = hubRecorder(t, `[{"id":"a/b","inferenceProviderMapping":[]}]`)
	if _, err := hub.Search(context.Background(), SearchOptions{Query: "x"}); err != nil {
		t.Errorf("an unserved model is ordinary: %v", err)
	}

	object := `{"id":"a/b","siblings":[],"inferenceProviderMapping":{
	  "nscale":{"status":"live","task":"conversational"}}}`
	detail, err := hubStub(t, object).Detail(context.Background(), "a/b", nil, 0)
	if err != nil {
		t.Fatalf("the detail shape must decode: %v", err)
	}
	if len(detail.Providers) != 1 || detail.Providers[0].Name != "nscale" {
		t.Errorf("object form not mapped: %+v", detail.Providers)
	}
}

// Map iteration order is random. Without sorting, two identical requests return
// JSON that differs, which makes the cache look broken and any diff unreadable.
func TestSearchReportsProvidersInAStableOrder(t *testing.T) {
	body := `[{"id":"a/b","inferenceProviderMapping":{
	  "together":{"status":"live","task":"conversational"},
	  "groq":{"status":"live","task":"conversational"},
	  "nscale":{"status":"error","task":"conversational"}}}]`
	for range 5 {
		hub, _ := hubRecorder(t, body)
		page, err := hub.Search(context.Background(), SearchOptions{Query: "x"})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range page.Models[0].Providers {
			names = append(names, p.Name)
		}
		if strings.Join(names, ",") != "groq,nscale,together" {
			t.Fatalf("providers must be sorted, got %v", names)
		}
	}
}

// The detail response does not carry baseModels or downloadsAllTime unless it
// is asked to, so a fact shown on the search card vanished the moment the
// repository was opened. One expansion list, used by both.
func TestDetailAsksForTheSameFactsAsTheSearch(t *testing.T) {
	hub, got := hubRecorder(t, `{"id":"a/b","siblings":[]}`)
	if _, err := hub.Detail(context.Background(), "a/b", nil, 0); err != nil {
		t.Fatal(err)
	}
	asked := got.Query()["expand[]"]
	for _, want := range searchExpand {
		if !contains(asked, want) {
			t.Errorf("detail must expand %q, asked for %v", want, asked)
		}
	}
	if got.Query().Get("blobs") != "true" {
		t.Errorf("detail still needs blobs for the sizes, got %s", got.RawQuery)
	}
}

// Repository ids arrive from search results, tool calls and browser URLs, and
// are interpolated into a path. A ".." in one addresses a different endpoint.
func TestRepoPathRefusesTraversal(t *testing.T) {
	for _, bad := range []string{"", "   ", "../../api/whoami-v2", "a/b/c", "a/../b", "./x", "/"} {
		if got, err := repoPath(bad); err == nil {
			t.Errorf("%q must be refused, got %q", bad, got)
		}
	}
	// Two segments, and one — the Hub's oldest models have no owner.
	for in, want := range map[string]string{
		"unsloth/Qwen3-8B-GGUF": "unsloth/Qwen3-8B-GGUF",
		"gpt2":                  "gpt2",
		" a/b ":                 "a/b",
		// A trailing slash is normalised away rather than refused: it names
		// the same repository.
		"gpt2/": "gpt2",
	} {
		got, err := repoPath(in)
		if err != nil || got != want {
			t.Errorf("%q: want %q, got %q (%v)", in, want, got, err)
		}
	}
}

func TestRevisionPathDefaultsAndConfines(t *testing.T) {
	for in, want := range map[string]string{
		"":       "main",
		"  ":     "main",
		"main":   "main",
		"v0.1.0": "v0.1.0",
		// A pull request ref legitimately carries slashes.
		"refs/pr/3": "refs/pr/3",
	} {
		got, err := revisionPath(in)
		if err != nil || got != want {
			t.Errorf("%q: want %q, got %q (%v)", in, want, got, err)
		}
	}
	for _, bad := range []string{"..", "refs/../../x", "a/./b"} {
		if _, err := revisionPath(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// Opening one repository costs several requests and comparing four does it four
// times. Without the cache, browsing is what exhausts the window.
func TestHubCachesRepeatedReads(t *testing.T) {
	var calls atomic.Int32
	hub := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"id":"a/b","siblings":[]}`))
	})
	for range 4 {
		if _, err := hub.Detail(context.Background(), "a/b", nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("want one request served from cache thereafter, got %d", got)
	}
}

// A failure must not be cached: the repository that was rate-limited a minute
// ago is the one the operator is about to retry.
func TestHubDoesNotCacheFailures(t *testing.T) {
	var calls atomic.Int32
	hub := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	for range 3 {
		if _, err := hub.Detail(context.Background(), "a/b", nil, 0); err == nil {
			t.Fatal("want an error")
		}
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("every retry must reach the Hub, got %d requests", got)
	}
}

func TestTreeReadsSizesDigestsAndPaging(t *testing.T) {
	var got string
	hub := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.String()
		w.Header().Set("Link", `<https://huggingface.co/x?cursor=more>; rel="next"`)
		_, _ = w.Write([]byte(`[
		  {"type":"directory","path":"BF16","size":0},
		  {"type":"file","path":"config.json","size":1234,"oid":"abc"},
		  {"type":"file","path":"m-Q4_K_M.gguf","size":136,
		   "lfs":{"sha256":"deadbeef","size":5027784512},
		   "lastCommit":{"id":"c1","title":"Upload","date":"2026-01-30T06:29:38.000Z"}}
		]`))
	})

	tree, err := hub.Tree(context.Background(), "unsloth/x", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "/api/models/unsloth/x/tree/main") {
		t.Errorf("want the tree endpoint at the default revision, got %s", got)
	}
	if !strings.Contains(got, "recursive=true") || !strings.Contains(got, "expand=true") {
		t.Errorf("the listing must be recursive and expanded, got %s", got)
	}
	if tree.Next != "more" {
		t.Errorf("want a next cursor, got %q", tree.Next)
	}
	if len(tree.Files) != 3 {
		t.Fatalf("want every file, not only the weights, got %d", len(tree.Files))
	}

	weights := tree.Files[2]
	// A pointer file is 136 bytes and the model is five gigabytes. Reporting
	// the pointer's size would understate the download by a factor of millions.
	if weights.Size != 5027784512 {
		t.Errorf("want the LFS size, got %d", weights.Size)
	}
	if weights.SHA256 != "deadbeef" {
		t.Errorf("want the LFS digest, got %q", weights.SHA256)
	}
	if weights.LastCommit == nil || weights.LastCommit.ID != "c1" {
		t.Errorf("the expanded listing carries a commit per file: %+v", weights.LastCommit)
	}
	// A small file kept in git has a blob sha1, which says nothing about the
	// content as this program would hash it, so it is not reported as a digest.
	if tree.Files[1].SHA256 != "" {
		t.Errorf("a git blob oid is not a content digest, got %q", tree.Files[1].SHA256)
	}
}

func TestRefsAndCommitsAreMapped(t *testing.T) {
	hub := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/refs"):
			_, _ = w.Write([]byte(`{"branches":[{"name":"main","ref":"refs/heads/main","targetCommit":"b9"}],
			  "tags":[{"name":"v1","ref":"refs/tags/v1"}],"converts":[]}`))
		default:
			_, _ = w.Write([]byte(`[{"id":"c1","title":"Create LICENSE","date":"2025-07-26T03:49:13.000Z",
			  "authors":[{"user":"someone"}]}]`))
		}
	})

	refs, err := hub.Refs(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs.Branches) != 1 || refs.Branches[0].TargetCommit != "b9" {
		t.Errorf("branches not mapped: %+v", refs.Branches)
	}
	if len(refs.Tags) != 1 || refs.Tags[0].Name != "v1" {
		t.Errorf("tags are what a download is pinned to: %+v", refs.Tags)
	}

	commits, err := hub.Commits(context.Background(), "a/b", "main", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || commits[0].Title != "Create LICENSE" || commits[0].Date.IsZero() {
		t.Fatalf("commits not mapped: %+v", commits)
	}
	if len(commits[0].Authors) != 1 || commits[0].Authors[0] != "someone" {
		t.Errorf("authors not mapped: %+v", commits[0].Authors)
	}
}

// An unfinished scan is not a clean one, and must not be able to read as one.
func TestScanKeepsUnfinishedApartFromClean(t *testing.T) {
	clean := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"scansDone":true,"filesWithIssues":[]}`))
	})
	got, err := clean.Scan(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ScansDone || len(got.FilesWithIssues) != 0 {
		t.Errorf("want a finished, clean scan: %+v", got)
	}

	flagged := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"scansDone":false,"filesWithIssues":[{"path":"p.bin","level":"unsafe"}]}`))
	})
	got, err = flagged.Scan(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if got.ScansDone {
		t.Error("a scan still running must not report as done")
	}
	if len(got.FilesWithIssues) != 1 || got.FilesWithIssues[0].Level != "unsafe" {
		t.Errorf("the flagged file and its level are the point: %+v", got.FilesWithIssues)
	}
}

// The front matter is already parsed into cardData and tags. Left in, the same
// facts appear twice — once as prose and once as machine noise.
func TestCardStripsFrontMatter(t *testing.T) {
	hub := hubHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/resolve/main/README.md") {
			t.Errorf("the card is fetched from the resolve path, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("---\nlicense: apache-2.0\ntags:\n  - gguf\n---\n\n# Qwen3\n\nProse.\n"))
	})
	card, err := hub.Card(context.Background(), "a/b", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(card, "license: apache-2.0") {
		t.Errorf("front matter must be dropped, got %q", card)
	}
	if !strings.HasPrefix(card, "# Qwen3") {
		t.Errorf("the prose must survive, got %q", card)
	}
}

// An unterminated block is a malformed card, not a licence to discard it.
func TestCardKeepsAnUnterminatedFrontMatter(t *testing.T) {
	body := "---\nlicense: apache-2.0\n\n# Heading\n"
	if got := stripFrontMatter(body); got != body {
		t.Errorf("want the card left alone, got %q", got)
	}
	plain := "# Heading\n\nProse.\n"
	if got := stripFrontMatter(plain); got != plain {
		t.Errorf("a card with no front matter is unchanged, got %q", got)
	}
}

func TestWhoAmIReportsTheTokensReach(t *testing.T) {
	// With no token there is nothing to ask about, and the Hub would answer
	// about the anonymous caller rather than failing.
	if _, err := hubHandler(t, "", nil).WhoAmI(context.Background()); !errors.Is(err, ErrHubForbidden) {
		t.Errorf("want a refusal with no token, got %v", err)
	}

	hub := hubHandler(t, "hf_test", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer hf_test" {
			t.Errorf("the token must be sent, got %q", got)
		}
		_, _ = w.Write([]byte(`{"name":"someone","fullname":"Some One","type":"user","isPro":true,
		  "email":"private@example.com",
		  "orgs":[{"name":"an-org"}],
		  "auth":{"accessToken":{"role":"fineGrained","fineGrained":{
		    "global":["inference.serverless.write"],
		    "scoped":[{"permissions":["repo.content.read","repo.read"]}]}}}}`))
	})

	who, err := hub.WhoAmI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if who.Name != "someone" || who.Role != "fineGrained" || !who.IsPro {
		t.Errorf("identity not mapped: %+v", who)
	}
	if len(who.Orgs) != 1 || who.Orgs[0] != "an-org" {
		t.Errorf("orgs decide access to a private repository: %+v", who.Orgs)
	}
	if strings.Join(who.Permissions, ",") != "inference.serverless.write,repo.content.read,repo.read" {
		t.Errorf("want the scopes, sorted and deduped: %v", who.Permissions)
	}
	// Nothing here has any use for the account's email address.
	if strings.Contains(strings.ToLower(who.FullName+who.Name), "@") {
		t.Errorf("the email must not be carried: %+v", who)
	}
}
