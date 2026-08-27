package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wintermute/internal/agent"
	"wintermute/internal/llm"
	"wintermute/internal/models"
	"wintermute/internal/node"
	"wintermute/internal/store"
	"wintermute/internal/tool"
)

// The whole chain, end to end: a node reports its hardware, a backend declares
// that it runs there, and a fit question is answered about that machine rather
// than about the box serving the API.
//
// This is the case the split exists for. wintermuted is meant to sit on a small
// always-on host, which is the one machine in the deployment that will never
// load a model — so a verdict computed from it is not merely unhelpful, it is
// confidently about the wrong hardware.
func TestFitIsGradedAgainstTheDeclaredNode(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router, err := llm.NewRouter([]*llm.Backend{{Name: "local", Provider: stubProvider{}}}, "local", "", log)
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	ag := agent.New(router, st, tools, log, 4)

	// The server's own backend is not on loopback and names no node, exactly as
	// a fleet deployment's would not be.
	cat := models.NewCatalog([]models.Backend{
		{Name: "big", Kind: models.KindLlamaCPP, BaseURL: "http://192.168.1.40:8080/v1", Node: "tycho"},
	}, st, models.NewHub("", ""), log)

	nodeStore, err := node.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nodeStore.Close() })
	cat.SetFleet(nodeStore)

	srv := New(ag, st, tools, cat, Workspace{}, ServerInfo{}, log).WithNodes(nodeStore, 2*time.Hour)
	handler := srv.Handler()

	_, nodeToken, err := st.CreateClient(t.Context(), "tycho", store.KindNode)
	if err != nil {
		t.Fatal(err)
	}
	_, browserToken, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}

	// The node reports a 3090 with most of its memory free.
	report := fmt.Sprintf(`{"format_version":1,
		"facts":{"hostname":"tycho.lan","cores":12,
		  "gpus":[{"index":0,"name":"NVIDIA GeForce RTX 3090","mem_total_bytes":%d}]},
		"samples":[{"at":%q,"cpu_percent":4,
		  "mem_total_bytes":%d,"mem_used_bytes":%d,
		  "gpu_mem_used_bytes":%d,"gpu_mem_total_bytes":%d}]}`,
		24*1<<30, time.Now().UTC().Format(time.RFC3339Nano),
		64*1<<30, 8*1<<30, 2*1<<30, 24*1<<30)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/report", strings.NewReader(report))
	req.Header.Set("Authorization", "Bearer "+nodeToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("report gave %d: %s", rec.Code, rec.Body.String())
	}

	// An 8B model at Q4_K_M is nothing to a 3090 and impossible on the API
	// server, which has no GPU at all and is not running a backend anyway.
	ask := func(paramsB float64) fitResponse {
		t.Helper()
		body := fmt.Sprintf(`{"params_b":%g,"quant":"Q4_K_M","context_tokens":4096}`, paramsB)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/models/fit", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+browserToken)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("fit gave %d: %s", rec.Code, rec.Body.String())
		}
		var out fitResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode fit: %v", err)
		}
		return out
	}

	got := ask(8)
	if got.Verdict != models.VerdictFits {
		t.Errorf("verdict = %q, want %q — graded against the node's 3090", got.Verdict, models.VerdictFits)
	}
	if got.Host != "tycho" {
		t.Errorf("host = %q, want %q: a verdict without the machine it is about "+
			"is not an answer on a fleet", got.Host, "tycho")
	}
	if got.TokensPerSec <= 0 {
		t.Error("no throughput estimate, though the card's bandwidth is known from its name")
	}
	if len(got.Hosts) != 1 {
		t.Errorf("graded %d machines, want 1: the API server serves no models and "+
			"must not be a candidate", len(got.Hosts))
	}

	// And the negative direction still works: something far too large for the
	// node is refused on the node's terms, not on this server's.
	huge := ask(400)
	if huge.Verdict == models.VerdictFits {
		t.Errorf("a 400B model reported as fitting on a 24GB card: %+v", huge.Verdict)
	}
	if huge.Host != "tycho" {
		t.Errorf("host = %q on the negative verdict too, want %q", huge.Host, "tycho")
	}
}

// The declaration is what makes a node a candidate. Without it the node is
// still reporting, still visible on the Fleet screen, and still not something
// this server will compute a verdict from — an address is not a machine.
func TestUndeclaredNodeIsNotGraded(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router, err := llm.NewRouter([]*llm.Backend{{Name: "local", Provider: stubProvider{}}}, "local", "", log)
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	ag := agent.New(router, st, tools, log, 4)

	// Addressed by the node's own name, and declaring nothing. The temptation
	// to infer the link from this string is exactly what is being refused.
	cat := models.NewCatalog([]models.Backend{
		{Name: "big", Kind: models.KindLlamaCPP, BaseURL: "http://tycho.lan:8080/v1"},
	}, st, models.NewHub("", ""), log)

	nodeStore, err := node.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nodeStore.Close() })
	cat.SetFleet(nodeStore)

	srv := New(ag, st, tools, cat, Workspace{}, ServerInfo{}, log).WithNodes(nodeStore, 2*time.Hour)
	handler := srv.Handler()

	_, nodeToken, err := st.CreateClient(t.Context(), "tycho", store.KindNode)
	if err != nil {
		t.Fatal(err)
	}
	_, browserToken, err := st.CreateClient(t.Context(), "laptop", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}

	report := fmt.Sprintf(`{"format_version":1,
		"facts":{"hostname":"tycho.lan","gpus":[{"index":0,"name":"NVIDIA GeForce RTX 3090","mem_total_bytes":%d}]},
		"samples":[{"at":%q,"gpu_mem_total_bytes":%d}]}`,
		24*1<<30, time.Now().UTC().Format(time.RFC3339Nano), 24*1<<30)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/report", strings.NewReader(report))
	req.Header.Set("Authorization", "Bearer "+nodeToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("report gave %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/models/fit",
		strings.NewReader(`{"params_b":8,"quant":"Q4_K_M","context_tokens":4096}`))
	req.Header.Set("Authorization", "Bearer "+browserToken)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var got fitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode fit: %v", err)
	}
	if got.Verdict != models.VerdictUnknown {
		t.Errorf("verdict = %q, want %q: nothing was declared as a machine that "+
			"runs models, so nothing may be graded", got.Verdict, models.VerdictUnknown)
	}
	// The footprint survives, as it does everywhere a verdict cannot be given.
	if got.TotalMB <= 0 {
		t.Error("the memory footprint was discarded along with the verdict")
	}
}
