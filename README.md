# wintermute

Wintermute is an AI assistant that can act on your home network — without ever
handing your filesystem to the machine that talks to the model, and without
requiring that the model be somebody else's.

It runs models you host yourself (llama.cpp, Ollama, vLLM) and can keep Claude
available alongside them as a deliberate, per-conversation choice.

It manages the models themselves: a repository of weights on a disk the server
owns, downloaded from Hugging Face, labelled, and handed out to the machines in
your fleet that should be holding them. Files on your own computers stay behind
the client harness, which proposes changes and never applies one you haven't
approved.

It also carries a small practice-management workspace — tasks, a CRM and your
company profile — reachable from the same browser UI and the same token. See
[Workspace](#workspace-tasks-crm-and-company).

- [Agents](docs/agents.md) — named sets of documents and sources, so a
  conversation reaches one client's material and not another's
- [Set up a local model server](docs/local-models.md) — GPU, drivers, llama.cpp
- [Running several backends](docs/backends.md) — multiple models at once, and
  when that actually makes anything faster

## How it works

Two splits define this program. Neither is an implementation detail — the first
is the security model, the second is the privacy model.

### 1. The agent loop is split across two processes

```
  your desktop                     your server                model backends
 ┌──────────────────────┐        ┌───────────────────────┐    ┌──────────────┐
 │ wintermute (harness) │───────▶│ wintermuted           │───▶│ local models │
 │                      │ msg +  │                       │◀───│  (your LAN)  │
 │ • declares what this │ tools  │ • owns the transcript │    └──────────────┘
 │   machine can do     │        │   (SQLite)            │    ┌──────────────┐
 │ • applies the        │◀───────│ • routes turns to a   │───▶│    Claude    │
 │   approval policy    │ reply +│   backend             │◀───│  (optional)  │
 │ • runs local actions │ pending│ • runs server-side    │    └──────────────┘
 │   inside your roots  │  calls │   tools (lookups)     │
 └──────────────────────┘        │ • serves the browser  │
                                 │   UI                  │
                                 └───────────────────────┘
```

- Tools the **server** owns (metadata lookup, hardware and model questions) run
  on the server.
- Tools the **client** owns (list a directory, stat a path, rename a file) are
  handed back as *pending calls*. The client decides whether to run them, and
  the server is told the outcome.
- The server never touches your filesystem and never decides that an action was
  approved. Only filenames and metadata leave your machine — file *contents*
  never do.

A turn therefore looks like this: you send a message, the server asks the model,
the model asks for a tool, and either the server runs it and loops again, or the
turn stops and returns pending calls to whoever asked. The client runs them,
posts the results back, and the loop resumes from the stored transcript. That is
why the transcript lives in SQLite and is replayed on every iteration — the loop
genuinely pauses across a network boundary, sometimes for as long as it takes a
person to answer a prompt.

Every proposed call is written to an audit table with its decision and outcome,
whether or not it ran. The transcript is not the audit trail; it can be edited
or discarded, and the audit table is append-only.

### 2. Turns are routed to a backend, and local is the normal path

`wintermuted` does not have "the model". It has a set of named **backends**, and
a router that decides which one serves a turn:

| | |
|---|---|
| **Backend** | A named model source: a URL, an API kind, a default model |
| **Default** | The backend used by a conversation that names none |
| **Per session** | A conversation may pin its own backend and model |
| **Fallback** | An optional backend retried when the selected one fails |

The design goal is that open-weight models on your LAN are the ordinary path and
a cloud model is reached *deliberately*. So:

- There is no implicit cloud. If you configure only local backends, a local
  backend that is down produces an error — it does not quietly send your
  transcript to a third party instead.
- A fallback fires only after the selected backend actually failed, never
  because it was slow, and never when *you* cancelled the turn.
- A fallback is never silent. Every turn reports which backend and model
  answered it, and if that isn't what was asked for, the reply carries
  `fell_back_from` and the reason.

One conversation is pinned to one backend, because moving it between machines
would throw away the served prompt cache and reprocess the whole transcript.
See [docs/backends.md](docs/backends.md).

The server also knows what it is running on. It probes each backend for the
models it serves, reads the host's GPU, VRAM, CPU and RAM, and can estimate
whether a given model and context length will fit. That is exposed both as HTTP
endpoints and as tools the model itself can call, so "will Gemma 3 12B fit on
this card?" is answered from measurements rather than from training data.

**What this does not do:** it does not manage inference. It never spawns
`llama-server`, downloads weights, or loads and unloads models. Those are
privileged local operations, and llama-swap or Ollama own them. Wintermute
observes, estimates, recommends and routes.

### Approval

Each tool declares a risk level, and that drives what happens on the client:

| Risk | Behaviour |
| --- | --- |
| `read` | auto-approved by default (`auto_approve_reads`) |
| `write` | prompts, unless you passed `-yes` |
| `destructive` | **always** prompts, even under `-yes` |

At a prompt you can answer `y` (yes), `n` (no), `a` (always allow this tool for
the rest of the run), or `q` (decline this and everything else in the turn).
A refusal still produces a tool result, so the model is told it was declined
rather than assuming success. This matters more with small local models than
with large ones: a model that is never told "no" will cheerfully report a change
it never made.

### Tools

**Server-side, model awareness** — `system_capabilities` (what hardware this
host has), `list_models` (what each backend is serving, with a fit verdict),
`estimate_model_fit` (will this model at this quant and context fit in VRAM),
`recommend_model` (rank what's available for a task), `search_models` (search
the Hugging Face Hub, results annotated with whether they'd run here).

**Server-side, tasks** — `list_todo_lists`, `create_todo_list`, `add_todo_task`:
read and build the task lists Workspace → Tasks shows. Short on purpose — creating
a list is reversible and touches nothing anyone audits.

**Client-side** — `list_directory` (read), `stat_path` (read),
`rename_file` (write; renames in place, never moves between directories). All
three are confined to your configured roots, checked *after* symlink resolution.

## Requirements

- Go 1.25.12+ to build. There are no cgo dependencies, so the client
  cross-compiles cleanly to a standalone Windows binary.
- **At least one model backend**, which is either:
  - a local OpenAI-compatible server — llama.cpp's `llama-server`, llama-swap,
    Ollama, vLLM or LM Studio. See [docs/local-models.md](docs/local-models.md)
    for a full build-and-tune guide, or
  - an **Anthropic API key** from
    [console.anthropic.com](https://console.anthropic.com), or both.
- Optionally, `nvidia-smi` on the server host, for GPU and VRAM reporting.
  Without it the hardware report simply omits the GPU.

## Build

```bash
go build -o wintermuted ./cmd/wintermuted   # server
go build -o wintermute  ./cmd/wintermute    # desktop harness

# Cross-compile the client for a Windows desktop:
GOOS=windows GOARCH=amd64 go build -o wintermute.exe ./cmd/wintermute
```

## Set up the server

### 1. Declare your backends

Backends are declared in `backends.json` in the working directory (override the
path with `WINTERMUTE_BACKENDS`). They live in a file rather than the
environment because there are several of them with several fields each, and
encoding that into environment variables produces something nobody can read.

```json
{
  "default": "local",
  "backends": [
    {
      "name": "local",
      "kind": "llamacpp",
      "base_url": "http://127.0.0.1:8080/v1",
      "api_key_env": "LLAMA_API_KEY",
      "model": "qwen3-8b"
    }
  ]
}
```

| Field | Meaning |
| --- | --- |
| `name` | How sessions and the UI refer to this backend. Must be unique |
| `kind` | `llamacpp`, `ollama`, `vllm`, `openai`, `hailo` or `anthropic` |
| `base_url` | API root. Required for everything except `anthropic` |
| `model` | Default model. May be empty if the backend serves exactly one |
| `api_key_env` | **Name of** the environment variable holding the key |

`api_key_env` names a variable rather than carrying the key itself, so this file
can be committed or shared without leaking a credential.

`kind` mostly selects how the backend is *probed*, not how it is called —
llama.cpp, Ollama, vLLM, LM Studio and hailo-ollama all speak the OpenAI API and
share one provider. `anthropic` is the exception and speaks the Messages API.

The top level takes three more keys:

- `"default"` — the backend used by conversations that name none. Defaults to
  the first declared backend.
- `"fallback"` — a backend retried when the selected one fails. **Leave it
  unset** unless you want failures to reach the cloud; unset means a failed
  local backend reports the failure and stops.

```json
{
  "default": "gpu",
  "backends": [
    { "name": "gpu", "kind": "llamacpp", "base_url": "http://192.168.1.10:8080/v1",
      "api_key_env": "LLAMA_API_KEY", "model": "qwen3-8b" },
    { "name": "nas", "kind": "ollama", "base_url": "http://192.168.1.11:11434/v1",
      "model": "gemma3:4b" }
  ]
}
```

[docs/backends.md](docs/backends.md) has the full picture.

To keep Claude available as a per-conversation alternative, add it as a second
backend:

```json
{
  "default": "local",
  "backends": [
    { "name": "local",  "kind": "llamacpp",  "base_url": "http://127.0.0.1:8080/v1",
      "api_key_env": "LLAMA_API_KEY", "model": "qwen3-8b" },
    { "name": "claude", "kind": "anthropic", "api_key_env": "ANTHROPIC_API_KEY",
      "model": "claude-opus-5" }
  ]
}
```

For **several backends at once** — a second GPU box, a CPU-only machine, a small
fast model beside a large slow one — see
[docs/backends.md](docs/backends.md), which also covers when that makes anything
faster and when it doesn't.

#### Or skip the file entirely

A single-backend setup needs no file. If `backends.json` is absent, one backend
is built from the environment:

```bash
# A local OpenAI-compatible server:
export WINTERMUTE_LLM_PROVIDER=openai        # or llamacpp / ollama / vllm
export WINTERMUTE_LLM_BASE_URL=http://127.0.0.1:8080/v1
export WINTERMUTE_LLM_MODEL=qwen3-8b
export WINTERMUTE_LLM_API_KEY=$(cat ~/.llama-api-key)

# Or Claude:
export ANTHROPIC_API_KEY=sk-ant-...
```

With no `WINTERMUTE_LLM_PROVIDER` set, the kind is inferred: a base URL means a
local OpenAI-compatible server, an Anthropic key alone means Claude. If neither
is present, the server refuses to start and tells you what to set.

### 2. Configure the rest

Everything else comes from the environment, loaded from a `.env` file in the
working directory if one is present (real environment variables win).

```bash
# .env
WINTERMUTE_ADDR=:8080
WINTERMUTE_DB=wintermute.db
LLAMA_API_KEY=...                         # referenced by api_key_env above
ANTHROPIC_API_KEY=sk-ant-...              # only if you declare an anthropic backend

# At least one of these, or the assistant can't verify titles:
```

| Variable | Default | Meaning |
| --- | --- | --- |
| `WINTERMUTE_BACKENDS` | `backends.json` | Path to the backend declaration |
| `WINTERMUTE_ADDR` | `:8080` | Listen address for the API and UI |
| `WINTERMUTE_DB` | `wintermute.db` | SQLite file |
| `WINTERMUTE_LLM_MAX_TOKENS` | `16000` | Cap on one response, thinking included |
| `WINTERMUTE_LLM_TIMEOUT` | `10m` | Bound on a single completion |
| `WINTERMUTE_MAX_TOOL_ITERATIONS` | `12` | Tool round-trips allowed per turn |
| `WINTERMUTE_BACKEND_PROBE_INTERVAL` | `1m` | How often backend health is re-checked; `0` probes only on demand |
| `HUGGINGFACE_TOKEN` | *(none)* | Only needed to search gated Hub repositories |
| `ANTHROPIC_API_KEY` | *(none)* | Required only by an `anthropic` backend |

The no-file fallback additionally reads `WINTERMUTE_LLM_PROVIDER`,
`WINTERMUTE_LLM_BASE_URL`, `WINTERMUTE_LLM_MODEL` and `WINTERMUTE_LLM_API_KEY`.

`WINTERMUTE_LLM_TIMEOUT` deserves a thought if you are running locally. Ten
minutes is generous for a cloud API and *not* generous for a 12B model doing a
long tool-using turn on a partially-offloaded card. Raise it before you conclude
a local backend is broken.

Migrations are embedded and applied on every start. `wintermuted -migrate-only`
applies them and exits.

### 3. Issue a client token

There is no self-registration endpoint, by design. Every client is created on
the server — but **which command depends on how the server runs**, because the
two find the database differently, and issuing a token into the wrong one is the
single most common way to end up locked out of your own UI.

**If the server runs as a systemd service** — what `scripts/setup.sh` installs,
and what you have if you did not deliberately choose otherwise — use
`scripts/clients.sh`:

```bash
sudo ./scripts/clients.sh list
sudo ./scripts/clients.sh add laptop browser   # kind defaults to harness
sudo ./scripts/clients.sh revoke laptop        # prompts first
```

It reads the database path out of the env file, refuses a relative path, refuses
to create a database that isn't already there, and runs `wintermuted` as the
service user so SQLite's `-wal`/`-shm` sidecars keep their ownership.

**If you run the server by hand**, the flags do the same jobs — but they read
`WINTERMUTE_DB` from *their own* environment, so set it to the same file the
server will open rather than relying on the fallback:

```bash
export WINTERMUTE_DB=/path/to/wintermute.db    # the same file the server opens
./wintermuted -add-client desktop              # a harness client (default)
./wintermuted -add-client browser -kind browser
./wintermuted -list-clients
./wintermuted -revoke-client desktop           # removes the client outright
```

> **Do not run these flags against a systemd install.** They fall back to a
> *relative* `wintermute.db`, while under systemd the database is named in
> `/etc/wintermute/wintermute.env` — an `EnvironmentFile`, which applies to the
> service and which an interactive shell never sees. So the flags create a
> second database in whatever directory you happened to be standing in and
> register the client in *that*. The token is genuine; the server simply never
> opens that file, so the UI answers `invalid token` and nothing on either side
> explains why. `sudo ./scripts/clients.sh list` prints the database it used —
> if the client you just made is missing from that listing, this is what
> happened.

The token is printed **once** and stored only as a hash. Copy it now.

`-revoke-client` (and `clients.sh revoke`) deletes the client row rather than
marking it dead, so the token stops working immediately and the name is free to
reuse. There is no undo and no way to recover the old token.

### 4. Run

```bash
./wintermuted            # add -debug for per-request logging
```

At startup every backend is probed once, with a 30-second budget, so the UI has
a catalog immediately. A backend that is down is recorded as unreachable and
retried on refresh — it never stops the server from starting, because the usual
reason a local inference server is unreachable is that nobody has started it
yet.

`GET /api/v1/health` is unauthenticated and reports liveness only. Everything
else needs `Authorization: Bearer <token>`.

### Or run it as a systemd service

`scripts/setup.sh` does steps 1–4 as a Linux service instead: it creates a
`wintermute` system user, writes `/etc/wintermute/wintermute.env` and
`backends.json` (detecting a local Ollama or llama.cpp server if one is already
running), installs both binaries, applies migrations, registers the first client
tokens, and installs `deploy/wintermuted.service`. It is safe to re-run —
anything that already exists is left alone.

Once it is a service, issue and revoke tokens with `scripts/clients.sh` rather
than the `wintermuted` flags directly — see the warning in step 3.

```bash
./scripts/setup.sh          # first time
sudo systemctl start wintermuted

./update.sh                 # after a code change: rebuild, migrate, restart
```

Two things differ from the manual instructions above. The service listens on
**`:8088`**, not `:8080`, because `:8080` is llama-server's port and on a host
that serves its own models the two collide. And the database lives at
`/var/lib/wintermute/wintermute.db`, which is the only writable path the unit has
under `ProtectSystem=strict`.

`update.sh` finishes with a report on what the running service can actually
reach: whether it answers, whether `WINTERMUTE_BACKENDS` is set (unset, a
carefully written `backends.json` is silently ignored), which backends are
reachable.

## Choosing a model per conversation

A session pins its own backend and model; empty means the server default. Create
one that way:

```bash
curl -X PATCH http://localhost:8080/api/v1/sessions/$ID/model \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"backend": "claude", "model": ""}'
```

Repointing an existing conversation keeps the transcript. That is deliberate —
escalating a stuck local turn to a stronger model, with everything that happened
so far intact, is one of the more useful things this setup can do.

Each turn's response reports what actually served it:

```json
{
  "reply": "...",
  "backend": "local",
  "model": "qwen3-8b",
  "fell_back_from": "",
  "fallback_reason": ""
}
```

## Server API

Beyond the conversation endpoints (`/api/v1/sessions`, `.../messages`,
`.../tool_results`, `.../audit`), the server exposes what it knows about the
models it can reach:

| Endpoint | Purpose |
| --- | --- |
| `GET /api/v1/me` | Who you are, plus the backend list, default and fallback |
| `GET /api/v1/system` | Host hardware: GPU, VRAM, NPUs, CPU, RAM |
| `GET /api/v1/backends` | Each backend with its last probe result |
| `POST /api/v1/backends/refresh` | Re-probe now |
| `GET /api/v1/models?context=8192` | Every model every backend serves, with a fit verdict |
| `GET /api/v1/models/search?q=qwen3` | Search the Hugging Face Hub |
| `GET /api/v1/models/detail/{author}/{name}` | One Hub model, sized against this host |
| `POST /api/v1/models/plan` | Recommend a model for a task |
| `POST /api/v1/models/fit` | Estimate VRAM for a hypothetical model |
| `GET /api/v1/tasks` | The planner's task classes |
| `PATCH /api/v1/sessions/{id}/model` | Repoint a conversation at another model |

The Hub search is proxied through the server rather than called from the browser
because the Hub token, if you configured one, must not reach the client — and
because the results are enriched with a fit verdict only the server can compute.

## Workspace: tasks, CRM, accounting and company

Beyond the model fleet, the server carries the practice-management modules
that moved here from an RCSA application: a task list, a CRM (clients,
engagements, billable time, a billing rollup) and the company profile — plus a
double-entry accounting module built on top of the CRM. They are views in the
browser UI and endpoints under `/api/v1/todo`, `/api/v1/crm`,
`/api/v1/accounting` and `/api/v1/company`, behind the same bearer token as
everything else.

| View | What it holds |
| --- | --- |
| **Tasks** | Lists, tasks, an agenda bucketed into overdue / today / next 14 days |
| **CRM** | Clients -> engagements -> billable time -> billing, with rates snapshotted at log time so a rate change never reprices invoiced work |
| **Accounts** | A general ledger, invoicing with EU VAT and gap-free numbering, payments, expenses and the statements — see [docs/accounting.md](docs/accounting.md) |
| **Company** | Your own legal name, address, registration numbers and contact details |
| **Admin** | What the server is actually running with: configuration, backends, hardware, tools and client tokens |

**These are single-user.** The application they came from scoped every row to a
signed-in user; wintermute has no user accounts, and the boundary is the client
token. The scoping columns are gone rather than stubbed — see
`internal/store/migrations/0004_workspace.sql`.

The task module also registers three tools on the agent — `list_todo_lists`,
`create_todo_list` and `add_todo_task` — so the chat can read and build lists.
That is what the old application's separate "Assistant" page did; it needed its
own model client, conversation store and tool registry to do it, and this
server already had all three.

Google Calendar sync did **not** come across with the tasks: it was an OAuth
integration with its own encrypted token store, and porting it is a separate
piece of work.

## Admin

The **Admin** view answers "why is the server behaving like this?" without an
ssh session, over `/api/v1/admin/*`:

| Tab | Shows |
| --- | --- |
| **Status** | Uptime, database path and size, write-ahead log size, row counts, registered tool count |
| **Configuration** | Listen address, database, backends file, model routing, token and iteration limits |
| **Backends** | Each backend's kind, model and probe status, with a re-probe button |
| **Hardware** | Host, CPU, memory and GPUs, as `GET /api/v1/system` reports them |
| **Tools** | Every server-side tool with the risk level it declares |
| **Clients** | Registered clients, when they were created and last seen, with revoke |

Two things it deliberately does not do.

**It never returns a secret.** API keys and the Hugging Face token are reported
as configured or not, never by value — this is a page that gets left open and
screenshotted. Client tokens could not be shown even if that were wanted, since
the store keeps only their hashes.

**It cannot issue a token.** `wintermuted -add-client` (or `scripts/clients.sh
add`) remains the only way to mint one, which keeps issuance on the machine. A
leaked browser token that could mint more would turn one stolen session into
permanent access. Revocation is exposed because it only ever removes access —
and revoking the client you are signed in as is refused with a `409`, since it
would lock you out mid-click with no way to explain why.

Configuration is read from the environment at startup and shown as a snapshot.
Changing any of it needs a restart, so the page presents it read-only rather
than offering fields it cannot honour.

## MCP server

`POST /mcp` speaks the [Model Context Protocol](https://modelcontextprotocol.io),
so something that is not the wintermute harness — Claude Code, Claude Desktop,
another agent — can call this server's tools directly. It implements
`initialize`, `ping`, `tools/list` and `tools/call` over JSON-RPC 2.0, one
message per POST, answered with one JSON body.

Authentication is the same bearer token as everything else, so an MCP client is
registered like any other:

```bash
./wintermuted -add-client claude-code -kind harness
```

```json
{
  "mcpServers": {
    "wintermute": {
      "type": "http",
      "url": "http://your-server:8080/mcp",
      "headers": { "Authorization": "Bearer wm_..." }
    }
  }
}
```

**Only server-side tools are offered** — the metadata lookup and the model
awareness tools. Client-side tools are absent by design, and this is the same
boundary the agent loop draws: a client-side tool is *defined* by the fact that
this server does not run it, and there is nowhere in MCP to hand a call back to
a third party whose approval policy decides. Advertising `rename_file` here
would promise an execution the server must never perform. An MCP client that
wants filesystem actions runs the wintermute harness instead, which owns its
own roots and its own approval prompts.

Risk levels come through as the annotations an MCP host expects: a read-only
tool carries `readOnlyHint`, a destructive one `destructiveHint`, so a host that
gates writes behind approval can do so without knowing anything about
wintermute's own vocabulary.

A tool that *fails* comes back as a successful call whose result has
`isError: true` — the caller is meant to read the failure and adapt. Only a
malformed or unroutable request is a JSON-RPC error.

## Set up the desktop client

```bash
./wintermute -init
```

That writes a starter config to `~/.config/wintermute/config.json`
(`%AppData%\wintermute\config.json` on Windows), mode `0600` because it holds a
token. Fill it in:

```json
{
  "server_url": "http://nas-host:8080",
  "token": "wm_…",
  "roots": ["/srv/files", "\\\\NAS\\share"],
  "auto_approve_reads": true,
  "always_allow": [],
  "never_allow": []
}
```

`roots` is the list of directories this machine will let the assistant touch.
Nothing outside them is reachable, whatever the model asks for. On Windows they
can be UNC paths (`\\NAS\share`) or mapped drives. `always_allow` and
`never_allow` name tools that skip the prompt or are refused outright.

`server_url`, `token` and `roots` can be overridden by `WINTERMUTE_SERVER`,
`WINTERMUTE_TOKEN` and `WINTERMUTE_ROOTS` (OS-separated list), so a token never
has to be written to disk on a shared machine.

Then run it:

```bash
./wintermute                                  # interactive REPL
./wintermute -prompt "tidy up /srv/files/inbox"  # one request, then exit
./wintermute -roots /srv/files/archive            # override roots for this run
./wintermute -config ./other.json
./wintermute -yes                             # auto-approve writes — see below
```

In the REPL, type a request and answer the approval prompts; `/exit` quits.

`-yes` auto-approves write-risk actions without asking. Destructive actions
still prompt. It's meant for unattended runs and it removes a confirmation you
would otherwise get on every rename — use it deliberately.

## Browser UI

The server serves an embedded UI at its listen address (`http://localhost:8080`
by default). Create a token with `-kind browser` — or `scripts/clients.sh add
<name> browser` on a service install — then open the page and paste it in; it's
kept in `localStorage` and mirrored into a cookie so plain navigations
authenticate too.

If the page rejects a token you just issued, the token is almost certainly in a
different database from the one the server reads; step 3 explains why. Confirm
with `sudo ./scripts/clients.sh list` — if the client isn't in that listing, the
server cannot see it either.

The kind is recorded for the UI and the audit trail and gates nothing, so a
`harness` token will log the browser in as well; use `browser` anyway so
`-list-clients` stays meaningful.

The browser has no local action set, so it can chat and use server-side lookups,
but it cannot list or rename anything on your disks. That takes the desktop
harness.

## A note on thinking

Reasoning models think before they answer, and the Anthropic Messages API
rejects a tool-use turn whose reasoning was dropped in between. Wintermute
therefore stores each turn's thinking blocks verbatim in the transcript and
replays them unedited. That is why `messages` has a `thinking` column — don't
strip it.

Don't disable thinking to avoid the problem either. With thinking off, models
sometimes write a tool call into their visible text instead of emitting a real
tool-use block, which looks like a turn that succeeded while the rename silently
never ran. The same failure has a second cause on local backends: `llama-server`
without `--jinja` doesn't use the model's own chat template, so the model never
sees the tool-call format it was trained on. If tool calls mysteriously stop
firing, check that flag first.

## Troubleshooting

**The server won't start: "no backends configured".** There is no
`backends.json` and no environment fallback. Set `WINTERMUTE_LLM_BASE_URL` for a
local server or `ANTHROPIC_API_KEY` for Claude, or write the file.

**A backend shows as unreachable.** The URL or key is wrong, or the inference
server isn't running. `POST /api/v1/backends/refresh` re-probes. Note that
base URLs for OpenAI-compatible servers include the `/v1` suffix.

**Turns fail with `backend not configured`.** A session is pinned to a backend
name that `backends.json` no longer declares. Repoint it with
`PATCH /api/v1/sessions/{id}/model`, or restore the name.

**The model describes calling a tool instead of calling it.** Local backend
missing `--jinja`, or a model that isn't reliably tool-capable. `list_models`
reports which models advertise the `tools` capability.

**Long local turns time out.** Raise `WINTERMUTE_LLM_TIMEOUT`.

**Everything is slow and adding a second backend didn't help.** Expected — see
[docs/backends.md](docs/backends.md). One conversation is a sequential loop, so
a second backend does not make a single turn faster. It gives you somewhere else
to run a *different* conversation.

## Development

```bash
go build ./...     # compile everything
go test ./...      # no external services needed
go vet ./...
gofmt -l .         # should print nothing
```

`internal/tool` is the shared contract between server and client — both sides
marshal `tool.Definition` over the wire, and the API decodes with
`DisallowUnknownFields`, so changing a `tool` struct changes the wire protocol.
`internal/api/api_test.go` guards the round-trip.

See [CLAUDE.md](CLAUDE.md) for the full layout and working practices.
