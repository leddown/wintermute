package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wintermute/internal/agent"
	"wintermute/internal/llm"
	"wintermute/internal/models"
	"wintermute/internal/store"
	"wintermute/internal/store/storetest"
	"wintermute/internal/tool"
)

// hubServer wires a server whose Hub points at a stub of the test's choosing,
// and returns a helper making authenticated calls plus the last URL the stub
// was asked for.
func hubServer(t *testing.T, token string, upstream http.HandlerFunc) (func(path string) *httptest.ResponseRecorder, *string) {
	t.Helper()

	var asked string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.String()
		upstream(w, r)
	}))
	t.Cleanup(hub.Close)

	st := storetest.New(t)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router, err := llm.NewRouter([]*llm.Backend{{Name: "local", Provider: stubProvider{}}}, "local", "", log)
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	ag := agent.New(router, st, tools, log, 4)
	cat := models.NewCatalog(nil, st, models.NewHub(hub.URL, token), log)
	srv := New(ag, st, tools, cat, Workspace{}, ServerInfo{}, log)

	_, clientToken, err := st.CreateClient(t.Context(), "laptop", store.KindHarness)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()

	call := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+clientToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	return call, &asked
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

// The browser cannot reach the Hub itself — it has no token, it would be
// metered against its own address, and it cannot grade a result against this
// host's hardware. So the filters have to survive the proxy hop.
func TestHubSearchPassesEveryFilterUpstream(t *testing.T) {
	call, asked := hubServer(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://huggingface.co/api/models?cursor=next2>; rel="next"`)
		w.Header().Set("RateLimit", `"api";r=412;t=98`)
		w.Header().Set("RateLimit-Policy", `"fixed window";"api";q=500;w=300`)
		_, _ = w.Write([]byte(`[{"id":"unsloth/x"}]`))
	})

	rec := call("/api/v1/hub/search?q=qwen&author=unsloth&library=gguf" +
		"&pipeline_tag=text-generation&filter=license:apache-2.0&filter=language:en" +
		"&inference_provider=groq&sort=likes&limit=5&cursor=page2")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	for _, want := range []string{
		"search=qwen", "author=unsloth", "library=gguf", "pipeline_tag=text-generation",
		"inference_provider=groq", "sort=likes", "limit=5", "cursor=page2",
		"license%3Aapache-2.0", "language%3Aen",
	} {
		if !strings.Contains(*asked, want) {
			t.Errorf("%q missing from the upstream request %s", want, *asked)
		}
	}

	body := decodeBody(t, rec)
	if body["next"] != "next2" {
		t.Errorf("the next cursor must reach the browser, got %v", body["next"])
	}
	// The allowance rides on the response rather than needing an endpoint of
	// its own, which would spend the budget it exists to watch.
	limit, ok := body["rate_limit"].(map[string]any)
	if !ok || limit["remaining"] != float64(412) || limit["quota"] != float64(500) {
		t.Errorf("want the allowance attached, got %v", body["rate_limit"])
	}
}

// GGUF is what these backends can load, so it is the default; going looking at
// original weights is the deliberate act.
func TestHubSearchDefaultsToGGUFAndCanBeTurnedOff(t *testing.T) {
	call, asked := hubServer(t, "", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})

	call("/api/v1/hub/search?q=qwen")
	if !strings.Contains(*asked, "filter=gguf") {
		t.Errorf("want the GGUF filter by default, got %s", *asked)
	}
	call("/api/v1/hub/search?q=qwen&gguf=false")
	if strings.Contains(*asked, "filter=gguf") {
		t.Errorf("gguf=false must lift the filter, got %s", *asked)
	}
}

