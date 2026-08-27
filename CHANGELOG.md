# Change Log

## 2026-08-27 (Adding a node stops being a research project)

Adding a machine to the fleet took a token from one command, an agent binary
copied by hand, a unit file, and an address the operator had to know. Every step
of that could fail on the far machine, at the end, after the token had been
spent. It is now one command on the server and two lines on the node.

**`scripts/add-node.sh`** does the server half. It checks the agent has actually
been built here *before* issuing anything, registers the machine, works out the
address and confirms it by fetching the installer with the token it just issued,
then prints the lines to run over there.

The address is worked out rather than assumed because `WINTERMUTE_ADDR` is what
wintermuted *binds*, which is not what a node connects to the moment anything
sits in front of it. A reverse proxy publishes 80 or 443 and forwards to the
listener, making the listen port precisely the address that will not work.

**The token goes on its own line.** Written into the command directly it appears
twice, making one line long enough for a terminal to wrap — and pasting a
wrapped line brings the wrap back as spaces, inside the token, inside the
`Authorization` header. The server then answers `401 invalid token` while
working perfectly, which is indistinguishable from a genuinely wrong token and
much commoner. Every place that prints this command now assigns the token first.

**`wintermute-node-update`** is installed on each node beside the agent, so a
later update is one argumentless command on the host rather than a curl line
rebuilt around a token that was shown once. It carries no copy of the install
steps: it reads the address and token out of `node.env` and asks the server for
`install.sh`, so a node updating runs the *current* installer. `--check`
compares against the server's `SHA256SUMS` and exits 10 when an update is
waiting, so a loop over hosts can skip the ones with nothing to do.

This is still not an update channel. The agent never fetches its own executable
and nothing in its reporting loop can reach the puller — an operator runs it.
Putting it on a timer is what would convert it into one, and then a compromised
server is root on every node in the fleet.

### `wintermuted -add-client` no longer sets a trap

It read `WINTERMUTE_DB` from its own environment and fell back to a *relative*
`wintermute.db`. The service's config is a systemd `EnvironmentFile`, which an
interactive shell never sees, so the flag typed in a checkout created a second
database there and registered the client into it. The token was real; it was
just in a file the server never opens, and the only symptom was `invalid token`
arriving later from somewhere else.

It now resolves the database out of `/etc/wintermute/wintermute.env` before a
checkout's `.env`, says which one it used, and refuses to create one for a
client command — `-migrate-only` may still, since that is how a database comes
into existence. It also hands the `-wal`/`-shm` files back to the account that
owns the database, because SQLite creates them as whoever is running and a
`sudo` invocation otherwise left the service unable to write.

`scripts/clients.sh` existed to work around all of that. It stays as the better
front door — it refuses relative paths outright and runs as the service user —
but it is no longer load-bearing, and it can issue a `node` token now, which it
could not.

### Help that can be found

**Utilities → Guides → Adding a node** is the walkthrough: what a node is, what
is needed first, the steps with the checks between them, updating later, and a
table of every failure this took to find. Markdown in the repo rendered in the
browser, the way the FAQ already is, with `{{server}}` substituted for this
server's own address at render time — a written-down address always gets this
wrong in the same direction, since the one that is obvious while writing is
localhost.

The Fleet screen explained itself only while it was empty, which is the one time
nobody needs it explained; the pointer to the guide now renders either way.

Also fixes the Markdown renderer, which consumed one line per list item, so a
hard-wrapped list rendered as a column of one-item lists with stray paragraphs
between them. The FAQ has no lists, so this was invisible until something else
was written.

### Also

- **Scratch**, a text pad, under **Workspace**. Free text with a name on it,
  saved on a timer: somewhere for a reply to go and be worked over, since the
  transcript is a log and is replayed to the model verbatim. Every message in
  the transcript carries a button that puts it there.
- **Workspace** — the tasks and the pad under one tab in the bar rather than
  two, on the Core view's pane-swap shape, each group's sidebar travelling with
  its pane.
- The task pane's header read the list name out of a list it loaded *after*
  rendering, so renaming a list changed the sidebar row and left the heading
  above the tasks showing the old name.

## 2026-08-14 (Portfolio: the investment ledger moves in from morpheus)

`internal/fintech` — a transaction ledger with holdings derived from it, AI
price forecasts scored against what actually happened, and a periodic position
review — moved here from morpheus. It is reachable under `/api/v1/fintech/*`,
as agent tools, and in the browser UI under **Portfolio**.

It moved for the model. Forecasting and reviewing cost one call per symbol, and
there those calls could only go to Anthropic; here they go through the router,
so a local model on your own hardware can do the work — which is the argument
this whole program makes, applied to the one module that was paying a per-token
price to disagree with it.

