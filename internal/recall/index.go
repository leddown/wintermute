package recall

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"wintermute/internal/llm"
)

// Indexable roles.
//
// Only what the operator said and what the assistant replied is indexed. Tool
// results are deliberately excluded, and the reason is security rather than
// tidiness.
//
// A transcript contains web search results, fetched pages, and file listings —
// text this server did not write and did not vet. Indexing it would make any
// of it eligible for automatic injection into a future conversation as trusted
// prior context. That is the memory-poisoning path: unlike a prompt injection
// that lives for one turn, a poisoned memory is retrieved again and again, and
// the model treats retrieved memory as authoritative. Published work has shown
// a handful of crafted documents steering a retrieval system most of the time.
//
// Tool *outcomes* are still recorded in muninn and still retrievable as
// episodic memory, where they are read as a record of what happened rather
// than as remembered context. What is excluded here is the raw returned text.
var indexedRoles = map[llm.Role]bool{
	llm.RoleUser:      true,
	llm.RoleAssistant: true,
}

// Indexer keeps the retrieval index up to date with the message store.
//
// It runs behind the write path, never in it. A message is committed to the
// store and queued here; the embedding happens afterwards. Losing an embedding
// is recoverable — it can be recomputed from the message that is still there —
// while losing the message is not, so the message never waits on the embedder.
type Indexer struct {
	db       *sql.DB
	store    *Store
	embedder llm.Embedder
	log      *slog.Logger
	// batch is how many messages are embedded per request. Embedding models
	// accept batches, and a batch of one would spend most of its time on HTTP.
	batch int
	// maxAttempts is when a message that will not embed stops being retried at
	// the front of the queue, so one bad row cannot block everything behind it.
	maxAttempts int
	// wake carries a nudge from the write path so a new message is indexed
	// promptly rather than at the next tick. It is buffered and dropped when
	// full: a missed nudge only means waiting for the tick.
	wake chan struct{}
}

// NewIndexer builds an indexer.
func NewIndexer(db *sql.DB, store *Store, embedder llm.Embedder, log *slog.Logger) *Indexer {
	return &Indexer{
		db:          db,
		store:       store,
		embedder:    embedder,
		log:         log,
		batch:       16,
		maxAttempts: 5,
		wake:        make(chan struct{}, 1),
	}
}

