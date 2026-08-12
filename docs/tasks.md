# Personal Tasks & the Assistant

Three modules that ship together: a personal task list, a Claude chat window
that can act on it, and an optional mirror of it into Google Calendar.

- **`internal/todo`** — lists, tasks, due dates, an agenda and a month calendar,
  at `/todo`.
- **`internal/assistant`** — the chat window and the tool-calling loop, at
  `/assistant`.
- **`internal/gcal`** — the Google Calendar link, at `/todo/google/*`.

They are separate packages on purpose. `todo` is usable with the other two
switched off entirely (no API key, no Google client, no network); `assistant`
has no idea what a task is — it only knows how to run tools that somebody
registered; and `gcal` reads tasks but `todo` knows nothing about calendars, so
the dependency runs one way and stays visible.

---

## The task module

| Route | |
|---|---|
| `GET /todo` | The page: agenda, lists, tasks, calendar |
| `GET /todo/data` | Lists + tasks + agenda in one response |
| `GET /todo/calendar?month=YYYY-MM` | Tasks due in a month, grouped by day |
| `POST/PUT/DELETE /todo/lists[/:id]` | List CRUD |
| `POST/PUT/DELETE /todo/tasks[/:id]` | Task CRUD |
| `POST /todo/tasks/:id/status` | The checkbox and the "in progress" action |

**Everything is owner-scoped.** The owner is the signed-in username, resolved
from the session and never read from a request body; in local mode it is the
empty string, which is one owner and therefore one code path for both modes.
Every query filters on it, including the ones that take an id — a list id is
guessable, so `GetList` and `CreateTask` check ownership rather than trusting
that the caller got the id from a page they were allowed to see.

A few rules worth knowing:

- **Status drives `completed_at`, not the client.** A completion timestamp the
  browser can set is a completion timestamp that lies. Ticking a task stamps it;
  un-ticking clears it.
- **An edit cannot move a task between lists.** That is a different operation
  with its own ordinal bookkeeping, and accepting a new `list_id` on a plain
  edit would silently reorder two lists.
- **The agenda excludes done tasks; the calendar includes them.** A list of
  things to do should not be mostly things already done — but a calendar is a
  record of when work landed as much as a plan, and a month showing only what
  slipped would misrepresent it.
- **Dates are stored as `YYYY-MM-DD` text, not timestamps.** A due date is a
  calendar day: "due Friday" should not move because the reader is in another
  timezone.

---

## The assistant

`/assistant` is a chat window. The model runs with adaptive thinking on
`claude-opus-5` (override with `ASSISTANT_MODEL`) and a tool set supplied by the
registry. See `RUNTIME_ARGS.md` for the environment it needs.

### What it can see and change

**Two tools, both confined to the signed-in user's own to-do lists:**

| Tool | |
|---|---|
| `list_todo_lists` | Read the user's lists — title, description, archived flag, task count, done count. Optional `include_archived`. |
| `create_todo_list` | Create a list from a title, an optional description and optional initial tasks. |

That is the entire action surface — the assistant cannot modify controls,
security NFRs, policies, the risk register, CRM clients or template settings,
and it has no read access to them either.

`list_todo_lists` exists to stop the obvious failure: with no way to see what
already exists, asking for the same list twice produced two lists. It returns a
projection rather than the stored rows, because `List` carries the owner and the
owner is session state the model has no business reading back.

The registry is assembled in one place:

```go
// internal/app/app.go — registerTaskRoutes
registry := assistant.NewRegistry()
registry.Register(todo.Tools(todoService)...)
```

That is deliberate. `assistant` never imports a feature module and a feature
module never registers itself, so the complete list of things a language model
may do in this application is readable in one function rather than assembled
from `init()` calls across the tree. **Widening it is an edit here**, which is
the point: the modules alongside this one hold an approved control catalogue and
a policy set with an audit trail and a separation-of-duties check, and giving a
model write access to those is a decision that should be made on purpose.

Tool handlers are thin adapters over the same `Service` methods the HTTP layer
calls, so a list Claude creates goes through the same validation, the same owner
scoping and the same timestamps as one typed into the page.

### How the loop works

```
user message
  → Messages API (history + tool specs)
  → stop_reason == "tool_use"?
      → run each tool_use block, owner-scoped
      → write an audit row per call
      → send the tool_result blocks back
      → repeat (max 6 iterations)
  → final assistant reply
```

Points that are load-bearing rather than incidental:

- **`owner` comes from the session, never from the model.** It is passed to the
  handler by the loop; a tool cannot be told which user to act as. This is the
  whole basis on which a handler is allowed to write anything.
- **A failing tool is reported, not fatal.** The error goes back as an
  `is_error` tool result, so the model can correct itself or explain the
  problem. A bad due date should not fail the conversation.
