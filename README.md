# wintermute

Wintermute wraps **Claude** so it can act on your home network — without ever
handing your filesystem to the server that talks to the model.

The first thing it's built to do is rename media files: your desktop lists a
NAS share, the server looks the titles up against TMDB/TVDB/OMDb, and the model
proposes renames that you approve one at a time.

## How it works

The agent loop is **split across two processes**, and that split is the whole
security model:

```
  your desktop                     your server                  Anthropic
 ┌──────────────────────┐        ┌───────────────────────┐    ┌──────────────┐
 │ wintermute (harness) │───────▶│ wintermuted           │───▶│    Claude    │
 │                      │ msg +  │                       │    │   Messages   │
 │ • declares what this │ tools  │ • owns the transcript │◀───│      API     │
 │   machine can do     │        │   (SQLite)            │    └──────────────┘
 │ • applies the        │◀───────│ • runs server-side    │
 │   approval policy    │ reply +│   tools (lookups)     │
 │ • runs local actions │ pending│ • serves the browser  │
 │   inside your roots  │  calls │   UI                  │
 └──────────────────────┘        └───────────────────────┘
```

- Tools the **server** owns (metadata lookup) run on the server.
- Tools the **client** owns (list a directory, stat a path, rename a file) are
  handed back as *pending calls*. The client decides whether to run them.
- The server never touches your filesystem and never decides that an action was
  approved. Only filenames and metadata leave your machine — file *contents*
  never do. Those filenames and directory listings do go on to Anthropic as
  part of the conversation, which is what lets Claude reason about them.

Every proposed call is written to an audit table with its decision and outcome,
whether or not it ran.

### Approval

Each tool declares a risk level, and that drives what happens:

| Risk | Behaviour |
| --- | --- |
| `read` | auto-approved by default (`auto_approve_reads`) |
| `write` | prompts, unless you passed `-yes` |
| `destructive` | **always** prompts, even under `-yes` |

At a prompt you can answer `y` (yes), `n` (no), `a` (always allow this tool for
the rest of the run), or `q` (decline this and everything else in the turn).
A refusal still produces a tool result, so the model is told it was declined
rather than assuming success.

### Tools

**Server-side** — `lookup_metadata`: resolve a movie, series or episode against
whichever of TMDB / TVDB / OMDb you configured, to confirm the canonical title,
year and episode name.

**Client-side** — `list_directory` (read), `stat_path` (read),
`rename_file` (write; renames in place, never moves between directories). All
three are confined to your configured roots, checked *after* symlink resolution.

## Requirements

- Go 1.25.12+ to build. There are no cgo dependencies, so the client
  cross-compiles cleanly to a standalone Windows binary.
- An **Anthropic API key** ([console.anthropic.com](https://console.anthropic.com)).
  The server calls the Messages API; the model runs on Anthropic's
  infrastructure, not yours.
- Optionally, API keys for TMDB, TVDB and/or OMDb. Without at least one, the
  lookup tool isn't registered at all and the assistant can't verify titles.

## Build

```bash
go build -o wintermuted ./cmd/wintermuted   # server
go build -o wintermute  ./cmd/wintermute    # desktop harness

# Cross-compile the client for a Windows desktop:
GOOS=windows GOARCH=amd64 go build -o wintermute.exe ./cmd/wintermute
```

## Set up the server

### 1. Configure

Configuration comes from the environment, loaded from a `.env` file in the
working directory if one is present (real environment variables win). Only
`ANTHROPIC_API_KEY` is required.

```bash
# .env
ANTHROPIC_API_KEY=sk-ant-...              # required
WINTERMUTE_LLM_MODEL=claude-opus-5        # optional — this is the default
WINTERMUTE_ADDR=:8080
WINTERMUTE_DB=wintermute.db

# At least one of these, or the assistant can't verify titles:
TMDB_API_KEY=...
TVDB_API_KEY=...
TVDB_PIN=...                              # only for user-supported TVDB keys
OMDB_API_KEY=...
```

| Variable | Default | Meaning |
| --- | --- | --- |
| `ANTHROPIC_API_KEY` | *(required)* | Your Anthropic API key |
| `ANTHROPIC_BASE_URL` | *(SDK default)* | Override the API root, for a proxy |
| `WINTERMUTE_LLM_MODEL` | `claude-opus-5` | Claude model to use |
| `WINTERMUTE_LLM_MAX_TOKENS` | `16000` | Cap on one response, thinking included |
| `WINTERMUTE_LLM_TIMEOUT` | `10m` | Bound on a single completion |
| `WINTERMUTE_ADDR` | `:8080` | Listen address for the API and UI |
| `WINTERMUTE_DB` | `wintermute.db` | SQLite file |
| `WINTERMUTE_MAX_TOOL_ITERATIONS` | `12` | Tool round-trips allowed per turn |

Migrations are embedded and applied on every start. `wintermuted -migrate-only`
applies them and exits.

### 2. Issue a client token

There is no self-registration endpoint, by design. Every client is created on
the server:

```bash
./wintermuted -add-client desktop              # a harness client (default)
./wintermuted -add-client browser -kind browser
./wintermuted -list-clients
./wintermuted -revoke-client desktop
```

The token is printed **once** and stored only as a hash. Copy it now.

### 3. Run

```bash
./wintermuted            # add -debug for per-request logging
```

`GET /api/v1/health` is unauthenticated and reports liveness only. Everything
else needs `Authorization: Bearer <token>`.

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
  "roots": ["/mnt/media", "\\\\NAS\\Media"],
  "auto_approve_reads": true,
  "always_allow": [],
  "never_allow": []
}
```

`roots` is the list of directories this machine will let the assistant touch.
Nothing outside them is reachable, whatever the model asks for. On Windows they
can be UNC paths (`\\NAS\Media`) or mapped drives. `always_allow` and
`never_allow` name tools that skip the prompt or are refused outright.

`server_url`, `token` and `roots` can be overridden by `WINTERMUTE_SERVER`,
`WINTERMUTE_TOKEN` and `WINTERMUTE_ROOTS` (OS-separated list), so a token never
has to be written to disk on a shared machine.

Then run it:

```bash
./wintermute                                  # interactive REPL
./wintermute -prompt "tidy up /mnt/media/tv"  # one request, then exit
./wintermute -roots /mnt/media/movies         # override roots for this run
./wintermute -config ./other.json
./wintermute -yes                             # auto-approve writes — see below
```

In the REPL, type a request and answer the approval prompts; `/exit` quits.

`-yes` auto-approves write-risk actions without asking. Destructive actions
still prompt. It's meant for unattended runs and it removes a confirmation you
would otherwise get on every rename — use it deliberately.

## Browser UI

The server serves an embedded UI at its listen address (`http://localhost:8080`
by default). Create a token with `-kind browser`, open the page, and paste it
in; it's kept in `localStorage` and mirrored into a cookie so plain navigations
authenticate too.

The browser has no local action set, so it can chat and use server-side lookups,
but it cannot list or rename anything on your disks. That takes the desktop
harness.

## A note on thinking

Claude thinks before it answers, and the Messages API rejects a tool-use turn
whose reasoning was dropped in between. Wintermute therefore stores each
turn's thinking blocks verbatim in the transcript and replays them unedited.
That is why `messages` has a `thinking` column — don't strip it.

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
