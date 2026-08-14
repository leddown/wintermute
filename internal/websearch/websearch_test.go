package websearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewReturnsNilWithoutAnInstance(t *testing.T) {
	// nil is the signal not to offer the tools. Offering a tool that always
	// fails teaches a model to keep trying it.
	if New(Config{}) != nil {
		t.Error("New with no SearXNG URL returned a client")
	}
	if New(Config{SearxURL: "  "}) != nil {
		t.Error("New with a blank URL returned a client")
	}
}

func TestSearch(t *testing.T) {
	var gotQuery, gotFormat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotFormat = r.URL.Query().Get("format")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"DORA text","url":"https://example.org/dora","content":"Digital operational resilience"},
			{"title":"Second","url":"https://example.org/2","content":"other"},
			{"title":"Third","url":"https://example.org/3","content":"other"}
		]}`))
	}))
	defer srv.Close()

	client := New(Config{SearxURL: srv.URL})
	results, err := client.Search(context.Background(), "dora regulation", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery != "dora regulation" || gotFormat != "json" {
		t.Errorf("asked SearXNG q=%q format=%q", gotQuery, gotFormat)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want the limit of 2", len(results))
	}
	if results[0].URL != "https://example.org/dora" {
		t.Errorf("first result = %+v", results[0])
	}
}

// A SearXNG instance without the JSON format enabled answers 403, and the
// error should name the fix rather than the status code.
func TestSearchExplainsAMissingJSONFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := New(Config{SearxURL: srv.URL}).Search(context.Background(), "x", 0)
	if err == nil || !strings.Contains(err.Error(), "settings.yml") {
		t.Fatalf("error = %v, want it to name the SearXNG setting", err)
	}
}

// fetch_url asks the *server* to make a request to an address the model chose.
// The guard is the whole reason that is acceptable.
func TestFetchRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>secret</body></html>"))
	}))
	defer srv.Close()

	client := New(Config{SearxURL: "http://127.0.0.1:9"})
	// The test server is on loopback, which is exactly what the dialer refuses.
	if _, err := client.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("Fetch reached a loopback address")
	} else if !strings.Contains(err.Error(), "private") {
		t.Errorf("error = %v, want it to say the address is private", err)
	}

	for _, bad := range []string{"file:///etc/passwd", "ftp://example.org", "not a url at all", ""} {
		if _, err := client.Fetch(context.Background(), bad); err == nil {
			t.Errorf("Fetch accepted %q", bad)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	page := `<html><head><title>Article 17</title><style>p{color:red}</style></head>
	<body><script>alert(1)</script><h1>Incident reporting</h1>
	<p>Report within 24&nbsp;hours &amp; notify customers.</p></body></html>`

	if got := htmlTitle(page); got != "Article 17" {
		t.Errorf("htmlTitle = %q", got)
	}
	text := collapseBlankLines(htmlToText(page))
	for _, want := range []string{"Incident reporting", "Report within 24 hours & notify customers."} {
		if !strings.Contains(text, want) {
			t.Errorf("text %q is missing %q", text, want)
		}
	}
	for _, unwanted := range []string{"alert(1)", "color:red", "<p>"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("text kept %q:\n%s", unwanted, text)
		}
	}
}

func TestDecodeEntities(t *testing.T) {
	got := decodeEntities("caf&#233; &mdash; 24&nbsp;hours &#x26; more")
	for _, want := range []string{"café", "—", "24 hours", "&"} {
		if !strings.Contains(got, want) {
			t.Errorf("decoded %q is missing %q", got, want)
		}
	}
}
