package app

import (
	"context"
	"log/slog"
	"time"

	"wintermute/internal/recall"
	"wintermute/internal/store"
)

// memoryLayer adapts internal/recall to the agent's Recaller interface.
//
// It is the only place that knows both what a session is and what the memory
// layer needs: the scope a retrieval is allowed to see, and how big a slice of
// the answering model's context window the result may occupy.
type memoryLayer struct {
	searcher *recall.Searcher
	store    *store.Store
	log      *slog.Logger

	topK        int
	recentTurns int
	fraction    float64
	budget      int
}

// Framing implements agent.Recaller.
func (m *memoryLayer) Framing() string { return recall.SystemPromptAddendum }

// Recall implements agent.Recaller.
//
// Every failure path returns "". Retrieval is an enhancement to a conversation
// that works without it, so a broken index, an unreachable embedder or a
// database error must be indistinguishable from having found nothing — the
// turn proceeds on its own transcript. Failures are logged at debug because on
// a home network the usual cause is that the machine serving the embedding
// model is switched off, which is not an incident.
func (m *memoryLayer) Recall(ctx context.Context, sess *store.Session, query string) string {
	scope := recall.Scope{
		ClientID: sess.ClientID,
		// The one-way mirror. A session scoped to an agent recalls that
		// agent's history alone; the unscoped assistant — Wintermute itself,
		// agent_id "" — recalls everything this client owns, which is what
		// makes it the memory across all the agents. Passing the session's own
		// agent id through unchanged is what implements both cases, and the
		// asymmetry lives in recall.Scope.where.
		AgentID: sess.AgentID,
		// The current conversation is already in the transcript; injecting its
		// own turns back as "prior context" would spend the budget repeating
		// what the model is about to read anyway.
		ExcludeSessionID: sess.ID,
	}

	opts := recall.Options{
		TopK:        m.topK,
		RecentTurns: m.recentTurns,
		TokenBudget: recall.BudgetFor(m.contextLenFor(ctx, sess), m.fraction, m.budget),
	}

	hits, err := m.searcher.Search(ctx, query, scope, opts)
	if err != nil {
		m.log.Debug("recall: retrieval failed, answering without prior context",
			"session", sess.ID, "error", err)
		return ""
	}
	if len(hits) == 0 {
		return ""
	}
	m.log.Debug("recall: prior context retrieved",
		"session", sess.ID, "hits", len(hits), "budget", opts.TokenBudget)
	return recall.Render(hits, time.Now().UTC())
}

// contextLenFor finds the answering model's context window from the probed
// catalog, so the memory budget is a fraction of the room actually available
// rather than a fixed number that is too big for a small local model and too
// small for a large one.
//
// Zero means unknown, and the caller falls back to a conservative absolute
// budget rather than a percentage of a guess.
func (m *memoryLayer) contextLenFor(ctx context.Context, sess *store.Session) int {
	if sess.Model == "" {
		return 0
	}
	rows, err := m.store.Catalog(ctx)
	if err != nil {
		return 0
	}
	for _, row := range rows {
		if row.ModelID != sess.Model {
			continue
		}
		// A session pinned to a backend must match that backend's copy of the
		// model; the same model id served by two backends can be configured
		// with different context lengths.
		if sess.Backend != "" && row.Backend != sess.Backend {
			continue
		}
		return row.CtxLen
	}
	return 0
}
