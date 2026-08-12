-- Workspace: the consulting-practice modules moved here from the RCSA app —
-- company profile, CRM (clients, engagements, time), and tasks.
--
-- Owner columns are deliberately absent. In the app these came from, every one
-- of these tables carried an `owner` string scoping rows to a signed-in user.
-- Wintermute has no user accounts — it authenticates registered *clients* by
-- bearer token — and this is a single-operator tool, so the scoping column
-- would be a constant empty string on every row and an index on nothing. The
-- boundary that matters here is the token, and it is enforced at the API.

-- One row, id 1. The firm's own identity: name, address, registration numbers,
-- billing details. Stored as a JSON document rather than columns because it is
-- read and written whole, has no queryable structure, and grows a field
-- whenever a new jurisdiction wants one.
CREATE TABLE IF NOT EXISTS company_profile (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    profile_json TEXT NOT NULL DEFAULT '{}',
    updated_at   TEXT NOT NULL DEFAULT '',
    updated_by   TEXT NOT NULL DEFAULT ''
);

-- CRM: who the work is for.
CREATE TABLE IF NOT EXISTS crm_clients (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL,
    contact_name TEXT NOT NULL DEFAULT '',
    email        TEXT NOT NULL DEFAULT '',
    phone        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'Active',
    -- The client's default rate. An engagement may override it, and a time
    -- entry records the rate that actually applied, so changing this never
    -- silently reprices work already logged.
    hourly_rate  REAL NOT NULL DEFAULT 0,
    notes        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS crm_engagements (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    client_id    INTEGER NOT NULL REFERENCES crm_clients(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'Active',
    hourly_rate  REAL NOT NULL DEFAULT 0,
    budget_hours REAL NOT NULL DEFAULT 0,
    start_date   TEXT NOT NULL DEFAULT '',
    end_date     TEXT NOT NULL DEFAULT '',
    notes        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_crm_engagements_client ON crm_engagements(client_id);

-- Time entries carry their own rate rather than joining to the engagement's.
-- An invoice raised last quarter must not change because a rate was revised
-- this quarter, and `invoiced` is what stops an entry being billed twice.
CREATE TABLE IF NOT EXISTS crm_time_entries (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    engagement_id INTEGER NOT NULL REFERENCES crm_engagements(id) ON DELETE CASCADE,
    entry_date    TEXT NOT NULL DEFAULT '',
    hours         REAL NOT NULL DEFAULT 0,
    description   TEXT NOT NULL DEFAULT '',
    billable      INTEGER NOT NULL DEFAULT 1,
    rate          REAL NOT NULL DEFAULT 0,
    invoiced      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_crm_time_engagement ON crm_time_entries(engagement_id, entry_date);

-- Tasks.
CREATE TABLE IF NOT EXISTS todo_lists (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    archived    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS todo_tasks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    list_id      INTEGER NOT NULL REFERENCES todo_lists(id) ON DELETE CASCADE,
    title        TEXT NOT NULL,
    notes        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'todo',
    priority     TEXT NOT NULL DEFAULT 'normal',
    due_date     TEXT NOT NULL DEFAULT '',
    ordinal      INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT '',
    updated_at   TEXT NOT NULL DEFAULT '',
    completed_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_todo_lists_archived ON todo_lists(archived, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_todo_tasks_list ON todo_tasks(list_id, ordinal);
-- "What is due soon" is the only query that does not start from a list id;
-- without this it is a scan of every task the install has ever held.
CREATE INDEX IF NOT EXISTS idx_todo_tasks_due ON todo_tasks(due_date) WHERE due_date <> '';
