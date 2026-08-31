# CLAUDE.md

Guidance for Claude Code (and other agents) working in this repository.

## Project Overview

`wintermute` is a Go module (`go 1.25.12`) that wraps **Claude** so it can act
on a home network. It is two binaries plus a browser UI:

- **`cmd/wintermuted`** — the server. Runs on a Linux host on the network,
  calls Claude through the Anthropic Messages API, owns the conversation
  transcript, and executes server-side tools. It also
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

The first feature built on this was media file renaming, against TMDB/TVDB/OMDb.
That has been removed. What the split now protects is narrower and unchanged in
kind: the client harness lists and renames files on the user's own machines, one
approval at a time, and the server never touches them.

State lives in SQLite (`modernc.org/sqlite`, pure Go — **no cgo**, which is
what lets the client cross-compile to a standalone Windows binary).

## Layout

```
cmd/wintermuted/    server entrypoint (flags, lifecycle)
cmd/wintermute/     client harness entrypoint (flags, REPL)
cmd/wintermute-node/ fleet agent: reports a remote Linux host's state
internal/app/       composition root — the only package importing all others
internal/agent/     the turn loop; partitions calls into server/client work
internal/api/       JSON HTTP handlers + auth middleware
internal/llm/       Provider interface + Anthropic Messages API implementation
internal/tool/      shared vocabulary: Definition, Call, Result, Registry
internal/store/     SQLite: clients, sessions, messages, muninn (audit)
internal/recall/    memory: embedding index, hybrid retrieval, prior-context block
internal/hostmetrics/ /proc readers, shared by the server and the node agent
internal/node/      fleet: wire types and the separate telemetry database
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

A test that needs a database calls `storetest.New(t)` (or `NewAt` when it also
needs the file's path), never `store.Open`. `store.Open` migrates an empty
database in twenty-two commits and fsyncs every one of them, which is about
three hundred milliseconds a test on a fast disk and much worse on the host
that runs the deploy gate. `storetest` builds the schema once per test binary,
hands out copies of the file, and opens them with fsync off — the databases
live in a temp directory and are deleted when the test ends, so there is
nothing for them to be durable for. Tests that are *about* migrating, like
`TestMigrationsAreIdempotent`, still call `store.Open` and should.

Server configuration is environment-based (`ANTHROPIC_API_KEY`, `WINTERMUTE_*`),
loaded from `.env` if present. `ANTHROPIC_API_KEY` is required;
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
action silently never ran).

## The machines

Two hosts, and confusing them wastes an afternoon: they have different jobs,
different service users and different things installed.

- **`wintermute`** (10.232.231.9) — **the server.** Runs `wintermuted` as user
  `wintermute`, behind nginx on port 80. It owns `WINTERMUTE_MODEL_REPO`, the
  Samba export of that directory, the conversion toolchain
  (`WINTERMUTE_CONVERT_CMD` → llama.cpp's `convert_hf_to_gguf.py`), and the
  browser UI. Anything about downloading, converting or serving the repository
  happens here.
- **`coven`** (10.232.231.8) — **a rig node and the development machine.** This
  repository is checked out here and this is where builds, `go test`, headless
  Chrome and throwaway servers run. It also runs `wintermute-node` as user
  `wintermute-node`, reporting to `http://wintermute.l3d.internal`: Ollama on
  127.0.0.1:11434, model store `/mnt/stor/Models`, repository mount
  `/mnt/wintermute_model_repo` (read-only, over Samba from the server).

So a change is *built and tested* on coven and *deployed* to wintermute, and a
tool the server needs — the converter, the model repository, the drive with room
on it — is installed on wintermute even though the work of writing the code
happened on coven. Coven also carries a `wintermuted` service of its own; it is
not the fleet's server, so don't reason about the live installation from it.

## Testing against running servers

Testing against the live server on this network is **explicitly allowed and
encouraged**. It is the user's own hardware, and a bug in a browser UI or a
turn loop is often not reachable from `go test` at all — several faults in this
repository were found only by driving the real thing and would have been
guessed at wrongly otherwise.

What that permits, without asking again each time:

