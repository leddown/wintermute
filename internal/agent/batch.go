package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wintermute/internal/llm"
	"wintermute/internal/store"
	"wintermute/internal/tool"
)

// Fanning a batch out is a throughput trick, not a change to who is allowed to
// do what. Three properties keep it that way, and the code below enforces each
// rather than trusting the prompt:
//
//   - A batch worker sees only server-side, read-only tools. It cannot be
//     handed a filesystem tool, so no amount of prompt injection in a filename
//     turns a worker into something that renames a file.
//   - Workers produce *proposals*. Every rename still goes back through the
//     main transcript, out to the client as a pending call, and past the user's
//     approval policy one at a time.
//   - Every tool call a worker makes is audited against the same session, so a
//     fanned-out batch is exactly as traceable as a serial one.

// maxBatchItems bounds one call. It is a guard against a model that has just
// listed a very large directory deciding to name all of it at once: the work is
// real, and the user should be asked again rather than have one tool call run
// for an hour.
const maxBatchItems = 200

// batchItemIterations is the tool-call budget for a single item. An item is
// meant to be "parse this name, confirm it with a lookup, answer" — a budget
// this small keeps a confused small model from spending the pool on one file.
const batchItemIterations = 4

// batchSystemPrompt frames a worker. It is deliberately not SystemPrompt: a
// worker has no conversation, no client tools and no user to talk to, and
// telling it otherwise invites it to write prose nobody reads.
const batchSystemPrompt = `You are naming one media file, as one item in a larger batch. Work only on the file you are given; you cannot see or affect any other file.

Use the lookup tools to confirm the canonical title, year, and — for episodes — the episode name and numbering. Do not rely on memory for titles or episode numbers.

Reply with a single JSON object and nothing else. Either:

  {"proposed_name": "New Name.ext", "reason": "one short line"}

or, to leave the file alone:

  {"skip": true, "reason": "one short line"}

Rules:
- Keep the original file extension exactly as it was.
- Propose a bare filename, never a path. You cannot move a file between directories.
- If a lookup returns nothing, skip the file. An unchanged filename is better than a guessed one.
- Do not claim the file has been renamed. You are proposing a name; a human approves it later.
- No prose, no code fences, no explanation outside the JSON object.`

// batchSchema is the model-facing input to the batch tool.
const batchSchema = `{
  "type": "object",
  "properties": {
    "directory": {
      "type": "string",
      "description": "The directory the files are in, for context. The batch cannot read it; pass the path you already listed."
    },
    "files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Bare filenames to propose names for, without directories. One item of work each."
    },
    "convention": {
      "type": "string",
      "description": "Optional naming convention to apply, e.g. \"Show - S01E01 - Title.ext\". Passed to every item."
    }
  },
  "required": ["files"]
}`

// batchInput is the decoded tool input.
type batchInput struct {
	Directory  string   `json:"directory"`
	Files      []string `json:"files"`
	Convention string   `json:"convention"`
}

