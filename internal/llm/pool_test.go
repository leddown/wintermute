package llm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPool(t *testing.T, members ...PoolMember) *Pool {
	t.Helper()
	p, err := NewPool("test", members, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func member(name string, slots int) PoolMember {
	return PoolMember{Backend: &Backend{Name: name, Model: name + "-model"}, Slots: slots}
}

// okJobs builds n jobs that record which backend served them.
func okJobs(n int, served *sync.Map) []Job {
	jobs := make([]Job, n)
	for i := range jobs {
		i := i
		jobs[i] = func(_ context.Context, b *Backend) error {
			served.Store(i, b.Name)
			return nil
		}
	}
	return jobs
}

func TestPoolRunsEveryJobOnce(t *testing.T) {
	var served sync.Map
	var runs int64
	jobs := make([]Job, 25)
	for i := range jobs {
		i := i
		jobs[i] = func(_ context.Context, b *Backend) error {
			atomic.AddInt64(&runs, 1)
			served.Store(i, b.Name)
			return nil
		}
	}

	p := testPool(t, member("a", 2), member("b", 2))
	results := p.Run(context.Background(), jobs, RunOptions{})

	if len(results) != len(jobs) {
		t.Fatalf("got %d results, want %d", len(results), len(jobs))
	}
	if got := atomic.LoadInt64(&runs); got != int64(len(jobs)) {
		t.Errorf("jobs ran %d times, want %d — a job must not be run twice when it succeeds", got, len(jobs))
	}
	for i, res := range results {
		if res.Err != nil {
			t.Errorf("job %d: %v", i, res.Err)
		}
		if res.Index != i {
			t.Errorf("result %d has index %d — results must stay in job order", i, res.Index)
		}
		if _, ok := served.Load(i); !ok {
			t.Errorf("job %d never ran", i)
		}
	}
}

// The point of the pool is that work actually overlaps. Every slot should be
// busy at once, not one at a time.
func TestPoolRunsJobsConcurrently(t *testing.T) {
	const slots = 4
	p := testPool(t, member("a", 2), member("b", 2))
	if p.Slots() != slots {
		t.Fatalf("pool has %d slots, want %d", p.Slots(), slots)
	}

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	jobs := make([]Job, 16)
	for i := range jobs {
		jobs[i] = func(context.Context, *Backend) error {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			time.Sleep(20 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		}
	}

	p.Run(context.Background(), jobs, RunOptions{})

	mu.Lock()
	defer mu.Unlock()
	if peak < 2 {
		t.Errorf("peak concurrency was %d — jobs are not overlapping at all", peak)
	}
	if peak > slots {
		t.Errorf("peak concurrency was %d, above the %d declared slots", peak, slots)
	}
}

// One backend dying must cost you its in-flight items, retried elsewhere, not
// the whole batch.
func TestPoolRetriesFailedJobOnAnotherMember(t *testing.T) {
	var tried sync.Map // backend name -> attempts
	jobs := []Job{
		func(_ context.Context, b *Backend) error {
			n, _ := tried.LoadOrStore(b.Name, 0)
			tried.Store(b.Name, n.(int)+1)
			if b.Name == "broken" {
				return errors.New("connection refused")
			}
			return nil
		},
	}

	// One slot each, so the retry has somewhere else to land.
	p := testPool(t, member("broken", 1), member("working", 1))
	results := p.Run(context.Background(), jobs, RunOptions{MaxAttempts: 3})

	if results[0].Err != nil {
		t.Fatalf("job failed despite a healthy member being available: %v", results[0].Err)
	}
	if results[0].Backend == "broken" {
		t.Errorf("result credits the broken backend")
	}
	if results[0].Attempts < 1 {
		t.Errorf("attempts = %d", results[0].Attempts)
	}
}

// A job that fails everywhere is reported as failed, with its error intact,
// and does not stop the rest of the batch.
func TestPoolReportsExhaustedJobWithoutFailingTheBatch(t *testing.T) {
	boom := errors.New("boom")
	jobs := []Job{
		func(context.Context, *Backend) error { return nil },
		func(context.Context, *Backend) error { return boom },
		func(context.Context, *Backend) error { return nil },
	}

	p := testPool(t, member("a", 1), member("b", 1))
	results := p.Run(context.Background(), jobs, RunOptions{MaxAttempts: 2, RetireAfter: 99})

	if results[0].Err != nil || results[2].Err != nil {
		t.Errorf("healthy jobs were affected by a failing sibling: %v, %v", results[0].Err, results[2].Err)
	}
	if !errors.Is(results[1].Err, boom) {
		t.Errorf("failed job error = %v, want %v", results[1].Err, boom)
	}
	if results[1].Attempts != 2 {
		t.Errorf("failed job ran %d times, want MaxAttempts=2", results[1].Attempts)
	}
}

// A dead backend must stop taking work, rather than grabbing item after item
// and making every one of them pay a retry.
func TestPoolRetiresConsistentlyFailingMember(t *testing.T) {
	var brokenCalls int64
	jobs := make([]Job, 30)
	for i := range jobs {
		jobs[i] = func(_ context.Context, b *Backend) error {
			if b.Name == "broken" {
				atomic.AddInt64(&brokenCalls, 1)
				return errors.New("connection refused")
			}
			return nil
		}
	}

	p := testPool(t, member("broken", 1), member("working", 1))
	results := p.Run(context.Background(), jobs, RunOptions{MaxAttempts: 3, RetireAfter: 3})

	for i, res := range results {
		if res.Err != nil {
			t.Errorf("job %d failed although a healthy member existed: %v", i, res.Err)
		}
	}
	// Three failures retire it; allow a little slack for jobs already in
	// flight on its slot when it went.
	if got := atomic.LoadInt64(&brokenCalls); got > 6 {
		t.Errorf("broken backend was tried %d times — it should have been retired after 3", got)
	}
}

// When there is nowhere left to run, the batch still returns: every remaining
// item is reported as abandoned rather than hanging the turn.
func TestPoolReturnsWhenEveryMemberIsRetired(t *testing.T) {
	jobs := make([]Job, 20)
	for i := range jobs {
		jobs[i] = func(context.Context, *Backend) error { return errors.New("down") }
	}

	p := testPool(t, member("a", 1), member("b", 1))

	done := make(chan []JobResult, 1)
	go func() {
		done <- p.Run(context.Background(), jobs, RunOptions{MaxAttempts: 2, RetireAfter: 2})
	}()

	select {
	case results := <-done:
		if len(results) != len(jobs) {
			t.Fatalf("got %d results, want %d", len(results), len(jobs))
		}
		var abandoned int
		for _, res := range results {
			if res.Err == nil {
				t.Errorf("job %d succeeded against a pool where everything is down", res.Index)
			}
			if errors.Is(res.Err, ErrNoHealthyMember) {
				abandoned++
			}
		}
		if abandoned == 0 {
			t.Error("no item reported ErrNoHealthyMember; the abandoned path never ran")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return once every member was retired — the batch hangs")
	}
}

// A cancelled context is the user stopping the batch. It must not be retried,
// and it must not be mistaken for a backend failure.
func TestPoolStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs int64
	jobs := make([]Job, 40)
	for i := range jobs {
		jobs[i] = func(ctx context.Context, _ *Backend) error {
			if atomic.AddInt64(&runs, 1) == 3 {
				cancel()
			}
			time.Sleep(5 * time.Millisecond)
			return ctx.Err()
		}
	}

	p := testPool(t, member("a", 1))
	results := p.Run(ctx, jobs, RunOptions{MaxAttempts: 3})

	if len(results) != len(jobs) {
		t.Fatalf("got %d results, want %d", len(results), len(jobs))
	}
	// With no retries on cancellation, the run cannot exceed one attempt per job.
	if got := atomic.LoadInt64(&runs); got > int64(len(jobs)) {
		t.Errorf("jobs ran %d times for %d jobs — cancelled work was retried", got, len(jobs))
	}
}

func TestPoolRunWithNoJobs(t *testing.T) {
	p := testPool(t, member("a", 1))
	if results := p.Run(context.Background(), nil, RunOptions{}); len(results) != 0 {
		t.Errorf("got %d results for an empty batch", len(results))
	}
}

func TestPoolProgressCallbackSeesEveryItem(t *testing.T) {
	var served sync.Map
	jobs := okJobs(12, &served)

	var (
		mu    sync.Mutex
		seen  []int
		total int
	)
	p := testPool(t, member("a", 2))
	p.Run(context.Background(), jobs, RunOptions{
		OnResult: func(done, n int, _ JobResult) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, done)
			total = n
		},
	})

	if len(seen) != len(jobs) {
		t.Errorf("progress fired %d times, want %d", len(seen), len(jobs))
	}
	if total != len(jobs) {
		t.Errorf("progress reported total %d, want %d", total, len(jobs))
	}
	// done must count up to the total exactly once each.
	counts := map[int]int{}
	for _, d := range seen {
		counts[d]++
	}
	for i := 1; i <= len(jobs); i++ {
		if counts[i] != 1 {
			t.Errorf("progress reported done=%d %d times, want once", i, counts[i])
		}
	}
}

func TestNewPoolRejectsBadMembers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := NewPool("p", nil, log); err == nil {
		t.Error("an empty pool was accepted; a batch would silently run nowhere")
	}
	if _, err := NewPool("p", []PoolMember{{Backend: &Backend{Name: "a"}}, {Backend: &Backend{Name: "a"}}}, log); err == nil {
		t.Error("a duplicate member was accepted")
	}
	if _, err := NewPool("p", []PoolMember{{Backend: nil}}, log); err == nil {
		t.Error("a member with no backend was accepted")
	}
}

