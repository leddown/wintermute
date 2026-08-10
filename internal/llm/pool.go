package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Pool is a set of backends that can each serve the same kind of short,
// independent request, used to run a batch of them at once.
//
// This is deliberately not the Router. A conversation is pinned to one backend
// because moving it would throw away the served prompt cache and reprocess the
// whole transcript — for a growing agent transcript that is usually the
// dominant cost, so spreading one conversation is a pessimisation. A batch is
// the opposite shape: many short prompts that share no prefix, so there is no
// cache to lose and the only limit is how many machines can be kept busy.
//
// Members are given slots rather than a global concurrency number. One slot per
// backend is the honest default for a single-GPU llama-server, where a second
// concurrent request divides the throughput it already had instead of adding
// to it; raise it for a backend that genuinely batches.
type Pool struct {
	name    string
	members []*poolMember
	log     *slog.Logger
}

// PoolMember declares one backend's participation in a pool.
type PoolMember struct {
	Backend *Backend
	// Slots is how many items this backend serves at once. Values below one
	// are treated as one.
	Slots int
}

// poolMember carries the per-run health of a member alongside its declaration.
type poolMember struct {
	backend *Backend
	slots   int

	mu          sync.Mutex
	consecutive int
	retired     bool
}

// NewPool builds a pool from its members. It fails rather than silently
// serving a batch from nothing.
func NewPool(name string, members []PoolMember, log *slog.Logger) (*Pool, error) {
	if len(members) == 0 {
		return nil, errors.New("pool: no members")
	}
	p := &Pool{name: name, log: log}
	seen := map[string]bool{}
	for _, m := range members {
		if m.Backend == nil || m.Backend.Name == "" {
			return nil, errors.New("pool: member has no backend")
		}
		if seen[m.Backend.Name] {
			return nil, fmt.Errorf("pool: duplicate member %q", m.Backend.Name)
		}
		seen[m.Backend.Name] = true
		if m.Slots < 1 {
			m.Slots = 1
		}
		p.members = append(p.members, &poolMember{backend: m.Backend, slots: m.Slots})
	}
	return p, nil
}

// Name reports the pool's name, for logs and reports.
func (p *Pool) Name() string { return p.name }

// Members lists the backend names in the pool, in declaration order.
func (p *Pool) Members() []string {
	out := make([]string, 0, len(p.members))
	for _, m := range p.members {
		out = append(out, m.backend.Name)
	}
	return out
}

// Slots reports the total number of items the pool will have in flight.
func (p *Pool) Slots() int {
	total := 0
	for _, m := range p.members {
		total += m.slots
	}
	return total
}

// HasCloudMember reports whether any member sends work off the local network.
// Callers surface this, because a batch is the one place where a lot of data
// can leave at once without anyone choosing it item by item.
func (p *Pool) HasCloudMember() bool {
	for _, m := range p.members {
		if m.backend.Cloud {
			return true
		}
	}
	return false
}

// Job is one unit of independent work, run against whichever member is free.
//
// A Job may be called again on a different backend after a failure, so it must
// keep no state that a second attempt would corrupt. It is called concurrently
// with other jobs.
type Job func(ctx context.Context, b *Backend) error

// JobResult is the outcome of one job.
type JobResult struct {
	// Index is the job's position in the slice handed to Run.
	Index int
	// Backend names what served it — the last one tried, when Err is set.
	Backend string
	// Cloud records whether that backend was off-network.
	Cloud bool
	// Attempts counts how many times the job ran, including the one that
	// succeeded.
	Attempts int
	Err      error
}

// RunOptions tunes one batch.
type RunOptions struct {
	// MaxAttempts bounds how many backends one job may be tried on before it
	// is reported as failed. Defaults to 2, which is enough to survive a
	// backend dying mid-batch without turning a bad prompt into a stampede.
	MaxAttempts int
	// RetireAfter is how many consecutive failures take a member out of the
	// rest of this run. Defaults to 3. Without this, a dead backend keeps
	// accepting items and every one of them pays a retry.
	RetireAfter int
	// OnResult is called once per finished job, from the worker goroutine, so
	// a long batch can report progress. It must not block for long.
	OnResult func(done, total int, res JobResult)
}

func (o RunOptions) withDefaults() RunOptions {
	if o.MaxAttempts < 1 {
		o.MaxAttempts = 2
	}
	if o.RetireAfter < 1 {
		o.RetireAfter = 3
	}
	return o
}

// ErrNoHealthyMember is reported for items left over when every member of the
// pool has been retired.
var ErrNoHealthyMember = errors.New("no healthy pool member remains")

// workItem is one job's journey through the queue.
type workItem struct {
	index    int
	attempts int
	lastErr  error
}

