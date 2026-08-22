package agent

import (
	"context"
	"sync"
	"time"

	"wintermute/internal/llm"
	"wintermute/internal/store"
)

// Off-the-record conversations.
//
// The loop keeps no transcript of its own: it re-reads the conversation from
// SQLite on every iteration, so persistence *is* the working memory. That is
// why an ephemeral conversation needs more than a flag. A row written with
// `excluded = 1` would still be a row — one that every future read query has
// to remember to filter, where a single missed WHERE clause puts the content
// back. So the ephemeral path does not write rows at all, and the loop reads
// its history from here instead.
//
// What this costs, stated plainly: an off-the-record conversation lives in this
// process's memory and nowhere else. It does not survive a restart. That is the
// price of the guarantee, and it is the right way round — the alternative is a
// conversation the operator was told was private surviving on disk.

// Transcript is where a turn's history is read from and written to. The store
// satisfies it directly; ephemeral sessions use the in-memory implementation
// below.
type Transcript interface {
	Append(ctx context.Context, sessionID string, msgs ...llm.Message) error
	Messages(ctx context.Context, sessionID string) ([]llm.Message, error)
}

// Indexer is told which messages have been durably written, so they can be
// embedded for retrieval afterwards.
//
// It is an interface so the loop does not import the recall package, and it is
// optional: a deployment with no embedder configured runs the loop exactly as
// it did before memory existed.
//
// Enqueue returns nothing. Indexing is downstream of the conversation and must
// never be able to fail it — the message is already committed, and an
// embedding that did not happen can be recomputed from it later by the
// backfill command.
type Indexer interface {
	Enqueue(ctx context.Context, messageIDs []int64)
}

// WithIndexer attaches the retrieval indexer.
func (a *Agent) WithIndexer(ix Indexer) *Agent {
	a.indexer = ix
	return a
}

// storeTranscript is the ordinary path: straight through to SQLite.
type storeTranscript struct {
	store   *store.Store
	indexer Indexer
}

func (t storeTranscript) Append(ctx context.Context, sessionID string, msgs ...llm.Message) error {
	ids, err := t.store.AppendMessages(ctx, sessionID, msgs...)
	if err != nil {
		return err
	}
	if t.indexer != nil {
		t.indexer.Enqueue(ctx, ids)
	}
	return nil
}

func (t storeTranscript) Messages(ctx context.Context, sessionID string) ([]llm.Message, error) {
	return t.store.Messages(ctx, sessionID)
}

// Ephemeral holds the transcripts of conversations that are not being written
// to the store.
//
// It is bounded in both directions. Conversations are dropped after they go
// quiet for ttl, and the least recently used is dropped when there are more
// than maxSessions — because this is unbounded growth in a long-running
// process otherwise, and a memory layer that eventually takes the server down
// is not a memory layer.
type Ephemeral struct {
	mu          sync.Mutex
	sessions    map[string]*ephemeralSession
	maxSessions int
	ttl         time.Duration
	now         func() time.Time
}

type ephemeralSession struct {
	messages []llm.Message
	lastUsed time.Time
}

// NewEphemeral builds the in-memory transcript pool.
func NewEphemeral(maxSessions int, ttl time.Duration) *Ephemeral {
	if maxSessions <= 0 {
		maxSessions = 64
	}
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Ephemeral{
		sessions:    map[string]*ephemeralSession{},
		maxSessions: maxSessions,
		ttl:         ttl,
		now:         time.Now,
	}
}

// Append adds messages to an in-memory transcript, creating it if needed.
func (e *Ephemeral) Append(_ context.Context, sessionID string, msgs ...llm.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	s, ok := e.sessions[sessionID]
	if !ok {
		s = &ephemeralSession{}
		e.sessions[sessionID] = s
	}
	// Messages are copied in whole, thinking blocks included. The Messages API
	// rejects a tool-use turn whose assistant message lost the thinking that
	// produced the call, and an ephemeral conversation has to replay exactly
	// as a recorded one does.
	s.messages = append(s.messages, msgs...)
	s.lastUsed = e.now()
	// Evicted after the append rather than before it, so the pool is trimmed
	// against its true size. Evicting first lets it settle one over the limit
	// forever, since the new session is not there to be counted yet.
	e.evictLocked()
	return nil
}

// Messages returns a copy of the in-memory transcript. A copy, because the
// caller hands it to the provider while other turns may still be appending.
func (e *Ephemeral) Messages(_ context.Context, sessionID string) ([]llm.Message, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	s, ok := e.sessions[sessionID]
	if !ok {
		return nil, nil
	}
	s.lastUsed = e.now()
	out := make([]llm.Message, len(s.messages))
	copy(out, s.messages)
	return out, nil
}

// Seed installs an existing history, which is what happens when a conversation
// already in progress is taken off the record: the turns so far are carried
// into memory so the conversation keeps working, and then erased from disk.
func (e *Ephemeral) Seed(sessionID string, msgs []llm.Message) {
	e.mu.Lock()
	defer e.mu.Unlock()

	held := make([]llm.Message, len(msgs))
	copy(held, msgs)
	e.sessions[sessionID] = &ephemeralSession{messages: held, lastUsed: e.now()}
	e.evictLocked()
}