### What changed in the port

**No owner column.** Morpheus scoped every row to a signed-in user. This server
authenticates clients by token and holds one portfolio, so `user_id`, the
`userID` parameter threaded through every method, and the `(user_id,
dedupe_hash)` uniqueness constraint are gone rather than stubbed — the same
decision, for the same reason, that 0004 made for the workspace tables.

**Quantities are summed in Go.** Postgres had `NUMERIC(28,10)` and could add
fractions of a bitcoin exactly in SQL. SQLite would coerce them to float64 and
lose the eighth decimal, so quantities stay TEXT and every aggregation runs
through `math/big`. Prices, fees and totals were always integer cents and stay
INTEGER, which is exact everywhere.

**Structured output degrades instead of failing.** Morpheus forced a tool call
to guarantee schema-valid JSON, which is a property of Anthropic's API and not
of a llama.cpp server. `structured.go` declares the schema as a tool *and*
states it in the system prompt, takes the tool call when there is one, finds
the JSON object in the text when there is not — brace-depth scan, string-aware,
so a rationale containing `}` does not truncate it — and on a failure retries
once, telling the model what could not be read. What is left is a validation
error someone can see, not a forecast assembled from a half-parsed reply.

**Secrets come from the environment.** `MARKET_DATA_API_KEY`,
`MARKET_DATA_PROVIDER`, `KRAKEN_API_KEY`/`SECRET`,
`ALPACA_PAPER_KEY`/`SECRET`. Morpheus encrypted these into the database with a
key it already had for signing sessions; this server has none, and a secret it
cannot protect at rest it does not store. `FINTECH_SCAN_INTERVAL` and
`FINTECH_REVIEW_INTERVAL` enable the background passes and default to off:
they spend model time on their own schedule, which should be asked for.

**The tests actually run.** They needed a live PostgreSQL server before and
skipped themselves without one, which meant the ledger's arithmetic — the
moving-average cost basis, realised P&L, the closed position that must not
appear — went unverified on most runs. In-memory SQLite has no such excuse: 25
tests, every run, in milliseconds.

**No email digest.** Morpheus mailed the review digest through the SMTP setup
it shared with its canary alerts. Nothing here sends mail, so the `Alerter`
seam is left unwired: reviews are still generated, still stored, still read in
the UI, and the carry-over queue still works — it simply has nothing to hand a
digest to.

## 2026-08-14 (Agents: named document libraries and sources)

An **agent** is a named configuration of the assistant — a prompt, a model pin,
the sources it may consult, and a library of documents uploaded to it. Pick one
when starting a chat, or name one from another application, and the
conversation reaches that material and nothing else. Not a second agent loop:
the loop, the transcript and the approval model are unchanged, and an agent
narrows what a turn can reach. See [docs/agents.md](docs/agents.md).

Two problems, one answer. Separation — a conversation about one client's
engagement should not reach another's documents, and before this every session
saw every tool and no documents at all. And groundedness — asked "how many
Security NFRs are focused on network segmentation?", a model with no access to
the catalog explains what it would need in order to answer and offers to work
through the list if someone pastes it in. It cannot know the list is one HTTP
call away. Now it is a tool call away, and the answer comes back with
references.

### `internal/knowledge`

Agents and their libraries. Migration 0007 adds `agents`, `agent_documents`,
`agent_document_chunks`, and `sessions.agent_id`. Uploads are extracted (PDF
via `pdftotext`; text, markdown, HTML, CSV, JSON and YAML directly), split at
headings rather than at a fixed width, and searched with BM25 — the heading
scored with the body, so a section titled "Incident reporting" is found by that
phrase even when its body says "notify the authority".

Only the extracted text is kept, not the original bytes: nothing here serves
the file back, and storing it would grow the database with material it never
reads. PDF extraction has no pure-Go fallback on purpose — poppler's layout
mode preserves the structure the chunker splits on, and a PDF parser would be
this program's third dependency for a job it would do worse.

The document tools are registered per session and bound to the session's agent,
so there is no agent argument for a model to change: one agent cannot read
another's library by naming it. A test covers exactly that.

### `internal/grc` and `internal/websearch`

Two new sources an agent can be given.

`grc` queries a GRC application's read-only knowledge API — its Security NFR
catalog, 800-53 controls, regulation coverage, policies and risk register —
through four tools. `grc_search` passes through both of that API's counts, how
many records matched any term and how many matched every term, because for a
two-word question those differ and reporting the first as the second turns a
precise question into an overstatement. `grc_list_nfrs` returns the whole
catalog, which is the only honest way to answer "how many".

