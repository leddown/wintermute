package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"wintermute/internal/tool"
)

// Register adds web_search and fetch_url. Both are read-only: they retrieve,
// and nothing they return is written anywhere. Adding something to a library is
// a person's decision, made by uploading the document — see docs/agents.md.
func Register(reg *tool.Registry, client *Client) error {
	if client == nil {
		return nil
	}

	tools := []struct {
		def     tool.Definition
		handler tool.Handler
	}{
		{
			def: tool.Definition{
				Name: "web_search",
				Description: "Search the web and return titles, URLs and snippets. " +
					"Use it for anything current or external — a regulation's official text, a vendor " +
					"advisory, a standard's latest revision — rather than answering from memory, which " +
					"is stale. The snippets are not the source: fetch_url the page before quoting it.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"query": {"type": "string", "description": "What to search for."},
						"limit": {"type": "integer", "description": "How many results. Defaults to 6, maximum 15."}
					},
					"required": ["query"]
				}`),
				Risk: tool.RiskRead,
			},
			handler: searchHandler(client),
		},
		{
			def: tool.Definition{
				Name: "fetch_url",
				Description: "Fetch one web page and return its text. Use it after web_search to read " +
					"a result properly, or directly when given a URL. Addresses on the local network are " +
					"refused. Quote from what comes back, and say when a page is truncated.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"url": {"type": "string", "description": "An http or https URL."}
					},
					"required": ["url"]
				}`),
				Risk: tool.RiskRead,
			},
			handler: fetchHandler(client),
		},
	}

	for _, t := range tools {
		if err := reg.Register(t.def, t.handler); err != nil {
			return fmt.Errorf("web tool %q: %w", t.def.Name, err)
		}
	}
	return nil
}

func searchHandler(client *Client) tool.Handler {
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
		results, err := client.Search(ctx, in.Query, in.Limit)
		if err != nil {
			return "", err
		}
		if len(results) == 0 {
			return fmt.Sprintf("No results for %q. Try different words, and do not fill the gap "+
				"from memory.", in.Query), nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%d result(s) for %q:\n\n", len(results), in.Query)
		for i, r := range results {
			fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), r.URL)
			if snippet := strings.TrimSpace(r.Content); snippet != "" {
				fmt.Fprintf(&b, "   %s\n", truncate(snippet, 300))
			}
		}
		b.WriteString("\nThese are search snippets, not sources. Use fetch_url before quoting.")
		return b.String(), nil
	}
}

func fetchHandler(client *Client) tool.Handler {
	type input struct {
		URL string `json:"url"`
	}
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in input
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
		}
		page, err := client.Fetch(ctx, in.URL)
		if err != nil {
			return "", err
		}

		var b strings.Builder
		if page.Title != "" {
			fmt.Fprintf(&b, "%s\n", page.Title)
		}
		fmt.Fprintf(&b, "%s\n\n", page.URL)
		b.WriteString(page.Text)
		if page.Truncated {
			b.WriteString("\n\n[…page truncated; this is the first part only]")
		}
		return b.String(), nil
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, ' '); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}
