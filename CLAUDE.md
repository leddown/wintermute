# CLAUDE.md

Guidance for Claude Code (and other agents) working in this repository.

## Project Overview

`wintermute` is a Go module (`go 1.25.12`) that wraps **Claude** so it can act
on a home network. It is two binaries plus a browser UI:

- **`cmd/wintermuted`** — the server. Runs on a Linux host on the network,
  calls Claude through the Anthropic Messages API, owns the conversation
  transcript, and executes server-side tools (metadata lookups). It also
  serves the embedded browser UI.
- **`cmd/wintermute`** — the desktop harness, built for Windows/macOS/Linux
  clients. It declares which actions the local machine can perform, and
  when the model asks for one it applies an approval policy, runs it
  locally, and reports the outcome back.

The defining constraint is that **the agent loop is split across two
processes**. Tools the server owns run in the server. Tools the client owns
are handed back as *pending calls*; the client decides whether to run them.
The server never touches a user's filesystem and never decides that an
action was approved. Preserve that split — it is the security model, not an
implementation detail.

The first feature built on this is media file renaming: the client lists a
NAS share, the server looks the titles up against TMDB/TVDB/OMDb, and the
model proposes renames that the user approves one by one.

State lives in SQLite (`modernc.org/sqlite`, pure Go — **no cgo**, which is
what lets the client cross-compile to a standalone Windows binary).

## Layout

```
cmd/wintermuted/    server entrypoint (flags, lifecycle)
cmd/wintermute/     client harness entrypoint (flags, REPL)
internal/app/       composition root — the only package importing all others
internal/agent/     the turn loop; partitions calls into server/client work
internal/api/       JSON HTTP handlers + auth middleware
internal/llm/       Provider interface + Anthropic Messages API implementation
internal/tool/      shared vocabulary: Definition, Call, Result, Registry
internal/lookup/    server-side tools: TMDB/TVDB/OMDb metadata lookup
internal/store/     SQLite: clients, sessions, messages, tool_audit
internal/client/    harness: config, transport, approval policy, prompting
internal/client/actions/   tools that run on the user's machine (fs, roots)
internal/config/    server config from env + .env
internal/web/       embedded browser UI (static assets)
```

`internal/tool` is the shared contract between server and client. Both sides
marshal `tool.Definition` over the wire, and the API decodes with
`DisallowUnknownFields` — so **changing a `tool` struct changes the wire
protocol**. `internal/api/api_test.go` has a round-trip test guarding this;
keep it passing.

## Build / Test / Run

```bash
go build ./...                  # compile everything
go test ./...                   # run all tests (no external services needed)
go vet ./...                    # static analysis
gofmt -l .                      # list files needing formatting

go run ./cmd/wintermuted        # run the server (config from env/.env)
go run ./cmd/wintermute         # run the local harness

# Cross-compile the Windows client (no cgo, so this just works):
GOOS=windows GOARCH=amd64 go build -o wintermute.exe ./cmd/wintermute
```

Run `gofmt -l .` and `go vet ./...` before considering any change complete.
Never leave the tree in a state that fails `go build ./...`.

Server configuration is environment-based (`ANTHROPIC_API_KEY`, `WINTERMUTE_*`,
plus `TMDB_API_KEY` / `TVDB_API_KEY` / `TVDB_PIN` / `OMDB_API_KEY`), loaded
from `.env` if present. `ANTHROPIC_API_KEY` is required;
`WINTERMUTE_LLM_MODEL` defaults to `claude-opus-5`. Client configuration
is a JSON file (`~/.config/wintermute/config.json`, `%AppData%` on Windows);
`wintermute -init` writes a starter.

Clients authenticate with a bearer token issued by
`wintermuted -add-client <name>`. Tokens are stored only as hashes and shown
once. There is no self-registration endpoint by design — don't add one
without explicit instruction.

Claude thinks by default, and the Messages API rejects a tool-use turn whose
assistant message dropped the thinking blocks that produced the call. The
transcript is replayed from SQLite on every iteration of the turn loop, so
`llm.Message.Thinking` is persisted verbatim and replayed unedited — don't
strip it, and don't disable thinking to avoid the problem (with thinking off,
the model sometimes writes a tool call into its visible text instead of
emitting a tool_use block, which looks like a turn that succeeded while the
rename silently never ran).

Migrations live in `internal/store/migrations/*.sql`, are embedded into the
binary, and are applied on every `store.Open` (so both the server and
`wintermuted -migrate-only` apply them). They must be idempotent. Never edit
a migration that has already been committed — add a new numbered file.

## Working Practices

- Make the smallest change that correctly solves the task. Don't refactor,
  rename, or "clean up" unrelated code in the same change.
- Don't add abstractions, interfaces, or config options for hypothetical
  future needs. Solve the problem in front of you.
- Match existing style and structure before introducing new patterns.
- Prefer editing existing files over creating new ones.
- Only create documentation files when explicitly asked.

## Task Planning & Progress Tracking

For any task with more than one step, build a task list before writing code
and keep it current as you go:

1. **Break the task down** into concrete, independently verifiable steps.
2. **Track status per step** as one of: `pending`, `in_progress`, `done`.
   Only one step should be `in_progress` at a time.
3. **Update status immediately** when a step finishes — don't batch updates
   at the end. If a step turns out to be wrong or unnecessary, mark it
   explicitly rather than silently dropping it.
4. **Surface blockers as new steps** instead of working around them
   silently.
5. **Re-plan when scope changes.** If mid-task work reveals the original
   breakdown was wrong, rewrite the task list rather than forcing new work
   into a stale plan.

When a task is genuinely complex (new tool, cross-cutting change, ambiguous
requirements), pause and propose a plan for the user to confirm before
implementing, rather than guessing at scope.

## Git

- Create new commits; don't amend existing ones unless explicitly asked.
- Write commit messages that explain *why*, not just *what*.
- Never commit compiled binaries, `.env` files, or `*.db` — see `.gitignore`.
- Don't push or force-push without explicit instruction.

## Security

These rules exist because this program renames files on a NAS on behalf of a
language model. Treat model output as untrusted input.

- **Every client action is confined to the configured roots.** Path
  validation lives in `internal/client/actions/roots.go`. Any new filesystem
  action must resolve its paths through `Roots`, and must do so *after*
  symlink resolution — never trust a path straight from tool input.
- **Risk levels drive approval.** A tool declares `RiskRead`, `RiskWrite`,
  or `RiskDestructive`. Reads may be auto-approved; writes prompt unless the
  user opted out; destructive actions *always* prompt, even under `-yes`.
  Pick the honest level for a new tool — under-declaring it silently removes
  a confirmation the user is relying on.
- **A refusal must still produce a tool result.** If the model isn't told
  an action was declined, it will report success it never achieved.
- **Everything is audited.** Every proposed call is written to `tool_audit`
  with its decision and outcome, whether or not it ran. The transcript is
  not the audit trail — it can be edited or discarded.
- Sessions are scoped to the authenticated client; always resolve a session
  through `store.Session(ctx, id, clientID)` rather than by ID alone.
- Never commit secrets or tokens. API keys and client tokens come from the
  environment or gitignored files.
- Be deliberate with anything that touches the filesystem or database
  destructively — confirm intent with the user for any operation that isn't
  trivially reversible.