`websearch` is `web_search` against the operator's own SearXNG instance plus
`fetch_url`. SearXNG rather than a search API for the same reason this program
runs local models: a search API makes every query — which client, which
regulation, which vulnerability — somebody else's log. `fetch_url` refuses
private address space in the dialer rather than on the hostname, so a name that
resolves publicly during validation and to a metadata endpoint a moment later
is still refused.

### Scoping the loop

`agent.WithScope` narrows the registry to the session agent's sources and
layers its prompt over the base one. `internal/app/scope.go` holds the wiring,
because the loop should not have to import three modules to pass them through.

A session with no agent, or one whose agent was deleted, gets no knowledge
tools — the assistant this was before — rather than everything. A source an
agent declares that the server has not configured is reported to the model so
it can say so; a test found that the prompt stayed silent when *every* declared
source was unconfigured, which was the case where silence misleads most.

### API and UI

`GET/POST/PUT/DELETE /api/v1/agents`, document upload and delete beneath each,
and `POST /api/v1/sessions` now accepts an `agent`. A new **Agents** view
creates them, uploads to them and starts a chat as one; the chat names the
agent it is talking to, because the same question gets a different answer
depending on which library is behind it.

### Testing a backend from the UI

**Admin → Backends → Send a test question** sends one prompt to one backend and
reports the reply, the model, the elapsed time and the token counts. It creates
no session, offers no tools, and does not fall back to another backend: a probe
says a backend answers HTTP, this says it answers questions, and an answer from
its neighbour would be a lie. `Router.CompleteOn` is the no-fallback path a
test needs. A failure comes back as a result with its timing rather than as a
server error.


## 2026-08-12 (Accounting, and an admin surface)

A double-entry accounting module built on top of the CRM, and a view that says
what the server is actually running with.

### `internal/accounting`

A general ledger with EU VAT invoicing, payments and expenses, sized for a
consultancy of a handful of people. The shape follows what the mature
open-source systems converge on — Bigcapital routes every money event through
one ledger module that writes balanced entries atomically, GnuCash derives an
account's normal balance from its type, Beancount refuses a transaction that
does not balance rather than repairing it. This takes all three positions.

Three rules run through it:

- **Every balance comes from the ledger.** Invoices, payments and expenses are
  *sources*: each posts a balanced journal entry and stores its id. Nothing sums
  a document table to learn what revenue was.
- **Money is `int64` minor units.** The CRM next door uses `REAL` for rates and
  hours, which is fine for "roughly how much is outstanding" and unusable here:
  a ledger's whole claim is that debits equal credits exactly. Conversion
  happens once, at the CRM boundary. Hours become exact thousandths before they
  ever touch a price.
- **Issued documents are immutable.** EU VAT requires invoice numbers to be
  unique, sequential and gap-free, and an issued invoice may not be deleted or
  amended. Numbers are allocated inside the issuing transaction, so a failed
  issue cannot leave a hole; corrections go through credit notes on their own
  series.

`crm_bridge.go` is the seam: unbilled billable time becomes a draft invoice, one
line per time entry carrying `time_entry_id`, and issuing flags exactly those
entries in the same transaction that posts the ledger. Two conditions stop
double-billing — the CRM's `invoiced` flag, and a check against lines on
existing non-void invoices that covers the window between drafting and issuing.

Reports (trial balance, profit and loss, balance sheet, aged receivables, VAT
summary) read the ledger and nothing else. The balance sheet carries current
earnings explicitly, because the module posts no year-end closing entry. The VAT
summary cross-checks the documents against the two control accounts and reports
a mismatch rather than reconciling it silently — that difference means something
was posted to a VAT account by hand.

Ten tools go on the shared registry. `issue_invoice` is declared
`RiskDestructive`: not destructive in the data sense, but irreversible, and
under-declaring it would let `-yes` push invoices out of the door unseen.
Editing the chart of accounts, changing VAT rates and locking periods are
deliberately not exposed to the model.

- `internal/store/migrations/0005_accounting.sql` — eleven tables, a seeded
  chart of ~39 accounts and five VAT treatments. **The seeded 21% / 9% rates are
  a placeholder**; they differ by member state and must be corrected.
- `internal/api/accounting.go` — 36 routes. A locked period returns `409`, not
  `400`: the request was fine, the books are closed.
- An **Accounts** view in the browser UI, and [docs/accounting.md](docs/accounting.md).

### Admin

`/api/v1/admin/*` and an **Admin** view: status (uptime, database and WAL size,
row counts), configuration, backends with their probe status, hardware, the
server-side tools with their declared risk levels, and client tokens.

