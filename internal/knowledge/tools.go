package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"wintermute/internal/tool"
)

// Retrieval bounds. A model reading search results needs enough of each to
// judge it and a pointer to the rest, not the document.
const (
	defaultSearchLimit = 6
	maxSearchLimit     = 15
	snippetChars       = 700
	maxReadChunks      = 6
)

// RegisterFor adds the document tools for one agent's library.
//
// Unlike the other tool modules, these are registered per session rather than
// at startup, because the library they read is the session's agent's. Binding
// the agent id into the handler here — rather than passing it as a tool
// argument — is what makes it impossible for a model to read another agent's
// documents by naming them: the argument does not exist.
func RegisterFor(reg *tool.Registry, svc *Service, agent *Agent) error {
	if agent == nil || !agent.Has(SourceDocuments) {
		return nil
	}

	tools := []struct {
		def     tool.Definition
		handler tool.Handler
	}{
		{
			def: tool.Definition{
				Name: "list_documents",
				Description: "List the documents in this agent's library, with their ids and sizes. " +
					"Call this when asked what you have, or before searching, to know what the library covers.",
				Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
				Risk:       tool.RiskRead,
			},
			handler: listDocumentsHandler(svc, agent.ID),
		},
		{
			def: tool.Definition{
				Name: "search_documents",
				Description: "Search this agent's document library and return the passages that match, " +
					"each with its document, heading and position. This is keyword search: it finds the " +
					"wording you ask for, so try the words the document would use. Quote from what it " +
					"returns rather than from memory, and if it returns nothing, say so.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"query": {"type": "string", "description": "Words to search for, e.g. 'incident reporting deadline'."},
						"limit": {"type": "integer", "description": "How many passages to return. Defaults to 6, maximum 15."}
					},
					"required": ["query"]
				}`),
				Risk: tool.RiskRead,
			},
			handler: searchDocumentsHandler(svc, agent.ID),
		},
		{
			def: tool.Definition{
				Name: "read_document",
				Description: "Read consecutive passages of one document from this agent's library, " +
					"by document id and starting position. Use it to read around a search hit when the " +
					"passage alone does not settle the question.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"document_id": {"type": "integer", "description": "From list_documents or search_documents."},
						"from": {"type": "integer", "description": "Passage number to start at. Defaults to 0, the start."},
						"count": {"type": "integer", "description": "How many consecutive passages. Defaults to 3, maximum 6."}
					},
					"required": ["document_id"]
				}`),
				Risk: tool.RiskRead,
			},
			handler: readDocumentHandler(svc, agent.ID),
		},
	}

	for _, t := range tools {
		if err := reg.Register(t.def, t.handler); err != nil {
			return fmt.Errorf("document tool %q: %w", t.def.Name, err)
		}
	}
	return nil
}

func listDocumentsHandler(svc *Service, agentID string) tool.Handler {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		docs, err := svc.Documents(ctx, agentID)
		if err != nil {
			return "", err
		}
		if len(docs) == 0 {
			return "This agent's library is empty. Nothing has been uploaded to it yet — say so rather " +
				"than answering from memory.", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d document(s) in this library:\n", len(docs))
		for _, d := range docs {
			fmt.Fprintf(&b, "#%d %s — %d passage(s), %s", d.ID, d.Title, d.ChunkCount, HumanBytes(d.ByteSize))
			if d.Filename != "" && d.Filename != d.Title {
				fmt.Fprintf(&b, ", from %s", d.Filename)
			}
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}

func searchDocumentsHandler(svc *Service, agentID string) tool.Handler {
	type input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in input
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
		}
		if strings.TrimSpace(in.Query) == "" {
			return "", fmt.Errorf("query is required")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = defaultSearchLimit
		}
		if limit > maxSearchLimit {
			limit = maxSearchLimit
		}

		hits, err := svc.SearchLibrary(ctx, agentID, in.Query, limit)
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return fmt.Sprintf("No passage in this library matches %q. The library may not cover it, "+
				"or the document may word it differently — try other terms, or list_documents to see "+
				"what is here. Do not answer from memory as though the library had said it.", in.Query), nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%d passage(s) matching %q:\n\n", len(hits), in.Query)
		for _, hit := range hits {
			fmt.Fprintf(&b, "— document #%d %q, passage %d",
				hit.Chunk.DocumentID, hit.Chunk.Title, hit.Chunk.Ordinal)
			if hit.Chunk.Heading != "" {
				fmt.Fprintf(&b, ", under %q", hit.Chunk.Heading)
			}
			fmt.Fprintf(&b, " (matched: %s)\n", strings.Join(hit.Terms, ", "))
			b.WriteString(truncate(hit.Chunk.Body, snippetChars))
			b.WriteString("\n\n")
		}
		b.WriteString("Use read_document with a document id and passage number to read further.")
		return b.String(), nil
	}
}

func readDocumentHandler(svc *Service, agentID string) tool.Handler {
	type input struct {
		DocumentID int64 `json:"document_id"`
		From       int   `json:"from"`
		Count      int   `json:"count"`
	}
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in input
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
		}
		if in.DocumentID <= 0 {
			return "", fmt.Errorf("document_id is required")
		}
		count := in.Count
		if count <= 0 {
			count = 3
		}
		if count > maxReadChunks {
			count = maxReadChunks
		}

		doc, chunks, err := svc.ReadDocument(ctx, agentID, in.DocumentID, in.From, count)
		if err != nil {
			return "", err
		}
		if len(chunks) == 0 {
			return fmt.Sprintf("%q has no passage at position %d; it has %d in total.",
				doc.Title, in.From, doc.ChunkCount), nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s — passages %d to %d of %d\n\n",
			doc.Title, chunks[0].Ordinal, chunks[len(chunks)-1].Ordinal, doc.ChunkCount)
		for _, chunk := range chunks {
			if chunk.Heading != "" {
				fmt.Fprintf(&b, "## %s\n", chunk.Heading)
			}
			b.WriteString(chunk.Body)
			b.WriteString("\n\n")
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexAny(cut, " \n"); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + " […]"
}