func TestPoolSlotsAndCloudReporting(t *testing.T) {
	p := testPool(t, member("local", 2), PoolMember{Backend: &Backend{Name: "claude", Cloud: true}, Slots: 1})

	if got := p.Slots(); got != 3 {
		t.Errorf("Slots() = %d, want 3", got)
	}
	if !p.HasCloudMember() {
		t.Error("HasCloudMember() = false, but a cloud backend is a member")
	}
	if got := p.Members(); len(got) != 2 || got[0] != "local" {
		t.Errorf("Members() = %v", got)
	}

	// A slot count below one is a configuration slip, not a request for zero
	// workers — it must not silently produce a pool that serves nothing.
	p2 := testPool(t, PoolMember{Backend: &Backend{Name: "a"}, Slots: 0})
	if p2.Slots() != 1 {
		t.Errorf("Slots() = %d for a member declaring 0, want 1", p2.Slots())
	}
}

// A pool job is handed the member's default model, so a batch spread over
// backends serving different models still names the right one on each.
func TestPoolJobGetsMemberDefaultModel(t *testing.T) {
	var got sync.Map
	jobs := []Job{
		func(ctx context.Context, b *Backend) error {
			resp, err := b.Complete(ctx, Request{})
			if err != nil {
				return err
			}
			got.Store(b.Name, resp.Message.Content)
			return nil
		},
	}

	b := &Backend{Name: "a", Model: "qwen3-8b", Provider: &echoModelProvider{}}
	p := testPool(t, PoolMember{Backend: b, Slots: 1})
	if res := p.Run(context.Background(), jobs, RunOptions{}); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}

	if v, _ := got.Load("a"); v != "qwen3-8b" {
		t.Errorf("request model = %v, want the member's default qwen3-8b", v)
	}
}

// echoModelProvider replies with whatever model the request named.
type echoModelProvider struct{}

func (echoModelProvider) Name() string { return "echo" }

func (echoModelProvider) Complete(_ context.Context, req Request) (*Response, error) {
	return &Response{Message: Message{Role: RoleAssistant, Content: req.Model}}, nil
}