// A search with nothing narrowing it is a browse, and the useful default order
// for that is what is being downloaded now rather than all-time popularity.
func TestHubSearchWithNoTermsBrowsesByTrend(t *testing.T) {
	call, asked := hubServer(t, "", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	call("/api/v1/hub/search")
	if !strings.Contains(*asked, "sort=trendingScore") {
		t.Errorf("want a trending browse, got %s", *asked)
	}
	// An author on its own is a narrowing, so the usual order applies.
	call("/api/v1/hub/search?author=unsloth")
	if !strings.Contains(*asked, "sort=downloads") {
		t.Errorf("want the default order once something narrows it, got %s", *asked)
	}
}

// Each of these is a fact about the world the operator can act on, and
// answering any of them with "internal error" makes the screen undiagnosable.
func TestHubFailuresKeepTheirMeaning(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		want     int
		contains string
	}{
		{"missing", http.StatusNotFound, `{"error":"Repo not found"}`,
			http.StatusNotFound, "Repo not found"},
		{"gated", http.StatusForbidden, `{"error":"Access restricted"}`,
			http.StatusForbidden, "Access restricted"},
		{"rejected", http.StatusBadRequest, `{"error":"Error parsing pagination cursor"}`,
			http.StatusBadRequest, "pagination cursor"},
		{"upstream down", http.StatusBadGateway, ``,
			http.StatusBadGateway, "not answering"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, _ := hubServer(t, "", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			rec := call("/api/v1/hub/detail/a/b")
			if rec.Code != tc.want {
				t.Fatalf("want %d, got %d: %s", tc.want, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.contains) {
				t.Errorf("want %q in the message, got %s", tc.contains, rec.Body.String())
			}
		})
	}
}

// A spent window is routine, and the one useful thing to say about it is how
// long the wait is — in the message for a person, in Retry-After for anything
// that has to decide when to try again.
func TestHubRateLimitIsPassedOnWithTheWait(t *testing.T) {
	call, _ := hubServer(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("RateLimit", `"api";r=0;t=143`)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	rec := call("/api/v1/hub/search?q=x")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "143" {
		t.Errorf("want Retry-After 143, got %q", got)
	}
	if !strings.Contains(rec.Body.String(), "143") {
		t.Errorf("the wait belongs in the message: %s", rec.Body.String())
	}
}

func TestHubRepositoryViewsAreProxied(t *testing.T) {
	call, asked := hubServer(t, "", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(`[{"type":"file","path":"m.gguf","size":9,
			  "lfs":{"sha256":"beef","size":4096}}]`))
		case strings.Contains(r.URL.Path, "/refs"):
			_, _ = w.Write([]byte(`{"branches":[{"name":"main","targetCommit":"c0"}],"tags":[]}`))
		case strings.Contains(r.URL.Path, "/commits/"):
			_, _ = w.Write([]byte(`[{"id":"c0","title":"Upload","date":"2026-01-30T06:29:38.000Z"}]`))
		case strings.Contains(r.URL.Path, "/scan"):
			_, _ = w.Write([]byte(`{"scansDone":true,"filesWithIssues":[]}`))
		case strings.HasSuffix(r.URL.Path, "README.md"):
			_, _ = w.Write([]byte("---\nlicense: mit\n---\n\n# Card\n"))
		default:
			_, _ = w.Write([]byte(`{"id":"a/b","siblings":[]}`))
		}
	})

	t.Run("tree", func(t *testing.T) {
		body := decodeBody(t, call("/api/v1/hub/tree/unsloth/x?revision=main"))
		files, _ := body["files"].([]any)
		if len(files) != 1 {
			t.Fatalf("want the file list, got %v", body)
		}
		first, _ := files[0].(map[string]any)
		// The pointer is nine bytes and the model is four kilobytes; reporting
		// the pointer's size would understate every download.
		if first["size"] != float64(4096) || first["sha256"] != "beef" {
			t.Errorf("LFS facts lost in the proxy: %v", first)
		}
		if !strings.Contains(*asked, "/tree/main") {
			t.Errorf("the revision must reach the Hub, got %s", *asked)
		}
	})

	t.Run("refs", func(t *testing.T) {
		body := decodeBody(t, call("/api/v1/hub/refs/unsloth/x"))
		refs, _ := body["refs"].(map[string]any)
		branches, _ := refs["branches"].([]any)
		if len(branches) != 1 {
			t.Fatalf("want the branch a download can be pinned to, got %v", body)
		}
	})

	t.Run("commits", func(t *testing.T) {
		body := decodeBody(t, call("/api/v1/hub/commits/unsloth/x?limit=3"))
		commits, _ := body["commits"].([]any)
		if len(commits) != 1 {
			t.Fatalf("want the history, got %v", body)
		}
	})

	t.Run("scan", func(t *testing.T) {
		body := decodeBody(t, call("/api/v1/hub/scan/unsloth/x"))
		scan, _ := body["scan"].(map[string]any)
		if scan["scans_done"] != true {
			t.Fatalf("want the scan verdict, got %v", body)
		}
	})

	t.Run("card", func(t *testing.T) {
		body := decodeBody(t, call("/api/v1/hub/card/unsloth/x"))
		// A string in a JSON field, so nothing downstream can mistake untrusted
		// prose from a stranger's repository for a document to render.
		card, ok := body["card"].(string)
		if !ok || !strings.HasPrefix(card, "# Card") {
			t.Fatalf("want the card as a string with its front matter dropped, got %v", body["card"])
		}
	})
}

