# Change Log

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