- **The iteration ceiling is required.** A model that calls a failing tool,
  reads the error and tries again has no natural stopping point.
- **Audit rows are written as each call completes**, not at the end. If the
  request is cancelled midway, actions that already happened are still recorded.
- **`stop_reason` is checked before the content.** A refusal returns HTTP 200
  with empty or partial content, so reading `content[0]` first would break on it.

### Storage

`assistant_messages.content_json` holds the content blocks in this package's own
form, not the SDK's. The two have different jobs: the SDK type is shaped for
building a request and changes with the SDK, while these rows have to be
readable by whatever version of the binary runs next year. Conversion lives in
one place (`client.go`), so an SDK upgrade breaks at compile time there rather
than silently failing to decode a year of stored conversations.

Tool blocks round-trip intact — the API needs the `tool_use` block back
unchanged on the next turn, paired to its `tool_result` by id, and a block that
was edited or reconstructed is rejected.

---

## The Google Calendar link

Optional, and off unless `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` are set —
see `RUNTIME_ARGS.md`. The panel on `/todo` says so rather than offering a
button that cannot work.

| Route | |
|---|---|
| `GET /todo/google/status` | Configured? Connected? Which account, last sync, how many tasks mirrored |
| `GET /todo/google/connect` | Redirect to Google's consent screen |
| `GET /todo/google/callback` | Google's redirect target; exchanges the code |
| `POST /todo/google/sync` | Push dated tasks, remove events for tasks that no longer qualify |
| `POST /todo/google/disconnect` | Revoke at Google, clear the local record |
| `GET /todo/google/events?month=YYYY-MM` | The connected calendar's events, grouped by day |

**The sync is one-way by design.** Tasks with a due date become events; events
never become tasks. This app is the system of record and Google is a view of it,
which is what lets the whole feature exist without conflict rules, sync tokens
or tombstones. Events are read back only for the overlay on the month grid.

**The scope is `calendar.app.created`.** The credential reaches the "CareLock
Tasks" calendar the app creates and nothing else — it cannot see, change or
delete the user's existing meetings. That is also why the overlay is a
reconciliation view rather than a general "show me my diary": it shows what
actually landed in Google, plus anything the user put on that calendar
themselves. Reading their other calendars would need `calendar.readonly`, which
is a deliberate widening rather than an oversight.

A few rules worth knowing:

- **Only dated tasks are mirrored, done ones included.** An event needs a day to
  sit on, and the calendar is a record of when work landed — the same rule the
  month view already follows.
- **Events are all-day, with the API's exclusive end date.** A due date is a
  calendar day; a clock time would invent precision nobody entered and would
  move the event for a reader in another timezone. End is due date + 1, because
  setting both to the same day produces an event Google renders wrongly.
- **A stored signature decides what gets written.** Unchanged tasks are skipped,
  so a sync costs one API call rather than one per task. `updated_at` is not
  part of the hash: it moves when an ordinal changes, which the calendar does
  not show.
- **Tokens are encrypted with a key kept outside the database.** Back up
  `<sqlite-path>.gcal.key` alongside the database. A copy of the database
  without it yields "reconnect required", not a working link — which is the
  point.
- **Disconnect leaves the events.** They are on a calendar the user owns and can
  delete in one action; silently erasing a month of it is not this code's call.
- **`gcal` is not in the assistant's tool registry.** Pushing somebody's task
  list to an external service is not an action a language model should be able
  to take unprompted.

---

## Testing

`internal/assistant` defines the model behind a `Messenger` interface, so the
tool loop is tested against a scripted model: tool dispatch, result feedback,
the audit trail, error handling, the iteration ceiling, owner isolation and the
history round trip all run without an API key or a network call. The SDK
conversion layer (`toSDKTools` / `toSDKMessages`) is tested directly, since a
mistake there is otherwise invisible until a live call returns a 400.

`internal/todo` covers validation, the completion rules, the agenda buckets, the
calendar, and — the one that matters most — owner isolation: that a second user
cannot read, write to, or delete another's lists even with a valid id.

`internal/gcal` runs the sync against a stub Calendar API, so create, update,
skip, delete, and recovery from an event deleted in Google are all exercised
without a Google account: the token endpoint and the API root are package
variables the tests point at `httptest` servers. Alongside those, the checks
that guard the security properties — the consent URL's scope and PKCE
parameters, single-use owner-bound state, the encryption round trip and its
refusal to decrypt under the wrong key, that the refresh token is not readable
in the database, and that two owners' link rows for the same task id stay
distinct.

**What is not covered by tests:** a real end-to-end call to the Anthropic API.
That path needs a key, and the model's actual willingness to call the tool
rather than describe it is a prompt property, not something a test can pin. The
same applies to Google: the stub asserts what this code sends and how it reacts,
but a live consent screen and Google's own refresh-token behaviour are not
something a test can stand in for.