- **HTTP to the fleet and the server** — `wintermute.l3d.internal` (port 80,
  nginx in front of wintermuted), the nodes, and any backend on the LAN.
  Reading state, posting turns, creating throwaway sessions and lists.
- **Throwaway servers** built from the working tree, on `127.0.0.1`. Use a
  scratch `WINTERMUTE_DB` and `WINTERMUTE_METRICS_DB`; never point one at
  `/var/lib/wintermute/wintermute.db`.
- **A real browser.** `google-chrome --headless=new` against a local server is
  how the UI actually gets checked; `node --check` only proves it parses.
  Driving it needs a page on the same origin that can seed
  `localStorage.wintermute_token`, so a small proxy in the scratchpad that
  injects a script into `index.html` is the established trick.
- **Stand-in model endpoints.** Point `ANTHROPIC_BASE_URL` at a local stub that
  records what it was sent, when the question is what the model was handed
  rather than what it replied. That is how "does a toolless session really get
  no tools" was answered.

Credentials: ask. The user will issue a client token
(`wintermuted -add-client <name>`) for the live server when one is needed.
Tokens are shown once, belong in the scratchpad rather than the repo, and
should be treated as spent once they have appeared in a transcript — say so and
suggest rotating (`sudo scripts/clients.sh revoke <name>`).

Still off limits without asking first: anything that writes to the live
server's own database or filesystem beyond ordinary API use — deleting
conversations, `-reindex-memory`, clearing the model repository, restarting the
service, or `update.sh`. Deploying is the user's call, not a test step.

## Memory

Every conversation is recorded in `messages` as neutral role/content text and
is never stored as a rendered prompt with a model's chat template applied.
That is what lets the history outlive the model that produced it, and it is
the constraint to protect above all others here — templates are applied at
call time, inside the provider, and nowhere else. Each row also records which
backend and model it passed through, because a session can be repointed at
another model mid-conversation.

`internal/recall` is the layer above: it embeds messages, retrieves relevant
prior exchanges for a new turn, and renders them as a delimited block. It is
never coupled to a chat model.

- **The embedder is pinned and separately configured.** `WINTERMUTE_EMBED_*`
  is not one of the chat backends. Its name and vector width are written to
  `recall_meta` on first index and compared at every startup; a mismatch
  refuses to start rather than retrieving against another model's vector
  space, which fails silently. Changing it is a deliberate
  `wintermuted -reindex-memory`.
- **The index is derived.** Vectors are float32 BLOBs and the lexical half is
  a contentless FTS5 table; both are rebuildable from `messages`, which is the
  source of truth. Do not make the index authoritative for anything.
- **Retrieved context goes in front of the user's message, not in the system
  prompt.** The system prompt is the cached prefix — Anthropic's cache
  hierarchy and a local backend's KV cache both key on it — and memory changes
  every turn. Only the static framing belongs there.
- **Only user and assistant turns are indexed.** Tool results and fetched web
  pages are excluded on purpose: indexing them would let text this server did
  not author be injected into later conversations as trusted context, and a
  poisoned memory is retrieved repeatedly. Treat retrieved context as
  untrusted input.
- **Recall is scoped.** `client_id` is a hard boundary. A session scoped to an
  agent recalls that agent's history alone; the unscoped assistant recalls
  everything the client owns. The asymmetry is deliberate — do not collapse it
  into `agent_id = ? OR agent_id = ''`, which would let one agent read
  another's material through Wintermute's own conversations.

A master switch in `recall_config` sits above everything: when it is off, no
conversation is given prior context whatever its own setting says. Indexing
carries on regardless, so turning it back on does not leave a gap. It is a row
rather than an env var so it can be flipped without a restart, and it is
reachable from Admin → Memory along with two ways to throw things away:
clearing the index (reversible — `-backfill-memory` rebuilds it) and deleting
every conversation (not reversible, and gated on a typed confirmation checked
server-side).

Two independent switches live on each session. `record` decides whether the
conversation is written down; `recall` decides whether prior context is
retrieved into it. Both default to on and ephemeral is never inferred. An
off-the-record conversation writes **no rows at all** — its transcript lives
in memory in `internal/agent`, and turning recording off mid-conversation
deletes what was already written in the same transaction. Muninn keeps
recording throughout: it holds what was *done*, not what was said.

