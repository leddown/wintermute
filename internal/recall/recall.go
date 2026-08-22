// Package recall is Wintermute's memory layer: it indexes the conversation
// store and retrieves relevant prior context for a new turn.
//
// It sits *above* the model and is never coupled to one. What it indexes is
// the neutral role/content text already in `messages`, and what it produces is
// a block of prior context handed to whichever model is answering. Swapping the
// chat model changes nothing here.
//
// The name avoids "memory", which in this codebase already means RAM — see
// config.ParseMemory and the backend sizing in internal/models.
//
// # Two streams
//
// Semantic memory is what was said: the messages themselves. Episodic memory
// is what was done: muninn's record of every proposed tool call, the decision
// taken on it, and the outcome. They answer different questions ("what did I
// tell you about the boiler" versus "what did you actually rename last
// Tuesday") and both are retrievable.
//
// # What is deliberately not done
//
// Facts are not extracted from conversations by a model. It is a popular
// design and it is the wrong one here: extraction bakes one model's reading of
// a conversation into the store, so a history extracted in 2026 carries 2026's
// model's judgement forever, and re-extracting later with a different model
// yields different "facts". Raw text is the only substrate that stays neutral
// across the model changes this store is built to outlive. Summaries can be
// derived later as another rebuildable layer if they earn their place.
package recall

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"wintermute/internal/llm"
)

// ErrEmbedderMismatch reports that the configured embedding model is not the
// one the existing index was built with.
var ErrEmbedderMismatch = errors.New("recall: configured embedder does not match the indexed one")

// Pin is the embedding model an index was built with.
type Pin struct {
	Model     string
	Dimension int
	CreatedAt time.Time
}

// Store holds the vectors and the lexical index. It takes the shared *sql.DB,
// matching how the other modules reach the same database.
type Store struct{ db *sql.DB }

// NewStore builds a Store.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Pin reads the recorded embedder, if the index has been built at all.
func (s *Store) Pin(ctx context.Context) (*Pin, error) {
	var p Pin
	err := s.db.QueryRowContext(ctx,
		`SELECT embedding_model, dimension, created_at FROM recall_meta WHERE id = 1`).
		Scan(&p.Model, &p.Dimension, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recall: read pin: %w", err)
	}
	return &p, nil
}

// SetPin records the embedder on first index. It refuses to overwrite an
// existing pin: changing the embedder is a reindex, which erases the vectors
// first and is a deliberate operation, not something a startup should do.
func (s *Store) SetPin(ctx context.Context, model string, dimension int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO recall_meta (id, embedding_model, dimension, created_at)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		model, dimension, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("recall: set pin: %w", err)
	}
	return nil
}

// ClearIndex removes every vector, every lexical entry and the pin, which is
// what a deliberate reindex starts with. The messages themselves are not
// touched — this is a derived index, and rebuilding it must never be able to
// cost the operator the thing it was derived from.
func (s *Store) ClearIndex(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recall: begin clear: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range []string{
		`DELETE FROM recall_vectors`,
		`DELETE FROM recall_fts`,
		`DELETE FROM recall_queue`,
		`DELETE FROM recall_meta`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("recall: clear index: %w", err)
		}
	}
	return tx.Commit()
}

// CheckPin compares the configured embedder against what the index was built
// with, and reports a mismatch rather than letting retrieval run against
// vectors from another model's space.
//
// This is the failure that otherwise happens silently. Distances between
// vectors from two different models are arithmetically valid and semantically
// meaningless, so nothing errors — retrieval simply starts returning
// plausible-looking nonsense, and it can be months before anyone notices.
func (s *Store) CheckPin(ctx context.Context, embedder llm.Embedder) error {
	pin, err := s.Pin(ctx)
	if err != nil {
		return err
	}
	if pin == nil {
		// Nothing indexed yet: the first write pins it.
		return nil
	}
	if pin.Model != embedder.Name() {
		return fmt.Errorf(
			"%w: the index was built with %q (%d dimensions) on %s, but the server is configured for %q.\n"+
				"Retrieving against vectors from a different model returns meaningless results, so this is refused.\n"+
				"Either set the embedder back to %q, or rebuild the index deliberately with: wintermuted -reindex-memory",
			ErrEmbedderMismatch, pin.Model, pin.Dimension,
			pin.CreatedAt.Format("2006-01-02"), embedder.Name(), pin.Model)
	}
	return nil
}

// ---- vector encoding -------------------------------------------------------
//
// Vectors are stored as little-endian float32, which is what every embedding
// model produces and what sqlite-vec's own float32 format uses — so if a vec0
// index is ever added beside this, the bytes can be handed straight over.

// encodeVector packs a vector into its stored form.
func encodeVector(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeVector unpacks a stored vector.
func decodeVector(buf []byte) ([]float32, error) {
	if len(buf)%4 != 0 {
		return nil, fmt.Errorf("recall: stored vector is %d bytes, not a whole number of float32s", len(buf))
	}
	out := make([]float32, len(buf)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return out, nil
}

// cosine returns the cosine similarity of two vectors, or 0 if they cannot be
// compared. A zero vector has no direction, so it matches nothing rather than
// dividing by zero.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
