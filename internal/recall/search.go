package recall

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"wintermute/internal/llm"
)

// Retrieval.
//
// Three rankers, fused by Reciprocal Rank Fusion:
//
//   - semantic: cosine similarity against the embedded query.
//   - lexical: BM25 through SQLite's own FTS5 index.
//   - recency: the most recent exchanges, newest first.
//
// Each is weak alone. Dense vectors handle paraphrase and lose exact rare
// terms — a filename, an error code, a serial number. BM25 does the reverse.
// And both will happily surface something from eight months ago while dropping
// what was said four turns back, because neither has any notion that recent
// things matter more.
//
// RRF combines them by rank position rather than by score: each result scores
// 1/(k+rank) from every list it appears in, and those are summed. That matters
// because the three scores are not comparable — a cosine of 0.82, a BM25 of
// -3.1 and "fourth most recent" have no common scale, and any attempt to
// normalise them into one becomes a set of hand-tuned weights that drift out
// of tune. Rank position sidesteps it entirely, and a result that two rankers
// both like beats one that a single ranker loves.
const (
	// rrfK damps the contribution of low-ranked results. 60 is the value from
	// the original paper and the one every implementation uses.
	rrfK = 60.0
	// candidateDepth is how deep each ranker looks before fusion. Retrieval
	// wants recall here and precision afterwards, so each list is generous and
	// the fused result is cut hard.
	candidateDepth = 40
)

// Scope decides what a retrieval may see.
//
// ClientID is a hard boundary and is never optional: it is the same
// authentication boundary the rest of the server enforces, and memory must not
// be the way around it.
//
// AgentID implements the one-way mirror. A conversation scoped to an agent
// recalls only that agent's own history. The unscoped assistant — Wintermute
// itself, agent_id "" — recalls everything the client owns, which is what
// makes it the memory across all the agents.
//
// The asymmetry is load-bearing and must not be "tidied" into a single OR.
// Wintermute's own conversations are tagged with the empty agent id, and they
// contain material drawn from every agent; letting a scoped agent read the
// unscoped pool would turn the god view into a laundering channel between
// exactly the things agent profiles exist to keep apart.
type Scope struct {
	ClientID int64
	AgentID  string
	// ExcludeSessionID keeps the conversation currently being held out of its
	// own retrieval. Its recent turns are already in the transcript, and
	// injecting them again as "prior context" wastes budget saying things the
	// model is about to read anyway.
	ExcludeSessionID string
}

// where builds the scope predicate and its arguments.
func (s Scope) where(alias string) (string, []any) {
	clauses := []string{alias + ".client_id = ?"}
	args := []any{s.ClientID}

	if s.AgentID != "" {
		clauses = append(clauses, alias+".agent_id = ?")
		args = append(args, s.AgentID)
	}
	if s.ExcludeSessionID != "" {
		clauses = append(clauses, alias+".session_id <> ?")
		args = append(args, s.ExcludeSessionID)
	}
	return strings.Join(clauses, " AND "), args
}

// Hit is one retrieved exchange.
type Hit struct {
	MessageID int64
	SessionID string
	// SessionTitle names the conversation a hit came from, so injected context
	// can say where it is from. Provenance is not decoration here: in the
	// unscoped view a hit may come from any agent, and both the operator and
	// the model need to be able to see which.
	SessionTitle string
	AgentID      string
	Role         string
	Content      string
	Model        string
	CreatedAt    time.Time
	// Score is the fused RRF score, and Sources names which rankers found it.
	Score   float64
	Sources []string
}

// Options tunes one retrieval.
type Options struct {
	// TopK is how many exchanges survive fusion.
	TopK int
	// RecentTurns is how many of the newest exchanges are pulled in
	// regardless of similarity.
	RecentTurns int
	// TokenBudget caps the total size of what is returned, measured in
	// estimated tokens.
	TokenBudget int
}

// Searcher retrieves prior context. It degrades rather than fails: if the
// embedder is unreachable or the index is empty, the lexical and recency
// rankers still answer, and if everything fails the caller gets no context and
// the conversation proceeds on its own transcript alone.
type Searcher struct {
	db       *sql.DB
	store    *Store
	embedder llm.Embedder
}

// NewSearcher builds a Searcher. embedder may be nil, in which case retrieval
// runs on the lexical and recency rankers alone.
func NewSearcher(db *sql.DB, store *Store, embedder llm.Embedder) *Searcher {
	return &Searcher{db: db, store: store, embedder: embedder}
}

