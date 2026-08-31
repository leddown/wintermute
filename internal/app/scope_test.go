package app

import (
	"context"
	"strings"
	"testing"

	"wintermute/internal/grc"
	"wintermute/internal/knowledge"
	"wintermute/internal/store/storetest"
	"wintermute/internal/tool"
	"wintermute/internal/websearch"
)

func newScope(t *testing.T, withGRC, withWeb bool) (*agentScope, *knowledge.Service) {
	t.Helper()
	st := storetest.New(t)

	svc := knowledge.NewService(knowledge.NewStore(st.DB()))
	scope := &agentScope{knowledge: svc}
	if withGRC {
		scope.grc = grc.New(grc.Config{BaseURL: "http://grc.invalid"})
	}
	if withWeb {
		scope.web = websearch.New(websearch.Config{SearxURL: "http://searx.invalid"})
	}
	return scope, svc
}

func toolNames(reg *tool.Registry) []string {
	defs := reg.Definitions()
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// The whole point of an agent is that it narrows what a turn can reach.
func TestScopeRegistersOnlyTheAgentsSources(t *testing.T) {
	ctx := context.Background()
	scope, svc := newScope(t, true, true)

	docsOnly, err := svc.CreateAgent(ctx, &knowledge.Agent{
		Name: "Acme", Sources: []string{knowledge.SourceDocuments}})
	if err != nil {
		t.Fatal(err)
	}
	everything, err := svc.CreateAgent(ctx, &knowledge.Agent{
		Name:    "GRC",
		Sources: []string{knowledge.SourceDocuments, knowledge.SourceGRC, knowledge.SourceWeb}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("documents only", func(t *testing.T) {
		reg := tool.NewRegistry()
		if _, err := scope.Scope(ctx, 1, docsOnly.ID, reg); err != nil {
			t.Fatalf("Scope: %v", err)
		}
		names := toolNames(reg)
		if !has(names, "search_documents") {
			t.Errorf("document tools missing: %v", names)
		}
		for _, forbidden := range []string{"grc_search", "web_search", "fetch_url"} {
			if has(names, forbidden) {
				t.Errorf("%s reached an agent that does not have that source: %v", forbidden, names)
			}
		}
	})

	t.Run("every source", func(t *testing.T) {
		reg := tool.NewRegistry()
		if _, err := scope.Scope(ctx, 1, everything.ID, reg); err != nil {
			t.Fatalf("Scope: %v", err)
		}
		names := toolNames(reg)
		for _, want := range []string{"search_documents", "grc_search", "grc_list_nfrs", "web_search", "fetch_url"} {
			if !has(names, want) {
				t.Errorf("%s missing: %v", want, names)
			}
		}
	})

	// No agent, and an agent that has since been deleted, are both the
	// unscoped assistant rather than an error or a free-for-all.
	for _, id := range []string{"", "deleted-agent"} {
		reg := tool.NewRegistry()
		prompt, err := scope.Scope(ctx, 1, id, reg)
		if err != nil {
			t.Fatalf("Scope(%q): %v", id, err)
		}
		if n := len(reg.Definitions()); n != 0 {
			t.Errorf("Scope(%q) registered %d tools", id, n)
		}
		if prompt != "" {
			t.Errorf("Scope(%q) returned a prompt", id)
		}
	}
}

// A source the agent declares that the server cannot back is a configuration
// mistake; the model is told so it can say so, rather than silently lacking a
// tool it was told it has.
func TestScopeSaysWhenASourceIsNotConfigured(t *testing.T) {
	ctx := context.Background()
	scope, svc := newScope(t, false, false)

	agent, err := svc.CreateAgent(ctx, &knowledge.Agent{
		Name: "GRC", Sources: []string{knowledge.SourceGRC, knowledge.SourceWeb}})
	if err != nil {
		t.Fatal(err)
	}

	reg := tool.NewRegistry()
	prompt, err := scope.Scope(ctx, 1, agent.ID, reg)
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	if n := len(reg.Definitions()); n != 0 {
		t.Errorf("registered %d tools for unconfigured sources", n)
	}
	if !strings.Contains(prompt, "the server has no such source set up") {
		t.Errorf("prompt does not report the missing sources:\n%s", prompt)
	}
	for _, want := range []string{"grc", "web"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not name %q:\n%s", want, prompt)
		}
	}
}

// The instruction that this whole feature exists to deliver.
func TestAgentPromptTellsTheModelToLookThingsUp(t *testing.T) {
	prompt := agentPrompt(
		&knowledge.Agent{
			Name:         "GRC",
			Description:  "Carelock compliance work",
			SystemPrompt: "Cite the NFR key.",
			Sources:      []string{knowledge.SourceDocuments, knowledge.SourceGRC},
		},
		map[string]bool{knowledge.SourceDocuments: true, knowledge.SourceGRC: true},
	)

	for _, want := range []string{
		`working as "GRC"`,
		"Carelock compliance work",
		"Cite the NFR key.",
		"search_documents",
		"grc_list_nfrs",
		"look it up before answering",
		"do not ask for a document to be pasted in",
		"say which count you are quoting",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestAgentPromptWithNoSources(t *testing.T) {
	prompt := agentPrompt(&knowledge.Agent{Name: "Plain"}, map[string]bool{})
	if !strings.Contains(prompt, "no knowledge sources available") {
		t.Errorf("prompt = %q", prompt)
	}
}
