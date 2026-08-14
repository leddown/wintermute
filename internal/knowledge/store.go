package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store persists agents and their documents. It takes the shared *sql.DB rather
// than the store.Store wrapper, matching how the workspace modules reach the
// same database.
type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// ---- agents ----

const agentColumns = `a.id, a.name, a.description, a.system_prompt, a.backend, a.model,
	a.sources, a.created_at, a.updated_at,
	(SELECT COUNT(*) FROM agent_documents d WHERE d.agent_id = a.id)`

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var sources string
	if err := row.Scan(&a.ID, &a.Name, &a.Description, &a.SystemPrompt, &a.Backend, &a.Model,
		&sources, &a.CreatedAt, &a.UpdatedAt, &a.DocumentCount); err != nil {
		return nil, err
	}
	a.Sources = decodeSources(sources)
	return &a, nil
}

// CreateAgent stores a new agent. The caller validates first.
func (s *Store) CreateAgent(ctx context.Context, a *Agent) (*Agent, error) {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents
		(id, name, description, system_prompt, backend, model, sources, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, a.Description, a.SystemPrompt, a.Backend, a.Model,
		encodeSources(a.Sources), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, Invalidf("an agent with id %q already exists", a.ID)
		}
		return nil, fmt.Errorf("create agent: %w", err)
	}
	return s.Agent(ctx, a.ID)
}

// UpdateAgent replaces an agent's configuration. Its documents are untouched.
func (s *Store) UpdateAgent(ctx context.Context, a *Agent) (*Agent, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE agents
		SET name = ?, description = ?, system_prompt = ?, backend = ?, model = ?,
		    sources = ?, updated_at = ?
		WHERE id = ?`,
		a.Name, a.Description, a.SystemPrompt, a.Backend, a.Model,
		encodeSources(a.Sources), time.Now().UTC(), a.ID)
	if err != nil {
		return nil, fmt.Errorf("update agent: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil, NotFound("agent " + a.ID)
	}
	return s.Agent(ctx, a.ID)
}

func (s *Store) Agent(ctx context.Context, id string) (*Agent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM agents a WHERE a.id = ?`, id)
	agent, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFound("agent " + id)
	}
	return agent, err
}

func (s *Store) Agents(ctx context.Context) ([]*Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentColumns+` FROM agents a ORDER BY a.name`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*Agent{}
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, agent)
	}
	return out, rows.Err()
}

// DeleteAgent removes an agent and its library. Sessions that used it keep
// their transcripts and fall back to the unscoped assistant, because deleting
// a configuration should not delete a conversation.
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return NotFound("agent " + id)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET agent_id = '' WHERE agent_id = ?`, id); err != nil {
		return fmt.Errorf("detach sessions from agent: %w", err)
	}
	return nil
}

// ---- documents ----

const documentColumns = `d.id, d.agent_id, d.title, d.filename, d.media_type, d.source_url,
	d.sha256, d.byte_size, d.text_chars, d.extract_via, d.uploaded_at,
	(SELECT COUNT(*) FROM agent_document_chunks c WHERE c.document_id = d.id)`

func scanDocument(row interface{ Scan(...any) error }) (*Document, error) {
	var d Document
	err := row.Scan(&d.ID, &d.AgentID, &d.Title, &d.Filename, &d.MediaType, &d.SourceURL,
		&d.SHA256, &d.ByteSize, &d.TextChars, &d.ExtractVia, &d.UploadedAt, &d.ChunkCount)
	return &d, err
}

// AddDocument stores a document and its chunks in one transaction. A document
// row with no chunks would list in the UI as searchable and retrieve nothing.
func (s *Store) AddDocument(ctx context.Context, doc *Document, chunks []Chunk) (*Document, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `INSERT INTO agent_documents
		(agent_id, title, filename, media_type, source_url, sha256, byte_size, text_chars, extract_via, uploaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		doc.AgentID, doc.Title, doc.Filename, doc.MediaType, doc.SourceURL,
		doc.SHA256, doc.ByteSize, doc.TextChars, doc.ExtractVia, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("insert document: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO agent_document_chunks (document_id, ordinal, heading, body) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()

	for _, chunk := range chunks {
		if _, err := stmt.ExecContext(ctx, id, chunk.Ordinal, chunk.Heading, chunk.Body); err != nil {
			return nil, fmt.Errorf("insert chunk %d: %w", chunk.Ordinal, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Document(ctx, id)
}

func (s *Store) Document(ctx context.Context, id int64) (*Document, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+documentColumns+` FROM agent_documents d WHERE d.id = ?`, id)
	doc, err := scanDocument(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFound(fmt.Sprintf("document %d", id))
	}
	return doc, err
}

func (s *Store) Documents(ctx context.Context, agentID string) ([]*Document, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+documentColumns+` FROM agent_documents d WHERE d.agent_id = ? ORDER BY d.id DESC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*Document{}
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// DocumentBySHA backs duplicate detection within one agent's library. The same
// document in two agents' libraries is normal and allowed.
func (s *Store) DocumentBySHA(ctx context.Context, agentID, sha string) (*Document, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+documentColumns+
		` FROM agent_documents d WHERE d.agent_id = ? AND d.sha256 = ?`, agentID, sha)
	doc, err := scanDocument(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFound("document")
	}
	return doc, err
}

func (s *Store) DeleteDocument(ctx context.Context, agentID string, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_documents WHERE id = ? AND agent_id = ?`, id, agentID)
	if err != nil {
		return fmt.Errorf("delete document: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return NotFound(fmt.Sprintf("document %d", id))
	}
	// The chunk rows cascade, but only where foreign keys are enforced.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_document_chunks WHERE document_id = ?`, id); err != nil {
		return fmt.Errorf("delete document chunks: %w", err)
	}
	return nil
}

// Chunks returns every chunk in an agent's library, for the search tool to
// score. A library is a handful of documents, so this is a small read; if one
// ever is not, this is the query to page.
func (s *Store) Chunks(ctx context.Context, agentID string) ([]Chunk, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.document_id, c.ordinal, c.heading, c.body,
		d.title, d.filename
		FROM agent_document_chunks c
		JOIN agent_documents d ON d.id = c.document_id
		WHERE d.agent_id = ?
		ORDER BY c.document_id, c.ordinal`, agentID)
	if err != nil {
		return nil, fmt.Errorf("read chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Chunk{}
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Ordinal, &c.Heading, &c.Body,
			&c.Title, &c.Filename); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DocumentChunks returns one document's chunks in order, for reading a passage
// the search tool pointed at.
func (s *Store) DocumentChunks(ctx context.Context, documentID int64, from, count int) ([]Chunk, error) {
	if from < 0 {
		from = 0
	}
	if count <= 0 {
		count = 3
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.document_id, c.ordinal, c.heading, c.body,
		d.title, d.filename
		FROM agent_document_chunks c
		JOIN agent_documents d ON d.id = c.document_id
		WHERE c.document_id = ? AND c.ordinal >= ?
		ORDER BY c.ordinal LIMIT ?`, documentID, from, count)
	if err != nil {
		return nil, fmt.Errorf("read document chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Chunk{}
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Ordinal, &c.Heading, &c.Body,
			&c.Title, &c.Filename); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}