// Search returns the prior context most relevant to query, within scope.
func (s *Searcher) Search(ctx context.Context, query string, scope Scope, opts Options) ([]Hit, error) {
	if opts.TopK <= 0 {
		opts.TopK = 6
	}
	if opts.RecentTurns < 0 {
		opts.RecentTurns = 0
	}

	// Each ranker is allowed to fail on its own. A retrieval that returns the
	// lexical results because the embedder is down is worth far more than an
	// error, and the requirement is explicit that retrieval failure must not
	// break normal chat.
	ranked := map[int64]*Hit{}
	var lists [][]int64

	if ids, err := s.semantic(ctx, query, scope); err == nil && len(ids) > 0 {
		lists = append(lists, ids)
		markSource(ranked, ids, "semantic")
	}
	if ids, err := s.lexical(ctx, query, scope); err == nil && len(ids) > 0 {
		lists = append(lists, ids)
		markSource(ranked, ids, "lexical")
	}
	if opts.RecentTurns > 0 {
		if ids, err := s.recent(ctx, scope, opts.RecentTurns); err == nil && len(ids) > 0 {
			lists = append(lists, ids)
			markSource(ranked, ids, "recent")
		}
	}
	if len(lists) == 0 {
		return nil, nil
	}

	fused := fuse(lists)
	if len(fused) > opts.TopK {
		fused = fused[:opts.TopK]
	}

	hits, err := s.hydrate(ctx, fused, ranked)
	if err != nil {
		return nil, err
	}
	return withinBudget(hits, opts.TokenBudget), nil
}

// semantic ranks by cosine similarity against the embedded query.
//
// The scan is exhaustive over the scoped vectors rather than an approximate
// index. At the scale this runs at — one household's conversations — that is
// microseconds, and it buys two things worth more than the speed: a delete is
// a real delete, with no graph left holding a reconstructible copy of removed
// content, and there is no index structure to rebuild or migrate as the years
// pass.
func (s *Searcher) semantic(ctx context.Context, query string, scope Scope) ([]int64, error) {
	if s.embedder == nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	vectors, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, nil
	}
	q := vectors[0]

	pred, args := scope.where("v")
	rows, err := s.db.QueryContext(ctx,
		`SELECT v.message_id, v.vector FROM recall_vectors v WHERE `+pred, args...)
	if err != nil {
		return nil, fmt.Errorf("recall: scan vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type scored struct {
		id    int64
		score float64
	}
	var all []scored
	for rows.Next() {
		var id int64
		var buf []byte
		if err := rows.Scan(&id, &buf); err != nil {
			return nil, fmt.Errorf("recall: scan vector: %w", err)
		}
		v, err := decodeVector(buf)
		if err != nil {
			// One unreadable row must not take the whole retrieval down.
			continue
		}
		sim := cosine(q, v)
		if sim <= 0 {
			continue
		}
		all = append(all, scored{id: id, score: sim})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recall: scan vectors: %w", err)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].id < all[j].id
	})
	if len(all) > candidateDepth {
		all = all[:candidateDepth]
	}
	out := make([]int64, len(all))
	for i, sc := range all {
		out[i] = sc.id
	}
	return out, nil
}

