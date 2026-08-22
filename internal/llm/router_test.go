package llm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
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

// countingRecorder collects samples so a test can assert on what the router
// measured rather than on whether it returned.
type countingRecorder struct{ samples []Sample }

func (c *countingRecorder) Record(s Sample) { c.samples = append(c.samples, s) }

// slowProvider takes a known amount of time, so the measured duration can be
// checked against something real rather than against zero.
type slowProvider struct {
	delay  time.Duration
	tokens int
	err    error
}

func (p *slowProvider) Name() string { return "slow" }

func (p *slowProvider) Complete(ctx context.Context, _ Request) (*Response, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if p.err != nil {
		return nil, p.err
	}
	return &Response{
		Message: Message{Role: RoleAssistant, Content: "done"},
		Usage:   Usage{CompletionTokens: p.tokens, PromptTokens: 10},
	}, nil
}

// Every call through the router is measured, including the token counts the
// provider reported.
func TestRouterRecordsEverySuccessfulCall(t *testing.T) {
	rec := &countingRecorder{}
	r := newTestRouter(t, &slowProvider{delay: 20 * time.Millisecond, tokens: 40})
	r.WithRecorder(rec)

	if _, err := r.Complete(context.Background(), "", Request{}); err != nil {
		t.Fatal(err)
	}
	if len(rec.samples) != 1 {
		t.Fatalf("recorded %d samples, want 1", len(rec.samples))
	}
	s := rec.samples[0]
	if s.CompletionTokens != 40 || s.PromptTokens != 10 {
		t.Errorf("tokens = %d/%d, want 10/40", s.PromptTokens, s.CompletionTokens)
	}
	if s.Duration < 15*time.Millisecond {
		t.Errorf("duration = %v, want at least the provider's delay", s.Duration)
	}
	if s.Failed {
		t.Error("a successful call was recorded as failed")
	}
	if rate := s.TokensPerSecond(); rate <= 0 {
		t.Errorf("tokens/sec = %v, want a positive rate", rate)
	}
}

// A failure is recorded rather than dropped: a backend that fails in two
// seconds would otherwise look faster than one that succeeds in twenty.
func TestRouterRecordsFailures(t *testing.T) {
	rec := &countingRecorder{}
	r := newTestRouter(t, &slowProvider{delay: time.Millisecond, err: errors.New("boom")})
	r.WithRecorder(rec)

	if _, err := r.Complete(context.Background(), "", Request{}); err == nil {
		t.Fatal("expected the call to fail")
	}
	if len(rec.samples) != 1 {
		t.Fatalf("recorded %d samples, want 1", len(rec.samples))
	}
	if !rec.samples[0].Failed {
		t.Error("a failed call was not marked failed")
	}
}

// Cancelling is the user pressing stop, not the backend being slow. Recording
// it would drag every average down with a measurement of how long somebody
// waited before changing their mind.
func TestRouterDoesNotRecordCancelledCalls(t *testing.T) {
	rec := &countingRecorder{}
	r := newTestRouter(t, &slowProvider{delay: 2 * time.Second, tokens: 10})
	r.WithRecorder(rec)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.Complete(ctx, "", Request{}); err == nil {
		t.Fatal("expected the call to be cancelled")
	}
	if len(rec.samples) != 0 {
		t.Errorf("recorded %d samples for a cancelled call, want 0", len(rec.samples))
	}
}

// A fallback produces two samples — the failure on the intended backend and
// the success on the one that covered for it — each attributed to the backend
// that did the work.
func TestRouterMarksFallbackSamples(t *testing.T) {
	rec := &countingRecorder{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := NewRouter([]*Backend{
		{Name: "local", Provider: &slowProvider{delay: time.Millisecond, err: errors.New("down")}},
		{Name: "cloud", Provider: &slowProvider{delay: time.Millisecond, tokens: 5}},
	}, "local", "cloud", log)
	if err != nil {
		t.Fatal(err)
	}
	r.WithRecorder(rec)

	if _, err := r.Complete(context.Background(), "local", Request{}); err != nil {
		t.Fatal(err)
	}
	if len(rec.samples) != 2 {
		t.Fatalf("recorded %d samples, want 2 (the failure and the fallback)", len(rec.samples))
	}
	if rec.samples[0].Backend != "local" || !rec.samples[0].Failed || rec.samples[0].FellBack {
		t.Errorf("first sample = %+v, want the failure on local", rec.samples[0])
	}
	if rec.samples[1].Backend != "cloud" || !rec.samples[1].FellBack {
		t.Errorf("second sample = %+v, want the fallback on cloud", rec.samples[1])
	}
}

func newTestRouter(t *testing.T, p Provider) *Router {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := NewRouter([]*Backend{{Name: "test", Provider: p}}, "test", "", log)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