It never returns a secret — API keys and the Hugging Face token are reported as
configured or not, never by value, because this is a page that gets left open
and screenshotted. It also cannot issue a token: `wintermuted -add-client`
stays the only way to mint one, since a leaked browser token that could mint
more would turn one stolen session into permanent access. Revoking the client
you are signed in as is refused with a `409`.

### Also

- `scripts/clients.sh` — issue, list and revoke client tokens against the
  database the *service* reads. `wintermuted -add-client` takes `WINTERMUTE_DB`
  from its own environment and falls back to a relative path, so running it by
  hand from a checkout silently creates a second database and registers the
  client there; the token is real, the server never opens that file, and the UI
  answers "invalid token" with nothing to explain why.
- The browser UI's `hidden` attribute was inert on `.gate`, `.app` and
  `.row-form`: an author rule setting `display` beats the user-agent rule that
  makes `hidden` work, so the login card stayed on screen after a successful
  login with the app stacked below it. One `[hidden] { display: none !important }`
  fixes all three.
- `deploy/wintermuted.service` — the comment about moving to port 80 told you to
  set `AmbientCapabilities` alone, which cannot work while `CapabilityBoundingSet`
  is empty: an ambient capability must also be in the bounding set.

## 2026-08-11 (Workspace: tasks, CRM and company profile)

Three modules moved here from an RCSA/compliance application, which kept that
app to the control catalog, security NFRs, risk register and policy authoring,
and put the practice-management side in the tool that is already a personal
server.

### What arrived

- **`internal/todo`** — lists, tasks, an agenda bucketed into overdue / due
  today / next fourteen days / undated, and a month calendar.
- **`internal/crm`** — clients -> engagements -> billable time -> billing.
  Time entries snapshot the rate that applied when the work was logged, so
  revising a client's rate never reprices work already invoiced.
- **`internal/company`** — the operator's own legal name, address, registration
  numbers and contact details.
- **`internal/store/migrations/0004_workspace.sql`** — their tables.
- **`internal/api/workspace.go`** — the HTTP surface, behind the same bearer
  token as everything else. The application these came from gated writes behind
  a separate admin token on top of a login; there is one credential and one
  operator here, so a second tier would be a second name for the same token.
- Four views in the browser UI: the chat that was already here, plus Tasks, CRM
  and Company.

### The assistant was not ported — it became tools

That application had a separate "Assistant" page: its own Anthropic client, its
own conversation store, its own tool registry. Porting it would have produced a
second agent loop next to the one this server already runs, with two transcripts
and two places to look when a model does something surprising.

What came across instead is the capability. `internal/todo` registers
`list_todo_lists`, `create_todo_list` and `add_todo_task` on the existing tool
registry, so the chat that was already here can read and build task lists. One
loop, one transcript, one audit table. They are also reachable over MCP, since
that endpoint serves the same registry.

### Single-user, deliberately

Every table came with an `owner` column scoping rows to a signed-in user. This
server has no user accounts — it authenticates registered clients by bearer
token — so the column and the parameter are gone rather than stubbed out. A
scoping argument that nothing scopes on reads as a guarantee that is not being
made. The `Repository` interfaces lost `owner string` from twenty signatures
and the queries lost their `owner = ?` clauses.

### Adaptations

- The repositories moved from a dialect-aware `db.Conn` (that app also spoke
  PostgreSQL) to the store's `*sql.DB`. Only the generated-key helper actually
  differed; here it is `LastInsertId` and nothing else.
- Timestamps in the new tables are TEXT holding RFC3339 rather than the
  TIMESTAMP type the older migrations use. The ported code round-trips them as
  strings, and converting would have meant rewriting every scan to change
  nothing a user can see.
- The workspace shares the store's database handle rather than opening its own,
  so there is one file, one WAL and one busy_timeout.

### Google Calendar sync did not come

The task module in that application could mirror due dates into a Google
Calendar. That was an OAuth flow with an AES-256-GCM token store, a key file
beside the database and a `google.golang.org/api` dependency — a bigger piece of
work than the three modules it hung off. Tasks arrived without it. Noting it
rather than leaving it to be discovered.

### Verified

Both `go test ./...` suites pass. Exercised end to end against a throwaway
database: company profile save (including the http/https-only website guard
rejecting `javascript:`), the full CRM chain — client, engagement, time entry
inheriting the client rate, billing rollup, invoicing — task creation, agenda
bucketing, completion timestamps, list counts, the `hours must be 24 or less`
and `due date must be YYYY-MM-DD` validators, 401 without a token, and
`create_todo_list` called through the MCP endpoint.
