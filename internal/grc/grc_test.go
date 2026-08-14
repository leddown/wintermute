package grc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wintermute/internal/tool"
)

// fakeGRC serves the shape grc/internal/knowledge produces.
func fakeGRC(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	token := new(string)
	mux := http.NewServeMux()

	mux.HandleFunc("/api/knowledge/overview", func(w http.ResponseWriter, r *http.Request) {
		*token = r.Header.Get("X-Knowledge-Token")
		writeJSON(w, map[string]any{
			"counts":           map[string]int{"nfr": 109, "control": 1193, "risk": 4},
			"nfr_domains":      []map[string]any{{"name": "Network Security", "count": 12}},
			"control_families": []map[string]any{{"name": "SC", "count": 162}},
			"note":             "Counting questions should use the full NFR index.",
		})
	})

	mux.HandleFunc("/api/knowledge/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("kind") != "nfr" {
			t.Errorf("search kind = %q", r.URL.Query().Get("kind"))
		}
		writeJSON(w, map[string]any{
			"kind": "nfr", "query": r.URL.Query().Get("q"),
			"terms":         []string{"network", "segmentation"},
			"total_matches": 26, "total_all_terms": 3, "returned": 2, "truncated": true,
			"note": "Lexical search finds wording rather than meaning.",
			"hits": []map[string]any{
				{"item": map[string]any{"kind": "nfr", "ref": "61", "title": "Network-level controls",
					"group": "Network Security", "summary": "Prevent components sharing a subnet from talking",
					"related": []string{"SC-7"}},
					"score": 16.5, "matched": []string{"network", "segmentation"}},
				{"item": map[string]any{"kind": "nfr", "ref": "67", "title": "Internal connections",
					"group": "Network Security"},
					"score": 8.0, "matched": []string{"network"}},
			},
		})
	})

	mux.HandleFunc("/api/knowledge/item", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") == "missing" {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"error": `nfr "missing" not found`})
			return
		}
		writeJSON(w, map[string]any{
			"kind": "control", "ref": "SC-7", "title": "Boundary Protection",
			"group": "SC", "body": "Monitor and control communications at external boundaries.",
			"fields": map[string]string{"family": "SC", "baselines": "low, moderate, high"},
			"url":    "/controls?control=SC-7",
		})
	})

	mux.HandleFunc("/api/knowledge/index/nfr", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"kind": "nfr", "count": 2, "items": []map[string]any{
			{"kind": "nfr", "ref": "61", "title": "Network-level controls", "group": "Network Security",
				"fields": map[string]string{"nist_mapping": "SC-7"}},
			{"kind": "nfr", "ref": "62", "title": "Component separation", "group": "Network Security"},
		}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, token
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func TestNewReturnsNilWithoutABaseURL(t *testing.T) {
	if New(Config{Token: "x"}) != nil {
		t.Error("New with no base URL returned a client")
	}
}

func TestClientSendsTheToken(t *testing.T) {
	srv, seen := fakeGRC(t)
	client := New(Config{BaseURL: srv.URL, Token: "know-tok"})
	if _, err := client.Overview(context.Background()); err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if *seen != "know-tok" {
		t.Errorf("token sent = %q", *seen)
	}
}

func TestUnauthorizedIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := New(Config{BaseURL: srv.URL}).Overview(context.Background())
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("error = %v, want it to name the token", err)
	}
}

// The tool output is what the model reads, so what it says is the interface.
// The counts especially: reporting 26 as though it were 3 is the difference
// between an answer and an overstatement.
func TestSearchToolReportsBothCounts(t *testing.T) {
	srv, _ := fakeGRC(t)
	reg := tool.NewRegistry()
	if err := Register(reg, New(Config{BaseURL: srv.URL})); err != nil {
		t.Fatalf("Register: %v", err)
	}

	handler, ok := reg.Handler("grc_search")
	if !ok {
		t.Fatal("grc_search was not registered")
	}
	out, err := handler(context.Background(), json.RawMessage(`{"kind":"nfr","query":"network segmentation"}`))
	if err != nil {
		t.Fatalf("grc_search: %v", err)
	}

	for _, want := range []string{
		"26 record(s) matched at least one term; 3 matched every term",
		"61 | Network-level controls",
		"matched: network, segmentation",
		"related: SC-7",
		"further match(es) not shown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tool output is missing %q\n%s", want, out)
		}
	}
}

func TestToolsAreNotRegisteredWithoutAClient(t *testing.T) {
	reg := tool.NewRegistry()
	if err := Register(reg, nil); err != nil {
		t.Fatalf("Register(nil): %v", err)
	}
	if n := len(reg.Definitions()); n != 0 {
		t.Errorf("registered %d tools with no GRC configured", n)
	}
}

func TestGetToolRendersARecord(t *testing.T) {
	srv, _ := fakeGRC(t)
	reg := tool.NewRegistry()
	if err := Register(reg, New(Config{BaseURL: srv.URL})); err != nil {
		t.Fatalf("Register: %v", err)
	}

	handler, _ := reg.Handler("grc_get")
	out, err := handler(context.Background(), json.RawMessage(`{"kind":"control","ref":"SC-7"}`))
	if err != nil {
		t.Fatalf("grc_get: %v", err)
	}
	for _, want := range []string{"control SC-7 — Boundary Protection", "baselines: low, moderate, high",
		"Monitor and control communications", "/controls?control=SC-7"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n%s", want, out)
		}
	}

	// A reference the installation does not have must come back as an error
	// the model can act on, not as an empty record it might describe.
	if _, err := handler(context.Background(), json.RawMessage(`{"kind":"nfr","ref":"missing"}`)); err == nil {
		t.Error("grc_get invented a record for an unknown ref")
	}
}

// The index tool is the honest way to answer "how many", and its output has to
// say that the list is complete.
func TestIndexToolSaysItIsComplete(t *testing.T) {
	srv, _ := fakeGRC(t)
	reg := tool.NewRegistry()
	if err := Register(reg, New(Config{BaseURL: srv.URL})); err != nil {
		t.Fatalf("Register: %v", err)
	}

	handler, _ := reg.Handler("grc_list_nfrs")
	out, err := handler(context.Background(), nil)
	if err != nil {
		t.Fatalf("grc_list_nfrs: %v", err)
	}
	for _, want := range []string{"All 2 nfr record(s)", "61 | Network-level controls | Network Security",
		"NIST: SC-7", "a count taken from it is exact"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n%s", want, out)
		}
	}
}
