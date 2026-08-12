# Change Log

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
