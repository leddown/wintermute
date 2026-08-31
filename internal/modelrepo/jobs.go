package modelrepo

// Download jobs, and the progress the UI polls.
//
// A model download is minutes to hours of work over a link that is not always
// reliable, which makes it the one thing in this server that must outlive the
// request that asked for it. The browser tab that started it may be closed; the
// page will be reloaded; the operator will want to know how far along it is
// from a different machine. So a download runs in the background and its state
// lives here, keyed by an id the caller can come back to.
//
// This registry is in memory on purpose. It records what a *running* transfer
// is doing, and a transfer does not survive a restart — but the bytes already
// fetched do, in the .part file, so a download started again after a restart
// resumes rather than beginning from nothing. Persisting job rows would add a
// second, staler account of a fact the filesystem already keeps.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// JobState is where a download has got to.
type JobState string

const (
	// JobRunning covers connecting, retrying and transferring alike. The
	// distinction the operator cares about is whether it is still trying.
	JobRunning JobState = "running"
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
	// JobCancelled is a download the operator stopped. Its partial file is
	// kept, so starting the same download again continues from where it got to.
	JobCancelled JobState = "cancelled"
)

// Job is one download, as reported to the UI.
type Job struct {
	ID       string   `json:"id"`
	HubID    string   `json:"hub_id"`
	Filename string   `json:"filename"`
	RelPath  string   `json:"rel_path"`
	State    JobState `json:"state"`
	// Phase distinguishes the two long silences inside a running job. Hashing
	// a 12GB file back off a USB disk takes minutes during which no bytes
	// move, and a progress bar frozen at 100% with no explanation is
	// indistinguishable from a hang.
	Phase string `json:"phase,omitempty"`

	// TotalBytes is zero until the server has said how big the file is, which
	// the UI renders as an indeterminate bar rather than as 0%.
	TotalBytes int64 `json:"total_bytes"`
	DoneBytes  int64 `json:"done_bytes"`
	// ResumedBytes is what was already on disk when this job started. It is
	// reported separately so a job that begins at 80% is visibly a resume
	// rather than a suspiciously fast download.
	ResumedBytes int64 `json:"resumed_bytes,omitempty"`
	// Attempt counts retries. A transfer that is quietly retrying looks
	// identical to one that is slow, and the difference matters.
	Attempt int `json:"attempt,omitempty"`

	BytesPerSecond float64 `json:"bytes_per_second,omitempty"`
	// Error is set for a failed job and is the operator's only explanation, so
	// it carries the underlying message rather than a generic one.
	Error string `json:"error,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// Done reports whether the job has stopped, whatever the outcome.
func (j Job) Done() bool { return j.State != JobRunning }

// finishedTTL is how long a completed job stays listed. Long enough that the
// operator sees how a download ended after stepping away, short enough that the
// panel is not a growing history — the repository listing itself is the record
// of what actually arrived.
const finishedTTL = 30 * time.Minute

// Jobs is the registry of downloads.
type Jobs struct {
	mu     sync.Mutex
	jobs   map[string]*Job
	cancel map[string]context.CancelFunc
	seq    int
}

// NewJobs builds an empty registry.
func NewJobs() *Jobs {
	return &Jobs{jobs: map[string]*Job{}, cancel: map[string]context.CancelFunc{}}
}

// ErrAlreadyRunning reports a second download of a file already being fetched.
type ErrAlreadyRunning struct{ RelPath string }

func (e ErrAlreadyRunning) Error() string {
	return fmt.Sprintf("%s is already downloading", e.RelPath)
}

// Start registers a job and returns it along with a context that Cancel stops.
//
// Refusing a duplicate is the point: two transfers writing the same .part file
// would interleave their bytes and produce a file that is the right length and
// entirely wrong, which the digest check would then reject after an hour of
// wasted transfer.
func (r *Jobs) Start(parent context.Context, hubID, filename, relPath string) (*Job, context.Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, j := range r.jobs {
		if j.RelPath == relPath && j.State == JobRunning {
			return nil, nil, ErrAlreadyRunning{RelPath: relPath}
		}
	}
	r.pruneLocked()

	r.seq++
	now := time.Now().UTC()
	job := &Job{
		ID:        fmt.Sprintf("dl-%d-%d", now.Unix(), r.seq),
		HubID:     hubID,
		Filename:  filename,
		RelPath:   relPath,
		State:     JobRunning,
		StartedAt: now,
		UpdatedAt: now,
	}
	r.jobs[job.ID] = job

	// The parent is deliberately not the HTTP request's context: the request
	// returns as soon as the job is registered, and inheriting its context
	// would cancel every download the instant the browser got its reply.
	ctx, cancel := context.WithCancel(parent)
	r.cancel[job.ID] = cancel

	snapshot := *job
	return &snapshot, ctx, nil
}

// Update applies a change to a live job under the lock.
func (r *Jobs) Update(id string, apply func(*Job)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return
	}
	apply(j)
	j.UpdatedAt = time.Now().UTC()
	if elapsed := j.UpdatedAt.Sub(j.StartedAt).Seconds(); elapsed > 0 {
		// Rate over the bytes this job actually moved, not including what a
		// resume inherited — otherwise a job resuming at 90% reports a
		// transfer speed it never achieved.
		if moved := j.DoneBytes - j.ResumedBytes; moved > 0 {
			j.BytesPerSecond = float64(moved) / elapsed
		}
	}
}

// Finish marks a job ended. A nil error means it succeeded.
func (r *Jobs) Finish(id string, state JobState, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return
	}
	j.State = state
	if err != nil {
		j.Error = err.Error()
	}
	now := time.Now().UTC()
	j.UpdatedAt, j.FinishedAt = now, now
	if cancel, ok := r.cancel[id]; ok {
		cancel()
		delete(r.cancel, id)
	}
}

// Cancel stops a running download. The partial file is left for a later resume.
func (r *Jobs) Cancel(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return fmt.Errorf("no such download %q", id)
	}
	if j.State != JobRunning {
		return fmt.Errorf("download %q has already finished", id)
	}
	if cancel, ok := r.cancel[id]; ok {
		cancel()
		delete(r.cancel, id)
	}
	// The state is set by the transfer as it unwinds, which is what keeps the
	// bytes-transferred figure honest.
	return nil
}

// List returns a snapshot, running jobs first and newest first within that.
func (r *Jobs) List() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()

	out := make([]Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].Done() != out[k].Done() {
			return !out[i].Done()
		}
		return out[i].StartedAt.After(out[k].StartedAt)
	})
	return out
}

// Running reports how many transfers are in flight.
func (r *Jobs) Running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, j := range r.jobs {
		if j.State == JobRunning {
			n++
		}
	}
	return n
}

// Forget removes a finished job from the listing.
//
// The TTL clears these on its own, but half an hour is a long time to look at
// a failure you have already read, understood and acted on — and the panel is
// above the thing you are trying to use. Only finished jobs: a running one is
// cancelled, which is a different verb with a different consequence.
func (r *Jobs) Forget(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return fmt.Errorf("no such download %q", id)
	}
	if !j.Done() {
		return fmt.Errorf("download %q is still running — cancel it first", id)
	}
	delete(r.jobs, id)
	delete(r.cancel, id)
	return nil
}

// pruneLocked drops finished jobs past their TTL. Called from the paths that
// already hold the lock rather than on a timer, because a registry nobody is
// looking at does not need tidying.
func (r *Jobs) pruneLocked() {
	cutoff := time.Now().UTC().Add(-finishedTTL)
	for id, j := range r.jobs {
		if j.Done() && j.FinishedAt.Before(cutoff) {
			delete(r.jobs, id)
		}
	}
}
