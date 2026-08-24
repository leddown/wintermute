package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// hubRecorder serves a canned response and remembers what was asked for.
func hubRecorder(t *testing.T, body string) (*Hub, *url.URL) {
	t.Helper()
	var got url.URL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.URL
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewHub(srv.URL, ""), &got
}

// hubStub serves one canned response for any path.
func hubStub(t *testing.T, body string) *Hub {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewHub(srv.URL, "")
}

func quantByLabel(t *testing.T, m *HubModel, label string) HubQuant {
	t.Helper()
	for _, q := range m.Quants {
		if q.Quant == label {
			return q
		}
	}
	t.Fatalf("no %s quantization in %v", label, m.Quants)
	return HubQuant{}
}

// A shard of a split GGUF is not a model. Fetching part one on its own put a
// 28GB file on the drive that looked complete and could not be loaded.
func TestDetailKeepsSplitQuantizationsWhole(t *testing.T) {
	body := `{
	  "id": "unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF",
	  "gguf": {"total": 30532122624, "architecture": "qwen3moe", "context_length": 262144},
	  "siblings": [
	    {"rfilename": ".gitattributes"},
	    {"rfilename": "BF16/Qwen3-Coder-30B-A3B-Instruct-BF16-00001-of-00002.gguf"},
	    {"rfilename": "BF16/Qwen3-Coder-30B-A3B-Instruct-BF16-00002-of-00002.gguf"},
	    {"rfilename": "Qwen3-Coder-30B-A3B-Instruct-IQ4_NL.gguf"}
	  ]
	}`
	m, err := hubStub(t, body).Detail(context.Background(), "unsloth/x", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	bf16 := quantByLabel(t, m, "BF16")
	if len(bf16.Parts) != 2 {
		t.Fatalf("both shards must be listed, got %v", bf16.Parts)
	}
	if bf16.Parts[0] != "BF16/Qwen3-Coder-30B-A3B-Instruct-BF16-00001-of-00002.gguf" ||
		bf16.Parts[1] != "BF16/Qwen3-Coder-30B-A3B-Instruct-BF16-00002-of-00002.gguf" {
		t.Errorf("shards must be in order, got %v", bf16.Parts)
	}
	if bf16.Filename != bf16.Parts[0] {
		t.Errorf("the file to load is the first shard, got %q", bf16.Filename)
	}
	if bf16.Incomplete {
		t.Error("both shards are present, so this set is not incomplete")
	}

	if single := quantByLabel(t, m, "IQ4_NL"); len(single.Parts) != 0 {
		t.Errorf("an ordinary single-file quantization carries no parts, got %v", single.Parts)
	}
}

// An upload still in progress advertises more shards than it lists. Fetching
// what is there produces something unloadable, so it is called out instead.
func TestDetailMarksMissingShardsIncomplete(t *testing.T) {
	body := `{"id": "a/b", "siblings": [
	    {"rfilename": "m-Q8_0-00001-of-00003.gguf"},
	    {"rfilename": "m-Q8_0-00003-of-00003.gguf"}
	]}`
	m, err := hubStub(t, body).Detail(context.Background(), "a/b", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	q := quantByLabel(t, m, "Q8_0")
	if !q.Incomplete {
		t.Error("a set missing shard 2 of 3 must be reported incomplete")
	}
}

// Distinct files can infer to the same label — Q2_K_L reads as Q2_K — and
// deduping on the label alone hid a download that was perfectly available.
func TestDetailKeepsDistinctFilesSharingALabel(t *testing.T) {
	body := `{"id": "a/b", "siblings": [
	    {"rfilename": "m-Q2_K.gguf"},
	    {"rfilename": "m-Q2_K_L.gguf"}
	]}`
	m, err := hubStub(t, body).Detail(context.Background(), "a/b", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Quants) != 2 {
		t.Fatalf("both files are downloadable and both should be listed, got %v", m.Quants)
	}
}

func TestSplitPartsReadsShardNaming(t *testing.T) {
	stem, part, want := splitParts("model-00002-of-00007.gguf")
	if stem != "model" || part != 2 || want != 7 {
		t.Errorf("got %q %d %d", stem, part, want)
	}
	// Not a shard: a plain file, and a nonsensically numbered one, are both
	// ordinary single files rather than a set invented out of the name.
	for _, name := range []string{"model.gguf", "model-00000-of-00000.gguf", "model-00009-of-00002.gguf"} {
		stem, part, want := splitParts(name)
		if stem != name || part != 1 || want != 1 {
			t.Errorf("%s: got %q %d %d", name, stem, part, want)
		}
	}
}

// The detail response is decoded from the Hub's own shape; if that decoding
// silently stopped working every quantization would vanish with no error.
func TestDetailReadsGGUFHeaderFacts(t *testing.T) {
	body := `{"id":"a/b","gguf":{"total":30532122624,"architecture":"qwen3moe","context_length":262144},
	  "cardData":{"license":"apache-2.0"},"siblings":[{"rfilename":"m-Q4_K_M.gguf"}]}`
	m, err := hubStub(t, body).Detail(context.Background(), "a/b", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.ParamsB < 30.5 || m.ParamsB > 30.6 {
		t.Errorf("params should come from the GGUF header, got %v", m.ParamsB)
	}
	if m.Architecture != "qwen3moe" || m.CtxLen != 262144 || m.License != "apache-2.0" {
		b, _ := json.Marshal(m)
		t.Errorf("header facts not carried through: %s", b)
	}
}

// Without expand[] a search returns barely more than an id, and every fact on
// the result card comes back empty — which is exactly how the cards came to be
// bare. The facts are all on the search response; asking for them is the whole
// fix, so the request itself is what this checks.
func TestSearchAsksForTheFactsItDisplays(t *testing.T) {
	hub, got := hubRecorder(t, `[{
	  "id": "unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF",
	  "author": "unsloth",
	  "downloads": 90000, "downloadsAllTime": 15120421, "likes": 340,
	  "gated": false,
	  "lastModified": "2026-01-30T06:29:38.000Z",
	  "gguf": {"total": 30532122624, "architecture": "qwen3moe", "context_length": 262144,
	           "chat_template": "{% if tools %}{{ tools }}{% endif %}"},
	  "cardData": {"license": "apache-2.0"},
	  "baseModels": {"relation": "quantized", "models": [{"id": "Qwen/Qwen3-Coder-30B-A3B-Instruct"}]},
	  "siblings": [{"rfilename": "m-Q4_K_M.gguf"}, {"rfilename": "m-Q8_0.gguf"}]
	}]`)

	page, err := hub.Search(context.Background(), SearchOptions{Query: "qwen"})
	if err != nil {
		t.Fatal(err)
	}
	out := page.Models

	asked := got.Query()["expand[]"]
	for _, want := range []string{"gguf", "author", "gated", "lastModified", "siblings", "baseModels"} {
		if !contains(asked, want) {
			t.Errorf("search must expand %q, asked for %v", want, asked)
		}
	}

	if len(out) != 1 {
		t.Fatalf("want one result, got %d", len(out))
	}
	m := out[0]
	switch {
	case m.Author != "unsloth":
		t.Errorf("author empty: %+v", m)
	case m.ParamsB < 30.5 || m.ParamsB > 30.6:
		t.Errorf("params must come from the GGUF header, not the name: %v", m.ParamsB)
	case m.Architecture != "qwen3moe" || m.CtxLen != 262144:
		t.Errorf("header facts missing: %+v", m)
	case m.License != "apache-2.0":
		t.Errorf("license missing: %+v", m)
	case m.DownloadsAllTime != 15120421:
		t.Errorf("all-time downloads missing: %+v", m)
	case m.BaseModel != "Qwen/Qwen3-Coder-30B-A3B-Instruct":
		t.Errorf("base model missing: %+v", m)
	case m.UpdatedAt.IsZero():
		t.Errorf("last modified missing: %+v", m)
	case m.QuantCount != 2:
		t.Errorf("want 2 quantizations counted, got %d", m.QuantCount)
	}
	if !contains(capabilityStrings(m.Capabilities), "tools") {
		t.Errorf("tool support is read from the chat template: %+v", m.Capabilities)
	}

	// The chat template is several kilobytes of Jinja per result and nothing
	// downstream reads it. It is consumed on the way past, never forwarded.
	blob, _ := json.Marshal(m)
	if strings.Contains(string(blob), "{% if tools %}") {
		t.Error("the chat template must not reach the browser")
	}
	// The per-file list belongs to the detail request, which is the only one
	// that carries sizes; shipping it here doubles the payload for a list most
	// of which is scrolled past.
	if len(m.Quants) != 0 {
		t.Errorf("search should not carry the file list, got %v", m.Quants)
	}
}

// gated is false for an open repository and a string naming the approval flow
// for a closed one. Decoding it as either would fail on the other.
func TestSearchDecodesGatedBothWays(t *testing.T) {
	for body, want := range map[string]string{
		`[{"id":"a/b","gated":false}]`:    "",
		`[{"id":"a/b","gated":"auto"}]`:   "auto",
		`[{"id":"a/b","gated":"manual"}]`: "manual",
	} {
		hub, _ := hubRecorder(t, body)
		page, err := hub.Search(context.Background(), SearchOptions{Query: "x"})
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if page.Models[0].Gated != want {
			t.Errorf("%s: want gated %q, got %q", body, want, page.Models[0].Gated)
		}
	}
}

// The download size is the number that decides whether a fetch is started, and
// a shard set costs what all of its shards cost.
func TestDetailSizesQuantizationsIncludingShards(t *testing.T) {
	hub, got := hubRecorder(t, `{"id":"a/b","siblings":[
	  {"rfilename":"BF16/m-BF16-00001-of-00002.gguf","size":49655154016},
	  {"rfilename":"BF16/m-BF16-00002-of-00002.gguf","size":11440652032},
	  {"rfilename":"m-IQ4_NL.gguf","size":17310784672}
	]}`)
	m, err := hub.Detail(context.Background(), "a/b", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Sizes only come back when blobs are asked for; without this every size
	// is silently zero.
	if got.Query().Get("blobs") != "true" {
		t.Errorf("detail must ask for blobs, requested %s", got.RawQuery)
	}
	if q := quantByLabel(t, m, "BF16"); q.SizeBytes != 49655154016+11440652032 {
		t.Errorf("a shard set costs what all its shards cost, got %d", q.SizeBytes)
	}
	if q := quantByLabel(t, m, "IQ4_NL"); q.SizeBytes != 17310784672 {
		t.Errorf("want the file size, got %d", q.SizeBytes)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func capabilityStrings(caps []Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
}