// The whole point of proxying is that the token stays here.
func TestHubNeverLeaksTheToken(t *testing.T) {
	call, _ := hubServer(t, "hf_secret_value", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer hf_secret_value" {
			t.Errorf("the token must reach the Hub, got %q", got)
		}
		_, _ = w.Write([]byte(`{"name":"someone","type":"user",
		  "auth":{"accessToken":{"role":"read"}}}`))
	})

	for _, path := range []string{"/api/v1/hub/status", "/api/v1/hub/whoami"} {
		rec := call(path)
		if strings.Contains(rec.Body.String(), "hf_secret_value") {
			t.Fatalf("%s leaked the token: %s", path, rec.Body.String())
		}
	}

	body := decodeBody(t, call("/api/v1/hub/whoami"))
	identity, _ := body["identity"].(map[string]any)
	if identity["role"] != "read" {
		t.Errorf("a read token cannot be told from a write one by looking at it: %v", identity)
	}
}

// Status makes no upstream request, so it keeps answering when the Hub is
// unreachable or the window is spent — which is when it is worth asking.
func TestHubStatusNeedsNoUpstreamCall(t *testing.T) {
	var reached bool
	call, _ := hubServer(t, "", func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	body := decodeBody(t, call("/api/v1/hub/status"))
	if reached {
		t.Error("status must not call the Hub")
	}
	if body["has_token"] != false {
		t.Errorf("want has_token false with no token configured, got %v", body["has_token"])
	}
}

// Without a token there is nothing to ask about, and the Hub would answer about
// the anonymous caller rather than failing — which would read as an identity.
func TestHubWhoAmIRefusesWithoutAToken(t *testing.T) {
	call, _ := hubServer(t, "", func(w http.ResponseWriter, r *http.Request) {
		t.Error("whoami must not reach the Hub with no token configured")
	})
	if rec := call("/api/v1/hub/whoami"); rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Every route is behind the same bearer token as the rest of the API.
func TestHubRoutesRequireAuthentication(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{
		"/api/v1/hub/search?q=x", "/api/v1/hub/detail/a/b", "/api/v1/hub/tree/a/b",
		"/api/v1/hub/refs/a/b", "/api/v1/hub/commits/a/b", "/api/v1/hub/scan/a/b",
		"/api/v1/hub/card/a/b", "/api/v1/hub/tags", "/api/v1/hub/status", "/api/v1/hub/whoami",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d", path, rec.Code)
		}
	}
}

// A repository id is a path fragment from outside. A traversal in one must be
// refused here rather than reaching a different Hub endpoint.
func TestHubRefusesTraversalInRepositoryIDs(t *testing.T) {
	call, _ := hubServer(t, "", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "whoami") {
			t.Errorf("a traversal reached %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"a/b","siblings":[]}`))
	})
	// Go's mux cleans "..", so the surviving shape is an id with too many
	// segments; either way it must not be forwarded as a repository.
	rec := call("/api/v1/hub/detail/a/b/c/d")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a malformed id, got %d: %s", rec.Code, rec.Body.String())
	}
}
