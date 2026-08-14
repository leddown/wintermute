package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
)

// Service is the module's entry point: agents, their libraries, and search over
// them.
type Service struct{ store *Store }

func NewService(store *Store) *Service { return &Service{store: store} }

// ---- agents ----

func (s *Service) Agents(ctx context.Context) ([]*Agent, error) { return s.store.Agents(ctx) }

func (s *Service) Agent(ctx context.Context, id string) (*Agent, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, Invalidf("an agent id is required")
	}
	return s.store.Agent(ctx, id)
}

// Lookup resolves an agent id that may be empty, returning nil for "no agent".
// The unscoped assistant is a valid configuration — it is what every session
// was before agents existed — so an empty id is not an error.
func (s *Service) Lookup(ctx context.Context, id string) (*Agent, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	agent, err := s.store.Agent(ctx, id)
	if err != nil {
		var notFound ErrNotFound
		if errors.As(err, &notFound) {
			// A session pinned to a deleted agent keeps working, unscoped,
			// rather than failing every turn.
			return nil, nil
		}
		return nil, err
	}
	return agent, nil
}

func (s *Service) CreateAgent(ctx context.Context, a *Agent) (*Agent, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return s.store.CreateAgent(ctx, a)
}

func (s *Service) UpdateAgent(ctx context.Context, a *Agent) (*Agent, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return s.store.UpdateAgent(ctx, a)
}

func (s *Service) DeleteAgent(ctx context.Context, id string) error {
	return s.store.DeleteAgent(ctx, id)
}

// ---- documents ----

// UploadInput is a document as it arrives from the browser.
type UploadInput struct {
	AgentID   string
	Title     string
	Filename  string
	MediaType string
	SourceURL string
	Body      []byte
}

// Upload extracts, chunks and stores a document in an agent's library.
func (s *Service) Upload(ctx context.Context, in UploadInput) (*Document, error) {
	agent, err := s.Agent(ctx, in.AgentID)
	if err != nil {
		return nil, err
	}

	filename := strings.TrimSpace(in.Filename)
	if filename == "" {
		return nil, Invalidf("a filename is required, so the document type can be read")
	}

	extraction, err := Extract(filename, in.MediaType, in.Body)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(in.Body)
	sha := hex.EncodeToString(sum[:])
	if existing, err := s.store.DocumentBySHA(ctx, agent.ID, sha); err == nil {
		return nil, Invalidf("%q is already in this agent's library as %q", filename, existing.Title)
	} else {
		var notFound ErrNotFound
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}

	chunks := ChunkText(extraction.Text)
	if len(chunks) == 0 {
		return nil, Invalidf("no text could be read out of %s", filename)
	}

	base := filepath.Base(filename)
	doc := &Document{
		AgentID:    agent.ID,
		Title:      firstNonEmpty(strings.TrimSpace(in.Title), strings.TrimSuffix(base, filepath.Ext(base))),
		Filename:   base,
		MediaType:  strings.TrimSpace(in.MediaType),
		SourceURL:  strings.TrimSpace(in.SourceURL),
		SHA256:     sha,
		ByteSize:   int64(len(in.Body)),
		TextChars:  len(extraction.Text),
		ExtractVia: extraction.Via,
	}
	return s.store.AddDocument(ctx, doc, chunks)
}

func (s *Service) Documents(ctx context.Context, agentID string) ([]*Document, error) {
	if _, err := s.Agent(ctx, agentID); err != nil {
		return nil, err
	}
	return s.store.Documents(ctx, agentID)
}

func (s *Service) DeleteDocument(ctx context.Context, agentID string, id int64) error {
	if _, err := s.Agent(ctx, agentID); err != nil {
		return err
	}
	return s.store.DeleteDocument(ctx, agentID, id)
}

// ---- search ----

// SearchLibrary ranks an agent's chunks against a query.
func (s *Service) SearchLibrary(ctx context.Context, agentID, query string, limit int) ([]Hit, error) {
	chunks, err := s.store.Chunks(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return Search(query, chunks, limit), nil
}

// ReadDocument returns consecutive chunks of one document, for following up a
// search hit with its surrounding text.
func (s *Service) ReadDocument(ctx context.Context, agentID string, documentID int64, from, count int) (*Document, []Chunk, error) {
	doc, err := s.store.Document(ctx, documentID)
	if err != nil {
		return nil, nil, err
	}
	// An agent may only read its own library; without this check a document id
	// guessed from another agent's conversation would cross the boundary this
	// whole package exists to draw.
	if doc.AgentID != agentID {
		return nil, nil, NotFound("document in this agent's library")
	}
	chunks, err := s.store.DocumentChunks(ctx, documentID, from, count)
	if err != nil {
		return nil, nil, err
	}
	return doc, chunks, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
