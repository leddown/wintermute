package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
