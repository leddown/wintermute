-- Clients are the devices allowed to talk to the API: the Windows harness,
-- a browser session, anything else added later. Tokens are stored hashed.
CREATE TABLE clients (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    token_hash  TEXT    NOT NULL UNIQUE,
    kind        TEXT    NOT NULL,
    created_at  TIMESTAMP NOT NULL,
    last_seen_at TIMESTAMP
);

-- A session is one conversation. The client harness keeps one per run; the
-- browser keeps one per chat.
CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    client_id  INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    title      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE INDEX idx_sessions_client ON sessions(client_id, updated_at DESC);

-- Messages are the durable transcript replayed to the model on every turn.
CREATE TABLE messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    role         TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',
    tool_calls   TEXT NOT NULL DEFAULT '',
    tool_call_id TEXT NOT NULL DEFAULT '',
    is_error     INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMP NOT NULL,
    UNIQUE (session_id, seq)
);

-- Every tool call the model proposed, what the approval policy decided, and
-- what happened. This is the audit trail: it must not live only in the chat
-- transcript, because a transcript can be edited or discarded.
CREATE TABLE tool_audit (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    call_id    TEXT NOT NULL,
    tool_name  TEXT NOT NULL,
    side       TEXT NOT NULL,
    risk       TEXT NOT NULL,
    input      TEXT NOT NULL DEFAULT '',
    decision   TEXT NOT NULL,
    outcome    TEXT NOT NULL DEFAULT '',
    is_error   INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL
);

CREATE INDEX idx_audit_session ON tool_audit(session_id, created_at DESC);