// Proposal is one item's outcome, as the main model sees it.
type Proposal struct {
	File string `json:"file"`
	// Proposed is the suggested new filename, empty when skipped or failed.
	Proposed string `json:"proposed_name,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Skip     bool   `json:"skip,omitempty"`
	// Raw carries the worker's reply verbatim when it could not be read as
	// the requested JSON — small models get this wrong often enough that
	// discarding the answer would waste the work.
	Raw string `json:"raw_reply,omitempty"`
	// Error is set when the item failed outright on every backend tried.
	Error   string `json:"error,omitempty"`
	Backend string `json:"backend,omitempty"`
}

// BatchReport is the whole fan-out, returned to the model as the tool result.
type BatchReport struct {
	Directory string     `json:"directory,omitempty"`
	Proposals []Proposal `json:"proposals"`
	Summary   struct {
		Total    int            `json:"total"`
		Proposed int            `json:"proposed"`
		Skipped  int            `json:"skipped"`
		Failed   int            `json:"failed"`
		Backends map[string]int `json:"backends"`
		// OffNetwork counts items served by a cloud member of the pool. It is
		// reported because a batch sends a lot of filenames off the network at
		// once without anyone approving each one.
		OffNetwork int    `json:"off_network,omitempty"`
		Elapsed    string `json:"elapsed"`
	} `json:"summary"`
	Note string `json:"note"`
}

// batchNote is appended to every report. The whole risk of a batch tool is a
// model reading a list of proposals as a list of things it has done.
const batchNote = "These are proposals only. Nothing has been renamed. To act on any of them you must call the client-side rename tool for each file you want changed, and the user approves each one."

// batchDefinition describes the tool to the model. It is built per pool so the
// description can name the machines the work will actually go to.
func batchDefinition(pool *llm.Pool) tool.Definition {
	desc := fmt.Sprintf(
		"Propose new names for many files at once, in parallel across %d model backend(s) (%s). "+
			"Each file is handled independently by a worker that can look up metadata, and you get back one proposal per file. "+
			"Use this instead of naming files one at a time when you have more than a handful. "+
			"It proposes only: you must still call the rename tool for each file you want changed.",
		len(pool.Members()), strings.Join(pool.Members(), ", "))
	if pool.HasCloudMember() {
		desc += " Note: this pool includes a cloud backend, so filenames may leave the local network."
	}
	return tool.Definition{
		Name:        "batch_propose_names",
		Description: desc,
		Parameters:  json.RawMessage(batchSchema),
		Risk:        tool.RiskRead,
	}
}

// batchHandler binds the batch tool to one session, which is what lets each
// item's tool calls land in that session's audit trail.
func (a *Agent) batchHandler(sess *store.Session) tool.Handler {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var in batchInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if len(in.Files) == 0 {
			return "", fmt.Errorf("files is empty: there is nothing to propose")
		}
		if len(in.Files) > maxBatchItems {
			return "", fmt.Errorf("%d files exceeds the %d-item limit for one batch; split it",
				len(in.Files), maxBatchItems)
		}

		report := a.runBatch(ctx, sess, in)
		out, err := json.Marshal(report)
		if err != nil {
			return "", fmt.Errorf("encode report: %w", err)
		}
		return string(out), nil
	}
}

// runBatch fans the items out over the pool and collects the proposals.
func (a *Agent) runBatch(ctx context.Context, sess *store.Session, in batchInput) *BatchReport {
	started := time.Now()
	workerTools := a.batchTools()

	proposals := make([]Proposal, len(in.Files))
	jobs := make([]llm.Job, len(in.Files))
	for i, file := range in.Files {
		i, file := i, file
		proposals[i] = Proposal{File: file}
		jobs[i] = func(ctx context.Context, b *llm.Backend) error {
			p, err := a.runBatchItem(ctx, sess, workerTools, b, in, file)
			if err != nil {
				return err
			}
			// Only a successful item writes its proposal, so a retry on
			// another backend cannot leave half an answer behind.
			p.File = file
			p.Backend = b.Name
			proposals[i] = p
			return nil
		}
	}

	a.log.Info("batch started",
		"session", sess.ID, "items", len(jobs),
		"members", a.pool.Members(), "slots", a.pool.Slots())

	// Progress goes to the log because a turn is a single request/response:
	// there is nowhere to stream to until the tool result comes back, and a
	// long batch that says nothing for minutes looks like a hang.
	step := len(jobs) / 10
	if step < 1 {
		step = 1
	}
	results := a.pool.Run(ctx, jobs, llm.RunOptions{
		OnResult: func(done, total int, res llm.JobResult) {
			if res.Err != nil {
				a.log.Warn("batch item failed",
					"session", sess.ID, "file", in.Files[res.Index],
					"backend", res.Backend, "attempts", res.Attempts, "error", res.Err)
				return
			}
			if done%step == 0 || done == total {
				a.log.Info("batch progress", "session", sess.ID, "done", done, "total", total)
			}
		},
	})

	report := &BatchReport{Directory: in.Directory, Note: batchNote}
	report.Summary.Backends = map[string]int{}
	for _, res := range results {
		p := &proposals[res.Index]
		if res.Err != nil {
			p.Error = res.Err.Error()
			if res.Backend != "" {
				p.Backend = res.Backend
			}
		}
		if res.Backend != "" {
			report.Summary.Backends[res.Backend]++
		}
		if res.Cloud && res.Err == nil {
			report.Summary.OffNetwork++
		}
		switch {
		case p.Error != "":
			report.Summary.Failed++
		case p.Skip:
			report.Summary.Skipped++
		default:
			report.Summary.Proposed++
		}
	}
	report.Proposals = proposals
	report.Summary.Total = len(proposals)
	report.Summary.Elapsed = time.Since(started).Round(time.Millisecond).String()

	a.log.Info("batch finished",
		"session", sess.ID, "total", report.Summary.Total,
		"proposed", report.Summary.Proposed, "skipped", report.Summary.Skipped,
		"failed", report.Summary.Failed, "elapsed", report.Summary.Elapsed)

	return report
}

// runBatchItem runs one item's private loop against one backend.
//
// The transcript here is local to the item and is thrown away afterwards. That
// is the whole reason a batch parallelises: each item is a fresh short prompt
// sharing no prefix with any other, so there is no served prompt cache to lose
// by sending it to a different machine.
func (a *Agent) runBatchItem(
	ctx context.Context,
	sess *store.Session,
	tools *tool.Registry,
	b *llm.Backend,
	in batchInput,
	file string,
) (Proposal, error) {
	msgs := []llm.Message{llm.UserMessage(batchItemRequest(in, file))}
	defs := tools.Definitions()

	for i := 0; i < batchItemIterations; i++ {
		resp, err := b.Complete(ctx, llm.Request{
			System:   batchSystemPrompt,
			Messages: msgs,
			Tools:    defs,
		})
		if err != nil {
			return Proposal{}, err
		}
		msgs = append(msgs, resp.Message)

		if len(resp.Message.ToolCalls) == 0 {
			return parseProposal(resp.Message.Content), nil
		}

		for _, call := range resp.Message.ToolCalls {
			if _, ok := tools.Definition(call.Name); !ok {
				// A worker asking for a tool it was not given is the case the
				// filtering exists for. Refuse it by name and let the item
				// recover within its budget.
				msgs = append(msgs, llm.ToolMessage(tool.Errorf(call.ID,
					"unknown tool %q; a batch worker may only use the tools it was given", call.Name)))
				continue
			}
			// The audit ID is namespaced by file: without it, concurrent items
			// reusing a model's stock call id ("call_1") would be
			// indistinguishable in the audit trail.
			res := a.runServerTool(ctx, tools, sess, call, batchAuditID(file, call.ID))
			msgs = append(msgs, llm.ToolMessage(res))
		}
	}

	return Proposal{}, fmt.Errorf("item used its %d-iteration budget without answering", batchItemIterations)
}

// batchItemRequest is the per-item user message.
func batchItemRequest(in batchInput, file string) string {
	var b strings.Builder
	if in.Directory != "" {
		fmt.Fprintf(&b, "Directory: %s\n", in.Directory)
	}
	fmt.Fprintf(&b, "Filename: %s\n", file)
	if in.Convention != "" {
		fmt.Fprintf(&b, "Naming convention: %s\n", in.Convention)
	}
	b.WriteString("\nPropose a name for this one file.")
	return b.String()
}

func batchAuditID(file, callID string) string {
	return "batch/" + file + "/" + callID
}

// batchTools is the tool set a worker is given: server-side and read-only,
// nothing else.
//
// This is a filter over the server registry rather than a hand-written list, so
// a tool added later is included only if it is genuinely both — and the batch
// tool itself is absent because it is registered per session, not here, which
// is also what stops a worker starting a batch of its own.
func (a *Agent) batchTools() *tool.Registry {
	reg := tool.NewRegistry()
	for _, def := range a.serverTools.Definitions() {
		if def.Side != tool.SideServer || def.Risk != tool.RiskRead {
			continue
		}
		h, ok := a.serverTools.Handler(def.Name)
		if !ok {
			continue
		}
		if err := reg.Register(def, h); err != nil {
			a.log.Error("batch tool registration failed", "tool", def.Name, "error", err)
		}
	}
	return reg
}

// parseProposal reads a worker's reply.
//
// Small models wrap JSON in code fences and prose more often than not, so the
// object is extracted rather than strictly decoded, and an unreadable reply is
// kept verbatim instead of thrown away — the main model can usually still use
// it, and silently dropping a worker's answer would be worse than passing it
// on labelled.
func parseProposal(reply string) Proposal {
	text := strings.TrimSpace(reply)
	if text == "" {
		return Proposal{Error: "worker returned an empty reply"}
	}

	obj := extractJSONObject(text)
	if obj == "" {
		return Proposal{Raw: text}
	}

	var parsed struct {
		Proposed string `json:"proposed_name"`
		Reason   string `json:"reason"`
		Skip     bool   `json:"skip"`
	}
	if err := json.Unmarshal([]byte(obj), &parsed); err != nil {
		return Proposal{Raw: text}
	}
	if parsed.Proposed == "" && !parsed.Skip {
		return Proposal{Raw: text}
	}

	// A worker that returns a path has ignored its instructions; keep only the
	// final element rather than passing a path to something that renames in
	// place.
	name := parsed.Proposed
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return Proposal{Proposed: name, Reason: parsed.Reason, Skip: parsed.Skip}
}

// extractJSONObject returns the outermost {...} span in s, or "" if there is
// none that balances.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Braces inside a string are part of the value, not structure.
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