// lexical ranks by BM25 through FTS5. This is what catches the exact rare
// token — a filename, a postcode, an error string — that a dense vector
// smooths away.
func (s *Searcher) lexical(ctx context.Context, query string, scope Scope) ([]int64, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}

	// The FTS table carries no scope of its own — it is contentless and keyed
	// by message id — so it is joined to recall_vectors, which does. That also
	// means the lexical ranker can only return messages that were indexed,
	// which is the intent: the same role restriction applies to both halves.
	pred, args := scope.where("v")
	queryArgs := append([]any{match}, args...)
	queryArgs = append(queryArgs, candidateDepth)

	rows, err := s.db.QueryContext(ctx,
		`SELECT f.rowid
		 FROM recall_fts f
		 JOIN recall_vectors v ON v.message_id = f.rowid
		 WHERE recall_fts MATCH ? AND `+pred+`
		 ORDER BY bm25(recall_fts)
		 LIMIT ?`, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("recall: lexical search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("recall: scan lexical hit: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// recent returns the newest indexed exchanges in scope.
func (s *Searcher) recent(ctx context.Context, scope Scope, limit int) ([]int64, error) {
	pred, args := scope.where("v")
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT v.message_id FROM recall_vectors v
		 WHERE `+pred+`
		 ORDER BY v.created_at DESC, v.message_id DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("recall: recent search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("recall: scan recent hit: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// fuse combines ranked lists by Reciprocal Rank Fusion.
func fuse(lists [][]int64) []int64 {
	scores := map[int64]float64{}
	first := map[int64]int{}
	for _, list := range lists {
		for rank, id := range list {
			scores[id] += 1.0 / (rrfK + float64(rank+1))
			if _, seen := first[id]; !seen {
				first[id] = rank
			}
		}
	}

	ids := make([]int64, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		// Deterministic ties, so asking the same question twice retrieves the
		// same context and an answer stays reproducible.
		if first[ids[i]] != first[ids[j]] {
			return first[ids[i]] < first[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

// markSource records which rankers found each id, and the fused score.
func markSource(into map[int64]*Hit, ids []int64, source string) {
	for _, id := range ids {
		h, ok := into[id]
		if !ok {
			h = &Hit{MessageID: id}
			into[id] = h
		}
		h.Sources = append(h.Sources, source)
	}
}

// hydrate loads the text and provenance for the fused ids, preserving order.
func (s *Searcher) hydrate(ctx context.Context, ids []int64, meta map[int64]*Hit) ([]Hit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.session_id, s.title, s.agent_id, m.role, m.content, m.model, m.created_at
		 FROM messages m
		 JOIN sessions s ON s.id = m.session_id
		 WHERE m.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("recall: hydrate hits: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := map[int64]Hit{}
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.MessageID, &h.SessionID, &h.SessionTitle, &h.AgentID,
			&h.Role, &h.Content, &h.Model, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("recall: scan hit: %w", err)
		}
		if m, ok := meta[h.MessageID]; ok {
			h.Sources = m.Sources
		}
		byID[h.MessageID] = h
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recall: hydrate hits: %w", err)
	}

	out := make([]Hit, 0, len(ids))
	for _, id := range ids {
		if h, ok := byID[id]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// withinBudget trims the hit list to an estimated token budget.
//
// If the best hit alone is larger than the whole budget it is shortened rather
// than dropped. Returning nothing in that case would be the worst outcome:
// memory would silently do nothing precisely when the relevant exchange was a
// long one, and the operator would have no way to tell that from having no
// relevant history at all. A shortened excerpt is visibly shortened.
func withinBudget(hits []Hit, budget int) []Hit {
	if budget <= 0 {
		return hits
	}
	out := make([]Hit, 0, len(hits))
	var used int
	for _, h := range hits {
		cost := EstimateTokens(h.Content)
		if used+cost <= budget {
			out = append(out, h)
			used += cost
			continue
		}
		if len(out) > 0 {
			break
		}
		h.Content = truncateToTokens(h.Content, budget)
		out = append(out, h)
		break
	}
	return out
}

// truncateToTokens shortens text to roughly a token budget, cutting at a word
// boundary and marking the cut so neither the reader nor the model mistakes a
// fragment for the whole thing.
func truncateToTokens(s string, tokens int) string {
	const marker = " […truncated]"
	limit := tokens * 4
	if limit <= len(marker) || len(s) <= limit {
		return s
	}
	cut := limit - len(marker)
	if idx := strings.LastIndexByte(s[:cut], ' '); idx > cut/2 {
		cut = idx
	}
	return strings.TrimSpace(s[:cut]) + marker
}

// EstimateTokens approximates a token count from text length.
//
// It is an estimate and is named like one. The exact figure differs per
// tokenizer, and this store is read by models whose tokenizers are not all
// known here — counting precisely would mean binding the memory layer to one
// model's tokenizer, which is the coupling this whole package exists to
// avoid. Four characters per token is the usual rule of thumb and errs on the
// side of over-counting for English prose, which is the right direction for a
// budget.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s)/4 + 1
}

// ftsQuery turns free text into an FTS5 MATCH expression.
//
// Every term is quoted and the terms are OR-ed. Quoting is not cosmetic: FTS5
// MATCH has its own syntax, and a user message containing a bare "AND", a
// quote or a wildcard would otherwise be parsed as an operator and either
// error or silently mean something else. This is untrusted text in a query
// language, and it gets escaped like it.
func ftsQuery(query string) string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	})
	terms := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		f = strings.Trim(f, "-_")
		if len(f) < 2 || seen[f] || ftsStopwords[f] {
			continue
		}
		seen[f] = true
		terms = append(terms, `"`+f+`"`)
	}
	if len(terms) == 0 {
		return ""
	}
	// Bounded so a pasted page does not become a thousand-term query.
	if len(terms) > 32 {
		terms = terms[:32]
	}
	return strings.Join(terms, " OR ")
}

// ftsStopwords keeps the most common words from dominating a BM25 query. It is
// short deliberately — the same reasoning as internal/knowledge, where
// dropping domain words would gut the signal.
var ftsStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "but": true, "by": true, "can": true, "did": true, "do": true,
	"does": true, "for": true, "from": true, "had": true, "has": true, "have": true,
	"how": true, "if": true, "in": true, "is": true, "it": true, "its": true,
	"of": true, "on": true, "or": true, "that": true, "the": true, "then": true,
	"this": true, "to": true, "was": true, "were": true, "what": true, "when": true,
	"which": true, "will": true, "with": true, "you": true, "your": true,
}
