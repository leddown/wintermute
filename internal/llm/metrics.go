package llm

import "time"

// Measuring what the models actually do.
//
// Every number here comes from a call the server already makes, so this costs
// nothing but a clock reading. That matters because it is the honest answer to
// "which model is quicker on this hardware" — not a benchmark run once under
// ideal conditions, but what the thing has actually been doing, at the context
// lengths and concurrency it is really used at.
//
// The recording happens in Router.complete rather than in the agent loop, which
// is deliberate: the loop is not the only caller. Batch workers, the fintech
// forecaster and the backend test page all reach models through the same
// function, and metrics gathered one layer up would quietly omit them and make
// a busy backend look idle.

// Sample is one completed model call.
//
// Token counts are the provider's own figures where it reported them, and zero
// where it did not — several OpenAI-compatible servers omit usage entirely.
// Zero therefore means "not reported", and anything deriving a rate from these
// has to exclude such rows rather than treat them as a call that produced no
// output.
type Sample struct {
	Backend string
	Model   string
	// PromptTokens and CompletionTokens are input and output as the provider
	// counted them.
	PromptTokens     int
	CompletionTokens int
	// Duration is the wall time of the whole call: queueing on the backend,
	// prompt processing and generation together.
	//
	// It is not time-to-first-token, which is the more useful figure and cannot
	// be measured without streaming — both providers here request complete
	// responses. Naming this Duration rather than Latency keeps that
	// distinction visible at every use.
	Duration time.Duration
	// Failed marks a call that returned an error. Worth keeping rather than
	// dropping: a backend that fails in two seconds would otherwise look
	// faster than one that succeeds in twenty.
	Failed bool
	// FellBack marks a call that was served by the fallback backend after the
	// intended one failed. The sample is attributed to the backend that
	// actually did the work, so this is the flag that explains why a cloud
	// backend has traffic nobody asked it for.
	FellBack bool
}

// TokensPerSecond is the generation rate this sample implies, or zero when it
// cannot be computed.
//
// Output tokens over wall time, which understates the true generation rate
// because it includes prompt processing and queueing. That is the right way to
// be wrong here: it is the rate the operator actually experiences, and a figure
// that flattered the model by excluding the wait would be measuring something
// nobody is waiting for.
func (s Sample) TokensPerSecond() float64 {
	if s.CompletionTokens <= 0 || s.Duration <= 0 {
		return 0
	}
	return float64(s.CompletionTokens) / s.Duration.Seconds()
}

// Recorder is told about every completed model call.
//
// Implementations must not block: this is called on the path of a live turn,
// and a metrics write that stalls would make the thing it measures slower.
// Losing a sample is recoverable; delaying an answer is not.
type Recorder interface {
	Record(Sample)
}

// WithRecorder attaches a metrics recorder. Optional — without one the router
// behaves exactly as it did before measurement existed.
func (r *Router) WithRecorder(rec Recorder) *Router {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorder = rec
	return r
}

// record hands a sample to the recorder, if there is one.
func (r *Router) record(s Sample) {
	r.mu.RLock()
	rec := r.recorder
	r.mu.RUnlock()
	if rec != nil {
		rec.Record(s)
	}
}