// Has reports whether a session is being held in memory.
func (e *Ephemeral) Has(sessionID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.sessions[sessionID]
	return ok
}

// Len reports how many messages are held for a session, which is what the
// progress endpoint needs for a conversation that has no rows to count.
func (e *Ephemeral) Len(sessionID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[sessionID]
	if !ok {
		return 0
	}
	return len(s.messages)
}

// Drop forgets a session's in-memory transcript.
func (e *Ephemeral) Drop(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.sessions, sessionID)
}

// evictLocked drops expired sessions, then the least recently used ones if the
// pool is still over its limit. The caller holds the mutex.
func (e *Ephemeral) evictLocked() {
	now := e.now()
	for id, s := range e.sessions {
		if now.Sub(s.lastUsed) > e.ttl {
			delete(e.sessions, id)
		}
	}
	for len(e.sessions) > e.maxSessions {
		var oldestID string
		var oldest time.Time
		for id, s := range e.sessions {
			if oldestID == "" || s.lastUsed.Before(oldest) {
				oldestID, oldest = id, s.lastUsed
			}
		}
		if oldestID == "" {
			return
		}
		delete(e.sessions, oldestID)
	}
}

// transcriptFor picks where a session's history lives.
//
// The ordinary case is the store, and it stays exactly as cheap as it was. A
// session goes to the in-memory pool when it is not being recorded, and stays
// there once it has been held there even if recording is switched back on —
// otherwise going back on the record would replay a transcript missing
// everything said while off it, which is worse than either state.
func (a *Agent) transcriptFor(sess *store.Session) Transcript {
	if a.ephemeral == nil {
		return storeTranscript{store: a.store, indexer: a.indexer}
	}
	if !sess.Record || a.ephemeral.Has(sess.ID) {
		return dualTranscript{
			memory:  a.ephemeral,
			store:   a.store,
			indexer: a.indexer,
			persist: sess.Record,
		}
	}
	return storeTranscript{store: a.store, indexer: a.indexer}
}

// dualTranscript reads from memory and writes to memory, additionally writing
// to the store when the conversation is back on the record.
//
// The asymmetry is the point. Reads always come from memory because memory is
// the only place holding the whole conversation once any part of it was off
// the record. Writes reach the store only for turns exchanged while recording
// is on, so turns exchanged off the record stay unrecorded rather than being
// retroactively committed when the switch is flipped back.
type dualTranscript struct {
	memory  *Ephemeral
	store   *store.Store
	indexer Indexer
	persist bool
}

func (t dualTranscript) Append(ctx context.Context, sessionID string, msgs ...llm.Message) error {
	if err := t.memory.Append(ctx, sessionID, msgs...); err != nil {
		return err
	}
	if !t.persist {
		return nil
	}
	ids, err := t.store.AppendMessages(ctx, sessionID, msgs...)
	if err != nil {
		return err
	}
	if t.indexer != nil {
		t.indexer.Enqueue(ctx, ids)
	}
	return nil
}

func (t dualTranscript) Messages(ctx context.Context, sessionID string) ([]llm.Message, error) {
	return t.memory.Messages(ctx, sessionID)
}

// SetMemory flips a conversation's record/recall switches, moving its history
// wherever the new state requires before the store is changed.
//
// Ordering matters. Going off the record, the turns written so far are carried
// into memory *before* they are deleted, so the conversation continues with
// its context intact while nothing of it remains on disk. If the delete then
// fails, the worst case is a conversation held in memory that is also still on
// disk — recoverable — rather than a live conversation that has lost its
// history.
func (a *Agent) SetMemory(ctx context.Context, sess *store.Session, record, recall bool) error {
	if a.ephemeral != nil && !record && sess.Record {
		history, err := a.store.Messages(ctx, sess.ID)
		if err != nil {
			return err
		}
		a.ephemeral.Seed(sess.ID, history)
	}
	// SetSessionMemory deletes the persisted turns when record goes false, in
	// the same transaction as the flag itself.
	return a.store.SetSessionMemory(ctx, sess.ID, sess.ClientID, record, recall)
}

// EphemeralMessages exposes an off-the-record transcript so the browser can
// still render the conversation it is having. It never touches the store.
func (a *Agent) EphemeralMessages(ctx context.Context, sessionID string) ([]llm.Message, error) {
	if a.ephemeral == nil {
		return nil, nil
	}
	return a.ephemeral.Messages(ctx, sessionID)
}

// EphemeralLen exposes the in-memory transcript length for the progress
// endpoint, which otherwise counts rows that deliberately do not exist.
func (a *Agent) EphemeralLen(sessionID string) int {
	if a.ephemeral == nil {
		return 0
	}
	return a.ephemeral.Len(sessionID)
}