// Run executes every job across the pool and returns one result per job, in
// the order the jobs were given.
//
// It returns when every job has finished, succeeded or failed; a failing job
// never aborts the batch, because the point of fanning out is that one bad
// item or one dead machine costs you that item rather than the whole run.
func (p *Pool) Run(ctx context.Context, jobs []Job, opts RunOptions) []JobResult {
	total := len(jobs)
	results := make([]JobResult, total)
	if total == 0 {
		return results
	}
	opts = opts.withDefaults()

	// The queue is buffered to the full job count so a worker requeueing a
	// failed item can never block: an item is either held by a worker, sitting
	// in the queue, or finished, so at most `total` are ever in flight.
	queue := make(chan *workItem, total)
	for i := range jobs {
		queue <- &workItem{index: i}
	}

	var (
		mu          sync.Mutex
		completed   int
		outstanding = int64(total)
		live        int64
	)

	// finish records a terminal outcome. The last one closes the queue, which
	// is what releases workers still waiting on it.
	finish := func(res JobResult) {
		mu.Lock()
		results[res.Index] = res
		completed++
		done := completed
		mu.Unlock()

		if opts.OnResult != nil {
			opts.OnResult(done, total, res)
		}
		if atomic.AddInt64(&outstanding, -1) == 0 {
			close(queue)
		}
	}

	var wg sync.WaitGroup
	for _, m := range p.members {
		for slot := 0; slot < m.slots; slot++ {
			wg.Add(1)
			atomic.AddInt64(&live, 1)
			go func(m *poolMember) {
				defer wg.Done()
				p.work(ctx, m, jobs, queue, finish, &live, opts)
			}(m)
		}
	}
	wg.Wait()

	return results
}

// work is one slot: pull an item, run it on this member, retry or report.
func (p *Pool) work(
	ctx context.Context,
	m *poolMember,
	jobs []Job,
	queue chan *workItem,
	finish func(JobResult),
	live *int64,
	opts RunOptions,
) {
	// The last slot to leave owns whatever is still queued. Without this, a
	// batch whose members have all been retired would block forever on items
	// nobody is left to serve.
	defer func() {
		if atomic.AddInt64(live, -1) != 0 {
			return
		}
		for {
			select {
			case item, ok := <-queue:
				if !ok {
					return
				}
				finish(JobResult{
					Index:    item.index,
					Attempts: item.attempts,
					Err:      abandoned(item.lastErr),
				})
			default:
				return
			}
		}
	}()

	for item := range queue {
		// Retirement is checked here rather than at the point of failure so
		// that every slot of a retired member steps aside, not just the one
		// that saw the error.
		if m.isRetired() {
			queue <- item
			return
		}
		if err := ctx.Err(); err != nil {
			finish(JobResult{Index: item.index, Backend: m.backend.Name, Attempts: item.attempts, Err: err})
			continue
		}

		item.attempts++
		err := jobs[item.index](ctx, m.backend)

		if err == nil {
			m.succeeded()
			finish(JobResult{
				Index:    item.index,
				Backend:  m.backend.Name,
				Cloud:    m.backend.Cloud,
				Attempts: item.attempts,
			})
			continue
		}

		// A cancelled context is the caller stopping the batch, not the
		// backend failing. Neither retry it nor hold it against the member.
		if ctx.Err() != nil {
			finish(JobResult{Index: item.index, Backend: m.backend.Name, Attempts: item.attempts, Err: err})
			continue
		}

		retired := m.failed(opts.RetireAfter)
		item.lastErr = err

		if item.attempts < opts.MaxAttempts {
			queue <- item
		} else {
			finish(JobResult{
				Index:    item.index,
				Backend:  m.backend.Name,
				Cloud:    m.backend.Cloud,
				Attempts: item.attempts,
				Err:      err,
			})
		}

		if retired {
			p.log.Warn("pool member retired for this batch",
				"pool", p.name, "backend", m.backend.Name,
				"consecutive_failures", opts.RetireAfter, "error", err)
			return
		}
	}
}

// abandoned explains an item nobody was left to run, keeping the cause that
// took the last member down.
func abandoned(last error) error {
	if last == nil {
		return ErrNoHealthyMember
	}
	return fmt.Errorf("%w (last failure: %v)", ErrNoHealthyMember, last)
}

func (m *poolMember) isRetired() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.retired
}

func (m *poolMember) succeeded() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consecutive = 0
}

// failed records a failure and reports whether it retired the member.
func (m *poolMember) failed(retireAfter int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consecutive++
	if m.consecutive >= retireAfter && !m.retired {
		m.retired = true
		return true
	}
	return false
}