## The fleet

`cmd/wintermute-node` runs on remote Linux hosts and reports what they are
doing. Three rules hold it in shape:

- **The host pushes; the server never scrapes.** Hosts sit behind NAT and get
  addresses from DHCP, and this keeps the property that the server never
  reaches into a machine uninvited.
- **The agent only reports.** It cannot be told to run anything. A fleet of
  agents that execute commands is a fleet of remote shells; loading and
  unloading models is done through the backend's own API instead, which needs
  no host access at all (`internal/models/control.go`).
- **A node is identified by the client it authenticates as**, never by a name
  in the request body — otherwise any node could write samples attributed to
  another. Register with `wintermuted -add-client <name> -kind node`.

Telemetry lives in its **own database file** (`WINTERMUTE_METRICS_DB`), not
beside the conversation memory. It arrives constantly, is worth little within
days, and would inflate every memory snapshot for data already past its
usefulness.

Raw samples live **two hours** and are then folded into minute, hour and day
buckets. The rule that follows is enforced in code, not by discipline:
`node.bucketFor` picks the tier from the requested span, so a month-long chart
cannot reach a raw row even by accident. Two invariants keep it true:

- **Tiers store sums, counts and maxima — never averages.** That is what lets
  hours be built from minutes and days from hours. An average has forgotten how
  many readings it stood for and cannot be re-aggregated without lying. Means
  are recovered at query time by dividing.
- **Raw is never deleted past the minute tier's watermark**, whatever the
  retention says. If folding stalls, raw accumulates until it recovers rather
  than being destroyed unsummarised — the one failure here that loses data
  permanently. There is a test for that ordering; keep it.

Timestamps in the metrics database are stored as explicit RFC3339 **text**. The
driver stores a `time.Time` in a form SQLite's own date functions cannot read,
which makes `strftime` return NULL and the whole fold silently produce nothing.

## Backups

The memory store cannot be rebuilt from anything else, so it has two
independent protections, both in `internal/utilities`:

- `Backup` takes a `VACUUM INTO` snapshot, then **reopens and verifies it**
  before reporting success, writing a `manifest.json` with checksums and row
  counts. A snapshot that fails verification is deleted rather than left
  looking like a backup. `WINTERMUTE_BACKUP_*` schedules it.
- `ExportMemory` writes a portable JSON Lines archive for carrying history
  into a rebuilt installation. Import is idempotent and verifies checksums
  before writing a row. Credentials are never exported.

Never make either path depend on a model or an embedder being reachable.

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

### Commented configuration files

The files that are mostly prose — `deploy/*.env.example`, the systemd units,
anything whose job is to explain a setting rather than to hold one — follow a
fixed shape, so a long file stays scannable:

- **The setting comes first, its explanation below it**, separated by one
  blank line. The name is what an operator is looking for; the paragraph is
  what they read once they have found it.
- **Two blank lines between the end of a comment block and the next setting.**
  One blank line is what separates a setting from its own comment, so the
  wider gap is what makes the boundary between entries visible.

```
WINTERMUTE_NODE_STORE=

# Where this host keeps its own copy of the weights it has been assigned, so
# switching a model is a local file read rather than a download.


WINTERMUTE_NODE_RUNTIME=

# What actually serves models on this host: ollama, llamacpp, or empty.
```

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

These rules exist because this program acts on a home network on behalf of a
language model: it renames files on the user's machines through the client
harness, and it downloads gigabytes onto disks the server owns. Treat model
output as untrusted input, and treat a filename from off the machine — a
Hugging Face repository, a fleet assignment — the same way.

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
- **Everything is audited.** Every proposed call is written to `muninn`
  with its decision and outcome, whether or not it ran. The transcript is
  not the audit trail — it can be edited or discarded.
- Sessions are scoped to the authenticated client; always resolve a session
  through `store.Session(ctx, id, clientID)` rather than by ID alone.
- Never commit secrets or tokens. API keys and client tokens come from the
  environment or gitignored files.
- Be deliberate with anything that touches the filesystem or database
  destructively — confirm intent with the user for any operation that isn't
  trivially reversible.
