package app

import (
	"context"
	"fmt"
	"strings"

	"wintermute/internal/grc"
	"wintermute/internal/knowledge"
	"wintermute/internal/recall"
	"wintermute/internal/tool"
	"wintermute/internal/websearch"
)

// agentScope narrows a turn to the profile its session belongs to.
//
// It lives here rather than in internal/agent because it is the one place that
// already sees every module: the loop should not have to import the document
// library, the GRC client and the web client merely to pass them along. What
// the loop asks is "given this session, what may it reach"; this answers.
//
// Absence is the default. A session with no agent, or one whose agent was
// deleted, gets no knowledge tools at all — the assistant wintermute was before
// this existed — rather than everything.
type agentScope struct {
	knowledge *knowledge.Service
	grc       *grc.Client
	web       *websearch.Client
	// recall, when memory is configured, backs the episodic-memory tool: what
	// was actually done in past conversations, across every agent this client
	// owns.
	recall *recall.Store
}

// Scope registers the session agent's permitted tools and returns the prompt
// text to append.
func (s *agentScope) Scope(ctx context.Context, clientID int64, agentID string, registry *tool.Registry) (string, error) {
	if s == nil {
		return "", nil
	}

	// Episodic memory is offered to every session, scoped or not, because it
	// is a record of this client's own actions rather than a body of knowledge
	// an agent profile grants access to. A scoped session sees only its own
	// agent's activity; the unscoped assistant sees all of it, which is what
	// makes it the memory across every agent.
	if s.recall != nil {
		def, handler := recall.ActivityTool(s.recall, clientID, agentID)
		if err := registry.Register(def, handler); err != nil {
			return "", fmt.Errorf("activity tool: %w", err)
		}
	}

	if s.knowledge == nil {
		return "", nil
	}
	agent, err := s.knowledge.Lookup(ctx, agentID)
	if err != nil {
		return "", err
	}
	if agent == nil {
		return "", nil
	}

	if err := knowledge.RegisterFor(registry, s.knowledge, agent); err != nil {
		return "", err
	}
	if agent.Has(knowledge.SourceGRC) && s.grc != nil {
		if err := grc.Register(registry, s.grc); err != nil {
			return "", err
		}
	}
	if agent.Has(knowledge.SourceWeb) && s.web != nil {
		if err := websearch.Register(registry, s.web); err != nil {
			return "", err
		}
	}
	return agentPrompt(agent, s.available(agent)), nil
}

// ScopeWeb registers web_search and fetch_url alone.
//
// This is the Core chat's exception, and the whole of it: a conversation with
// no agent, no documents and no client actions, which the operator has decided
// may look something up. Nothing else is registered here — not the knowledge
// tools, not episodic memory — because the value of that mode is that what
// answers is the model, and every tool added is another thing answering for it.
//
// It reports false rather than an error when no web client is configured. A
// server with no SEARXNG_URL is not misconfigured, it is a server without web
// search, and the turn should proceed as the toolless conversation it is.
func (s *agentScope) ScopeWeb(registry *tool.Registry) (bool, string, error) {
	if s == nil || s.web == nil {
		return false, "", nil
	}
	if err := websearch.Register(registry, s.web); err != nil {
		return false, "", fmt.Errorf("web tools: %w", err)
	}
	return true, "", nil
}

// available lists the sources this agent declares that the server can actually
// serve. An agent asking for the web on a server with no search instance is a
// configuration mistake, and the model should be told rather than left to
// wonder why its tool is missing.
func (s *agentScope) available(agent *knowledge.Agent) map[string]bool {
	return map[string]bool{
		knowledge.SourceDocuments: agent.Has(knowledge.SourceDocuments),
		knowledge.SourceGRC:       agent.Has(knowledge.SourceGRC) && s.grc != nil,
		knowledge.SourceWeb:       agent.Has(knowledge.SourceWeb) && s.web != nil,
	}
}

// agentPrompt is what gets appended to the base system prompt.
//
// The instruction that matters most is the last one. The failure this whole
// feature exists to fix is a model asked "how many Security NFRs concern
// network segmentation?" answering with a description of how it would answer
// if someone pasted the catalog in. It has the catalog now; the prompt says to
// go and read it.
func agentPrompt(agent *knowledge.Agent, available map[string]bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are working as %q.", agent.Name)
	if agent.Description != "" {
		fmt.Fprintf(&b, " %s", strings.TrimSpace(agent.Description))
	}
	b.WriteString("\n\n")

	if agent.SystemPrompt != "" {
		b.WriteString(strings.TrimSpace(agent.SystemPrompt))
		b.WriteString("\n\n")
	}

	var sources []string
	if available[knowledge.SourceDocuments] {
		sources = append(sources, "- A document library uploaded to this agent. Use list_documents "+
			"and search_documents to consult it, and read_document to read further around a hit.")
	}
	if available[knowledge.SourceGRC] {
		sources = append(sources, "- A GRC application holding this organisation's Security NFR "+
			"catalog, its NIST SP 800-53 controls, analysed regulations, policies and risk register. "+
			"Use grc_overview, grc_list_nfrs, grc_search and grc_get.")
	}
	if available[knowledge.SourceWeb] {
		sources = append(sources, "- The web, through web_search and fetch_url.")
	}

	// Declared but unavailable is a configuration problem the operator needs to
	// see, and the model repeating it is how they find out. Worked out before
	// the no-sources branch below, because an agent whose every source is
	// unconfigured is the case where saying nothing would mislead most.
	var missing []string
	for _, source := range agent.Sources {
		if !available[source] {
			missing = append(missing, source)
		}
	}

	if len(sources) == 0 {
		b.WriteString("This agent has no knowledge sources available, so answer from the " +
			"conversation and say plainly when you do not have the material to answer.")
		if len(missing) > 0 {
			fmt.Fprintf(&b, "\n\nNote: it is configured to use %s, but the server has no such source "+
				"set up. Say so — this is a configuration problem the operator needs to hear about, "+
				"not a question you should answer around.", strings.Join(missing, " and "))
		}
		return b.String()
	}

	b.WriteString("Sources you can consult for this work:\n")
	b.WriteString(strings.Join(sources, "\n"))
	b.WriteString("\n\nUsing them:\n\n")
	b.WriteString("1. When a question is about this material, look it up before answering. " +
		"Do not describe what you would need in order to answer, and do not ask for a document " +
		"to be pasted in — you can read it. A question you could answer with a tool and did not " +
		"is a wrong answer.\n")
	b.WriteString("2. Quote and cite what you find: the NFR key, the control id, the document " +
		"and passage. A reader has to be able to check you.\n")
	b.WriteString("3. Counting questions deserve care. Read the whole catalog when it is small " +
		"enough to list, say which count you are quoting when a search reports several, and " +
		"distinguish records that genuinely address the subject from those that merely mention " +
		"one of its words. Show the borderline ones and say why they are borderline.\n")
	b.WriteString("4. When the sources do not cover something, say so. An honest gap is worth " +
		"more here than a plausible completion.")

	if len(missing) > 0 {
		fmt.Fprintf(&b, "\n\nNote: this agent is configured to use %s, but the server has no such "+
			"source set up, so those tools are unavailable. Say so if the question needs them.",
			strings.Join(missing, " and "))
	}
	return b.String()
}
