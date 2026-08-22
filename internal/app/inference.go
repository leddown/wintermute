package app

import (
	"context"
	"log/slog"
	"time"

	"wintermute/internal/llm"
	"wintermute/internal/store"
)

// inferenceRecorder buffers model-call samples and writes them in batches.
//
// The contract from llm.Recorder is that Record must not block: it is called on
// the path of a live turn, and a metrics write that stalled would slow the
// thing it is measuring. So Record does nothing but a non-blocking send, and a
// full buffer drops the sample rather than waiting.
//
// Dropping is the right failure. A missing measurement makes an average very
// slightly less accurate; a turn waiting on a database write to record how fast
// it was is absurd. The same reasoning governs the memory indexer: losing the
// derived thing is recoverable, delaying the real one is not.
type inferenceRecorder struct {
	store *store.Store
	log   *slog.Logger
	ch    chan store.InferenceSample
	// dropped counts what the buffer could not take, reported once on
	// shutdown rather than logged per sample — a full buffer would otherwise
	// produce a flood of warnings about a flood.
	dropped int
}

// inferenceBuffer is how many samples may be in flight before dropping starts.
//
// Generous enough that a burst — a batch fanning out across a pool — is
// absorbed whole, small enough to be irrelevant to memory.
const inferenceBuffer = 512

// inferenceFlush is how long a partial batch waits before being written, so a
// quiet server still records the handful of calls it made.
const inferenceFlush = 5 * time.Second

func newInferenceRecorder(st *store.Store, log *slog.Logger) *inferenceRecorder {
	return &inferenceRecorder{
		store: st,
		log:   log,
		ch:    make(chan store.InferenceSample, inferenceBuffer),
	}
}

// Record implements llm.Recorder. Non-blocking by construction.
func (r *inferenceRecorder) Record(s llm.Sample) {
	sample := store.InferenceSample{
		Backend:          s.Backend,
		Model:            s.Model,
		PromptTokens:     s.PromptTokens,
		CompletionTokens: s.CompletionTokens,
		DurationMS:       s.Duration.Milliseconds(),
		Failed:           s.Failed,
		FellBack:         s.FellBack,
		CreatedAt:        time.Now().UTC(),
	}
	select {
	case r.ch <- sample:
	default:
		r.dropped++
	}
}

// Run drains the buffer until ctx is cancelled, writing in batches.
//
// Batching matters: samples arrive one per model call, and a transaction each
// would make the measurements more expensive to store than they were to gather.
func (r *inferenceRecorder) Run(ctx context.Context) {
	ticker := time.NewTicker(inferenceFlush)
	defer ticker.Stop()

	batch := make([]store.InferenceSample, 0, 64)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// A short timeout of its own rather than ctx: on shutdown ctx is
		// already cancelled, and the last batch is still worth writing.
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := r.store.RecordInference(writeCtx, batch)
		cancel()
		if err != nil {
			r.log.Warn("could not record inference samples", "count", len(batch), "error", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// Drain whatever is already buffered before going, so the last
			// turns before a restart are not lost.
			for {
				select {
				case s := <-r.ch:
					batch = append(batch, s)
					continue
				default:
				}
				break
			}
			flush()
			if r.dropped > 0 {
				r.log.Info("inference samples dropped by a full buffer", "count", r.dropped)
			}
			return

		case s := <-r.ch:
			batch = append(batch, s)
			if len(batch) >= cap(batch) {
				flush()
			}

		case <-ticker.C:
			flush()
		}
	}
}