// Enqueue marks messages for indexing. It is called after the transcript has
// been committed, and its failures are logged rather than returned: a message
// that could not be queued is still safely stored, and the backfill command
// exists precisely to pick up whatever the queue missed.
//
// Messages from conversations that are not being recorded never reach here,
// because they have no rows to enqueue.
func (ix *Indexer) Enqueue(ctx context.Context, messageIDs []int64) {
	if len(messageIDs) == 0 {
		return
	}
	now := time.Now().UTC()
	for _, id := range messageIDs {
		if _, err := ix.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO recall_queue (message_id, enqueued_at) VALUES (?, ?)`,
			id, now); err != nil {
			ix.log.Warn("recall: could not queue message for indexing",
				"message", id, "error", err)
		}
	}
	ix.Nudge()
}

// Nudge asks the indexer to run now rather than at its next tick.
func (ix *Indexer) Nudge() {
	select {
	case ix.wake <- struct{}{}:
	default:
	}
}

// Run works the queue until ctx is cancelled.
func (ix *Indexer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	ix.log.Info("recall indexer started", "embedder", ix.embedder.Name(), "interval", interval)

	for {
		select {
		case <-ctx.Done():
			ix.log.Info("recall indexer stopped")
			return
		case <-ticker.C:
			ix.drain(ctx)
		case <-ix.wake:
			ix.drain(ctx)
		}
	}
}

// drain works batches until the queue is empty or something goes wrong. A
// failure just stops this pass; the next tick tries again.
func (ix *Indexer) drain(ctx context.Context) {
	for {
		n, err := ix.IndexBatch(ctx)
		if err != nil {
			// An unreachable embedder is an expected state on a home network,
			// not an incident: the machine serving it may simply be off. The
			// queue keeps the work.
			if errors.Is(err, llm.ErrEmbedderUnavailable) {
				ix.log.Debug("recall: embedder unavailable, backlog retained", "error", err)
			} else {
				ix.log.Warn("recall: indexing failed", "error", err)
			}
			return
		}
		if n == 0 {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// pending is one queued message with the text and scope it needs.
type pending struct {
	messageID int64
	sessionID string
	clientID  int64
	agentID   string
	role      string
	content   string
	createdAt time.Time
}

// IndexBatch embeds and stores up to one batch of queued messages, returning
// how many it indexed. Exported so the backfill and reindex commands can drive
// it to completion without duplicating the logic.
func (ix *Indexer) IndexBatch(ctx context.Context) (int, error) {
	items, err := ix.claim(ctx)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}

	inputs := make([]string, len(items))
	for i, it := range items {
		inputs[i] = it.content
	}

	vectors, err := ix.embedder.Embed(ctx, inputs)
	if err != nil {
		ix.recordFailure(ctx, items, err)
		return 0, err
	}

	// The first vector to be stored fixes the index's dimension. Everything
	// afterwards is checked against it, because a model that quietly changed
	// width would otherwise fill the store with vectors that cannot be
	// compared to the ones already there.
	if err := ix.store.SetPin(ctx, ix.embedder.Name(), len(vectors[0])); err != nil {
		return 0, err
	}
	pin, err := ix.store.Pin(ctx)
	if err != nil {
		return 0, err
	}

	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("recall: begin index write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var indexed int
	for i, it := range items {
		v := vectors[i]
		if pin != nil && len(v) != pin.Dimension {
			return 0, fmt.Errorf(
				"recall: embedder %q returned a %d-dimension vector but the index is %d-dimension; "+
					"the embedding model appears to have changed under a pinned name",
				ix.embedder.Name(), len(v), pin.Dimension)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO recall_vectors
			 (message_id, session_id, client_id, agent_id, role, created_at, dim, vector)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			it.messageID, it.sessionID, it.clientID, it.agentID, it.role,
			it.createdAt, len(v), encodeVector(v)); err != nil {
			return 0, fmt.Errorf("recall: store vector: %w", err)
		}

		// The lexical index shares the message's rowid, so the two halves of
		// retrieval agree on what a result is without a mapping table.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM recall_fts WHERE rowid = ?`, it.messageID); err != nil {
			return 0, fmt.Errorf("recall: clear lexical entry: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO recall_fts (rowid, content) VALUES (?, ?)`,
			it.messageID, it.content); err != nil {
			return 0, fmt.Errorf("recall: store lexical entry: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM recall_queue WHERE message_id = ?`, it.messageID); err != nil {
			return 0, fmt.Errorf("recall: dequeue: %w", err)
		}
		indexed++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("recall: commit index write: %w", err)
	}
	return indexed, nil
}

// claim reads the next batch of queued work, joined to everything the vector
// row needs. Messages whose role is not indexed, or whose text is empty, are
// dropped from the queue here rather than embedded.
func (ix *Indexer) claim(ctx context.Context) ([]pending, error) {
	rows, err := ix.db.QueryContext(ctx,
		`SELECT q.message_id, m.session_id, s.client_id, s.agent_id, m.role, m.content, m.created_at
		 FROM recall_queue q
		 JOIN messages m ON m.id = q.message_id
		 JOIN sessions s ON s.id = m.session_id
		 WHERE q.attempts < ?
		 ORDER BY q.enqueued_at
		 LIMIT ?`, ix.maxAttempts, ix.batch)
	if err != nil {
		return nil, fmt.Errorf("recall: read queue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var items []pending
	var skip []int64
	for rows.Next() {
		var it pending
		if err := rows.Scan(&it.messageID, &it.sessionID, &it.clientID, &it.agentID,
			&it.role, &it.content, &it.createdAt); err != nil {
			return nil, fmt.Errorf("recall: scan queue: %w", err)
		}
		if !indexedRoles[llm.Role(it.role)] || strings.TrimSpace(it.content) == "" {
			skip = append(skip, it.messageID)
			continue
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recall: read queue: %w", err)
	}

	for _, id := range skip {
		if _, err := ix.db.ExecContext(ctx, `DELETE FROM recall_queue WHERE message_id = ?`, id); err != nil {
			return nil, fmt.Errorf("recall: drop unindexable message: %w", err)
		}
	}
	return items, nil
}

// recordFailure counts an attempt against each message in a failed batch, so a
// row that can never be embedded eventually stops holding up the queue.
func (ix *Indexer) recordFailure(ctx context.Context, items []pending, cause error) {
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	for _, it := range items {
		if _, err := ix.db.ExecContext(ctx,
			`UPDATE recall_queue SET attempts = attempts + 1, last_error = ? WHERE message_id = ?`,
			msg, it.messageID); err != nil {
			ix.log.Warn("recall: could not record indexing failure", "message", it.messageID, "error", err)
		}
	}
}

// Backlog reports how much work is queued, for the backfill command and for
// anyone asking whether the index has caught up.
func (ix *Indexer) Backlog(ctx context.Context) (int, error) {
	var n int
	if err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM recall_queue`).Scan(&n); err != nil {
		return 0, fmt.Errorf("recall: read backlog: %w", err)
	}
	return n, nil
}

// QueueEverything enqueues every message eligible for indexing that is not
// already indexed. This is the backfill: it is how history that predates the
// index — or that was written while the embedder was down — gets picked up.
//
// It is safe to run at any time and does not re-embed what is already there.
func (ix *Indexer) QueueEverything(ctx context.Context) (int, error) {
	roles := make([]string, 0, len(indexedRoles))
	for role := range indexedRoles {
		roles = append(roles, string(role))
	}
	// The role list is built from a constant map, never from input.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(roles)), ",")
	args := make([]any, 0, len(roles))
	for _, r := range roles {
		args = append(args, r)
	}

	res, err := ix.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO recall_queue (message_id, enqueued_at)
		 SELECT m.id, ?
		 FROM messages m
		 WHERE m.role IN (`+placeholders+`)
		   AND TRIM(m.content) <> ''
		   AND NOT EXISTS (SELECT 1 FROM recall_vectors v WHERE v.message_id = m.id)`,
		append([]any{time.Now().UTC()}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("recall: queue everything: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recall: queue everything: %w", err)
	}
	ix.Nudge()
	return int(n), nil
}

// DrainFully works the queue to completion, which is what the backfill and
// reindex commands want: they are run by a person waiting for an answer, not
// by a ticker.
func (ix *Indexer) DrainFully(ctx context.Context) (int, error) {
	var total int
	for {
		n, err := ix.IndexBatch(ctx)
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, nil
		}
		total += n
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}
