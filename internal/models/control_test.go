package models

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeOllama records what it was asked to do, so the tests can assert on the
// request rather than on whether the call returned nil.
type fakeOllama struct {
	calls []map[string]any
	// resident is what /api/ps reports.
	resident []map[string]any
	// status, when set, is returned instead of 200.
	status int
	body   string
}

func (f *fakeOllama) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/generate":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			f.calls = append(f.calls, body)
			if f.status != 0 {
				w.WriteHeader(f.status)
				_, _ = w.Write([]byte(f.body))
				return
			}
			_, _ = w.Write([]byte(`{"done":true}`))
		case "/api/ps":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": f.resident})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ollamaBackend(url string) Backend {
	return Backend{Name: "rig", Kind: KindOllama, BaseURL: url}
}

// Loading by hand means "and keep it there", so it pins by default rather than
// letting the backend evict it on its idle timer a few minutes later.
func TestLoadPinsTheModel(t *testing.T) {
	fake := &fakeOllama{}
	srv := fake.server(t)

	if err := NewController().Load(context.Background(), ollamaBackend(srv.URL), "qwen3:8b", -1); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("made %d calls, want 1", len(fake.calls))
	}
	call := fake.calls[0]
	if call["model"] != "qwen3:8b" {
		t.Errorf("model = %v", call["model"])
	}
	if ka, ok := call["keep_alive"].(float64); !ok || ka != -1 {
		t.Errorf("keep_alive = %v, want -1 (pinned)", call["keep_alive"])
	}
	// No prompt: this must load the weights, not generate anything.
	if _, ok := call["prompt"]; ok {
		t.Error("a prompt was sent; preloading must not generate")
	}
}

// Unloading is the same endpoint with a zero keep-alive, which is what
// releases the weights immediately instead of waiting out the idle timer.
func TestUnloadReleasesImmediately(t *testing.T) {
	fake := &fakeOllama{}
	srv := fake.server(t)

	if err := NewController().Unload(context.Background(), ollamaBackend(srv.URL), "qwen3:8b"); err != nil {
		t.Fatal(err)
	}
	call := fake.calls[0]
	if ka, ok := call["keep_alive"].(float64); !ok || ka != 0 {
		t.Errorf("keep_alive = %v, want 0", call["keep_alive"])
	}
}

// A backend that serves whatever it was started with must say so up front
// rather than having a request fail somewhere inside it.
func TestUnsupportedBackendsRefuseUpFront(t *testing.T) {
	ctrl := NewController()
	for _, kind := range []Kind{KindLlamaCPP, KindAnthropic, KindVLLM} {
		b := Backend{Name: "other", Kind: kind, BaseURL: "http://127.0.0.1:1"}
		if ctrl.Supports(b) {
			t.Errorf("%s reported as controllable", kind)
			continue
		}
		err := ctrl.Load(context.Background(), b, "anything", -1)
		if !errors.Is(err, ErrControlUnsupported) {
			t.Errorf("%s load error = %v, want ErrControlUnsupported", kind, err)
		}
		if err := ctrl.Unload(context.Background(), b, "anything"); !errors.Is(err, ErrControlUnsupported) {
			t.Errorf("%s unload error = %v, want ErrControlUnsupported", kind, err)
		}
	}
	if !ctrl.Supports(Backend{Kind: KindOllama}) || !ctrl.Supports(Backend{Kind: KindHailo}) {
		t.Error("ollama-family backends should be controllable")
	}
}

// A backend that refuses should surface what it said, not just a status code:
// "model not found" is the whole answer, and hiding it sends the operator to
// the server log for one line.
func TestRefusalCarriesTheBackendsExplanation(t *testing.T) {
	fake := &fakeOllama{status: http.StatusNotFound, body: `{"error":"model 'ghost' not found"}`}
	srv := fake.server(t)

	err := NewController().Load(context.Background(), ollamaBackend(srv.URL), "ghost", -1)
	if err == nil {
		t.Fatal("a refused load reported success")
	}
	if got := err.Error(); !strings.Contains(got, "not found") {
		t.Errorf("error = %q, want the backend's own explanation", got)
	}
}

// Residency is read live rather than from the probe cache, because after
// loading a model the operator wants to see it now.
func TestResidentReportsVRAMAndExpiry(t *testing.T) {
	expires := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339)
	fake := &fakeOllama{resident: []map[string]any{
		{"name": "qwen3:8b", "size_vram": int64(6_000_000_000), "expires_at": expires},
	}}
	srv := fake.server(t)

	got, err := NewController().Resident(context.Background(), ollamaBackend(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d resident models, want 1", len(got))
	}
	if got[0].ModelID != "qwen3:8b" || got[0].VRAMBytes != 6_000_000_000 {
		t.Errorf("resident = %+v", got[0])
	}
	if got[0].ExpiresAt.IsZero() {
		t.Error("expiry was not parsed; the UI cannot say when it will be evicted")
	}
	if got[0].Backend != "rig" {
		t.Errorf("backend = %q, want the one asked", got[0].Backend)
	}
}
