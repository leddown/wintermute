package llm

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
)

func testRouter(t *testing.T, names ...string) *Router {
	t.Helper()
	backends := make([]*Backend, 0, len(names))
	for _, n := range names {
		backends = append(backends, &Backend{Name: n, Provider: &echoModelProvider{}})
	}
	r, err := NewRouter(backends, names[0], "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return r
}

// Declaring a backend in the UI has to reach the router without a restart,
// which is the whole point of Replace.
func TestReplaceAddsAndRemovesBackends(t *testing.T) {
	r := testRouter(t, "local")

	if _, ok := r.Backend("claude"); ok {
		t.Fatal("claude resolves before it was added")
	}
	if err := r.Replace([]*Backend{
		{Name: "local", Provider: &echoModelProvider{}},
		{Name: "claude", Provider: &echoModelProvider{}},
	}, "local", ""); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, ok := r.Backend("claude"); !ok {
		t.Error("claude does not resolve after being added")
	}
	if got := r.Names(); len(got) != 2 {
		t.Errorf("Names() = %v, want both backends", got)
	}
	if r.Default() != "local" {
		t.Errorf("Default() = %q, want local", r.Default())
	}

	if err := r.Replace([]*Backend{
		{Name: "local", Provider: &echoModelProvider{}},
	}, "local", ""); err != nil {
		t.Fatalf("replace back: %v", err)
	}
	if _, ok := r.Backend("claude"); ok {
		t.Error("claude still resolves after being removed")
	}
}

// A rejected Replace must leave the router serving what it served before:
// adding one bad backend cannot be allowed to take the working ones offline.
func TestReplaceRejectedLeavesRouterIntact(t *testing.T) {
	r := testRouter(t, "local")

	for _, tc := range []struct {
		name     string
		backends []*Backend
		def      string
	}{
		{"no backends", nil, "local"},
		{"unnamed backend", []*Backend{{Provider: &echoModelProvider{}}}, ""},
		{"duplicate name", []*Backend{
			{Name: "dup", Provider: &echoModelProvider{}},
			{Name: "dup", Provider: &echoModelProvider{}},
		}, "dup"},
		{"default not present", []*Backend{
			{Name: "other", Provider: &echoModelProvider{}},
		}, "missing"},
	} {
		if err := r.Replace(tc.backends, tc.def, ""); err == nil {
			t.Errorf("%s: replace succeeded, want an error", tc.name)
		}
		if _, ok := r.Backend("local"); !ok {
			t.Fatalf("%s: the working backend was lost by a rejected replace", tc.name)
		}
	}
}

// Replace runs while turns are in flight, so the accessors must be safe under
// -race against a concurrent swap.
func TestReplaceIsSafeUnderConcurrentReads(t *testing.T) {
	r := testRouter(t, "local")
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			set := []*Backend{{Name: "local", Provider: &echoModelProvider{}}}
			if i%2 == 0 {
				set = append(set, &Backend{Name: "claude", Provider: &echoModelProvider{}})
			}
			if err := r.Replace(set, "local", ""); err != nil {
				t.Errorf("replace: %v", err)
				return
			}
		}
	}()

	for i := 0; i < 500; i++ {
		r.Names()
		r.Default()
		r.Fallback()
		r.Backend("local")
		if _, err := r.Complete(ctx, "local", Request{}); err != nil {
			t.Fatalf("complete during replace: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}
