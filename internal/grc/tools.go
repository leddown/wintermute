package grc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"wintermute/internal/tool"
)

// Kinds the tools accept, mirroring the API's vocabulary.
const kindList = "nfr, control, regulation_clause, regulation, policy_clause, policy, risk"

// Register adds the GRC tools. Four of them, deliberately: a model uses a small
// vocabulary well and a large one badly, and everything here is a variation on
// "orient, list, search, fetch".
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
				Name: "grc_overview",
				Description: "Show what the GRC application holds: how many Security NFRs, 800-53 " +
					"controls, analysed regulation clauses, policies and risks, with the NFR domains " +
					"and control families and their counts. Call this first when a question is about " +
					"the catalogs and you do not yet know their shape.",
				Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
				Risk:       tool.RiskRead,
			},
			handler: overviewHandler(client),
		},
		{
			def: tool.Definition{
				Name: "grc_list_nfrs",
				Description: "List the Security NFR catalog in full — every requirement with its key, " +
					"summary, domain and NIST mapping. Use this for questions that need a count or a " +
					"complete answer (\"how many NFRs cover X\", \"which domains have no Y\"): the " +
					"catalog is small enough to read whole, and search can only tell you what matched " +
					"the words you chose. Also accepts control, regulation, policy or risk to list " +
					"those, but the control catalog is large — search it instead.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"kind": {"type": "string", "description": "Defaults to nfr. One of: nfr, control, regulation, policy, risk."}
					}
				}`),
				Risk: tool.RiskRead,
			},
			handler: indexHandler(client),
		},
		{
			def: tool.Definition{
				Name: "grc_search",
				Description: "Search one kind of GRC record by keyword. Returns the matching records " +
					"and two counts: how many matched any of your terms, and how many matched every " +
					"term. For a multi-word query the second is usually the honest number — say which " +
					"one you are quoting. This is keyword search, so it finds wording rather than " +
					"meaning; check each result's matched terms before counting it.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"kind": {"type": "string", "description": "One of: ` + kindList + `. Defaults to nfr."},
						"query": {"type": "string", "description": "Words to search for."},
						"limit": {"type": "integer", "description": "How many records to return. Defaults to 10, maximum 50."}
					},
					"required": ["query"]
				}`),
				Risk: tool.RiskRead,
			},
			handler: searchHandler(client),
		},
		{
			def: tool.Definition{
				Name: "grc_get",
				Description: "Fetch one GRC record in full by its reference — an NFR key, a control id " +
					"like AC-2, a regulation clause ref, a policy reference, a risk id. Use it after a " +
					"search to read a record properly before quoting it.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"kind": {"type": "string", "description": "One of: ` + kindList + `. Defaults to nfr."},
						"ref": {"type": "string", "description": "The record's reference."}
					},
					"required": ["ref"]
				}`),
				Risk: tool.RiskRead,
			},
			handler: getHandler(client),
		},
	}

	for _, t := range tools {
		if err := reg.Register(t.def, t.handler); err != nil {
			return fmt.Errorf("grc tool %q: %w", t.def.Name, err)
		}
	}
	return nil
}

func overviewHandler(client *Client) tool.Handler {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		overview, err := client.Overview(ctx)
		if err != nil {
			return "", err
		}

		var b strings.Builder
		b.WriteString("GRC application contents:\n")
		for _, kind := range []string{"nfr", "control", "regulation_clause", "regulation", "policy_clause", "policy", "risk"} {
			if n, ok := overview.Counts[kind]; ok {
				fmt.Fprintf(&b, "  %-18s %d\n", kind, n)
			}
		}
		if len(overview.NFRDomains) > 0 {
			b.WriteString("\nSecurity NFR domains:\n")
			for _, g := range overview.NFRDomains {
				fmt.Fprintf(&b, "  %-42s %d\n", g.Name, g.Count)
			}
		}
		if len(overview.ControlFamilies) > 0 {
			b.WriteString("\n800-53 control families (top): ")
			parts := make([]string, 0, 12)
			for i, g := range overview.ControlFamilies {
				if i >= 12 {
					break
				}
				parts = append(parts, fmt.Sprintf("%s=%d", g.Name, g.Count))
			}
			b.WriteString(strings.Join(parts, ", ") + "\n")
		}
		if len(overview.Regulations) > 0 {
			b.WriteString("\nRegulations analysed:\n")
			for _, r := range overview.Regulations {
				fmt.Fprintf(&b, "  #%s %s — %s\n", r.Ref, r.Title, r.Summary)
			}
		}
		if len(overview.RiskStatuses) > 0 {
			b.WriteString("\nRisks by status: ")
			parts := make([]string, 0, len(overview.RiskStatuses))
			for _, g := range overview.RiskStatuses {
				parts = append(parts, fmt.Sprintf("%s=%d", g.Name, g.Count))
			}
			b.WriteString(strings.Join(parts, ", ") + "\n")
		}
		if overview.Note != "" {
			b.WriteString("\n" + overview.Note)
		}
		return b.String(), nil
	}
}

func indexHandler(client *Client) tool.Handler {
	type input struct {
		Kind string `json:"kind"`
	}
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		in := input{Kind: "nfr"}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
		}
		if strings.TrimSpace(in.Kind) == "" {
			in.Kind = "nfr"
		}

		index, err := client.Index(ctx, in.Kind)
		if err != nil {
			return "", err
		}
		if index.Count == 0 {
			return fmt.Sprintf("The GRC application holds no %s records.", in.Kind), nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "All %d %s record(s):\n\n", index.Count, index.Kind)
		for _, item := range index.Items {
			fmt.Fprintf(&b, "%s | %s", item.Ref, oneLine(item.Title, 120))
			if item.Group != "" {
				fmt.Fprintf(&b, " | %s", item.Group)
			}
			if mapping := item.Fields["nist_mapping"]; mapping != "" {
				fmt.Fprintf(&b, " | NIST: %s", oneLine(mapping, 60))
			}
			b.WriteString("\n")
		}
		b.WriteString("\nThis is the complete list, so a count taken from it is exact. " +
			"Judge each entry on its wording rather than assuming a keyword decides it.")
		return b.String(), nil
	}
}

func searchHandler(client *Client) tool.Handler {
	type input struct {
		Kind  string `json:"kind"`
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		in := input{Kind: "nfr"}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
		}
		if strings.TrimSpace(in.Kind) == "" {
			in.Kind = "nfr"
		}
		if strings.TrimSpace(in.Query) == "" {
			return "", fmt.Errorf("query is required")
		}

		result, err := client.Search(ctx, in.Kind, in.Query, in.Limit)
		if err != nil {
			return "", err
		}
		if result.TotalMatches == 0 {
			return fmt.Sprintf("No %s record matches %q. Nothing in the catalog uses those words — "+
				"try others, or grc_list_nfrs to read the catalog. Do not answer from memory as though "+
				"the catalog had said it.", in.Kind, in.Query), nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s search for %q (terms: %s)\n", in.Kind, result.Query, strings.Join(result.Terms, ", "))
		fmt.Fprintf(&b, "%d record(s) matched at least one term; %d matched every term. Showing %d.\n\n",
			result.TotalMatches, result.TotalAllTerms, result.Returned)

		for _, hit := range result.Hits {
			item := hit.Item
			fmt.Fprintf(&b, "— %s | %s", item.Ref, oneLine(item.Title, 140))
			if item.Group != "" {
				fmt.Fprintf(&b, " | %s", item.Group)
			}
			fmt.Fprintf(&b, "\n  matched: %s\n", strings.Join(hit.Matched, ", "))
			if item.Summary != "" {
				fmt.Fprintf(&b, "  %s\n", oneLine(item.Summary, 300))
			}
			if len(item.Related) > 0 {
				fmt.Fprintf(&b, "  related: %s\n", strings.Join(item.Related, ", "))
			}
		}
		if result.Truncated {
			fmt.Fprintf(&b, "\n%d further match(es) not shown; raise limit to see them.",
				result.TotalMatches-result.Returned)
		}
		if result.Note != "" {
			b.WriteString("\n" + result.Note)
		}
		return b.String(), nil
	}
}

func getHandler(client *Client) tool.Handler {
	type input struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	}
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		in := input{Kind: "nfr"}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
		}
		if strings.TrimSpace(in.Kind) == "" {
			in.Kind = "nfr"
		}
		if strings.TrimSpace(in.Ref) == "" {
			return "", fmt.Errorf("ref is required")
		}

		item, err := client.Get(ctx, in.Kind, in.Ref)
		if err != nil {
			return "", err
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s %s — %s\n", item.Kind, item.Ref, item.Title)
		if item.Group != "" {
			fmt.Fprintf(&b, "group: %s\n", item.Group)
		}
		for _, key := range sortedKeys(item.Fields) {
			fmt.Fprintf(&b, "%s: %s\n", key, item.Fields[key])
		}
		if len(item.Related) > 0 {
			fmt.Fprintf(&b, "related: %s\n", strings.Join(item.Related, ", "))
		}
		if item.Body != "" {
			b.WriteString("\n" + item.Body + "\n")
		}
		if item.URL != "" {
			fmt.Fprintf(&b, "\nIn the application: %s", item.URL)
		}
		return b.String(), nil
	}
}

func sortedKeys(fields map[string]string) []string {
	out := make([]string, 0, len(fields))
	for key := range fields {
		out = append(out, key)
	}
	// A stable order so the same record reads the same way twice.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, ' '); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}
