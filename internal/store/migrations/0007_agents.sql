-- Agents: named configurations of the assistant, each with its own document
-- library and its own idea of what it may consult.
--
-- The need is concrete. One conversation is about a GRC engagement and should
-- reach that client's regulations and control catalog; the next is about a
-- different client, or about the firm's own finances, and must not. Before
-- this, every session saw every tool and no documents at all, so the only way
-- to give the model context was to paste it into the message.
--
-- An agent is deliberately thin: a prompt, a model pin, a set of enabled
-- sources, and the documents uploaded to it. It is not a separate assistant —
-- the same agent loop, transcript store and approval model serve all of them.

CREATE TABLE IF NOT EXISTS agents (
    -- A slug, so a client (grc, a script) can name an agent in configuration
    -- without first looking up a numeric id.
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    -- Layered over the base system prompt rather than replacing it: the rules
    -- about never claiming an action was performed apply to every agent.
    system_prompt TEXT NOT NULL DEFAULT '',
    -- Optional model pin for sessions opened against this agent.
    backend       TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    -- Comma-separated source names: documents, grc, web. What is absent is not
    -- offered to the model, which is the point of the table.
    sources       TEXT NOT NULL DEFAULT 'documents',
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- A document uploaded to one agent's library. The extracted text lives here;
-- the original bytes do not, because nothing in wintermute serves them back and
-- keeping them would make the database grow with material it never reads.
CREATE TABLE IF NOT EXISTS agent_documents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id    TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    title       TEXT NOT NULL DEFAULT '',
    filename    TEXT NOT NULL DEFAULT '',
    media_type  TEXT NOT NULL DEFAULT '',
    source_url  TEXT NOT NULL DEFAULT '',
    sha256      TEXT NOT NULL DEFAULT '',
    byte_size   INTEGER NOT NULL DEFAULT 0,
    text_chars  INTEGER NOT NULL DEFAULT 0,
    extract_via TEXT NOT NULL DEFAULT '',
    uploaded_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_agent_documents_agent ON agent_documents(agent_id, id);

-- Section-aware chunks, which is what the search tool actually scores. Split at
-- headings rather than at a fixed width so a retrieved chunk is a passage
-- somebody wrote, and carries the heading that says what it is about.
CREATE TABLE IF NOT EXISTS agent_document_chunks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    document_id INTEGER NOT NULL REFERENCES agent_documents(id) ON DELETE CASCADE,
    ordinal     INTEGER NOT NULL DEFAULT 0,
    heading     TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_agent_document_chunks_document
    ON agent_document_chunks(document_id, ordinal);

-- Which agent a conversation belongs to. Empty means the unscoped assistant
-- that existed before this migration, so every stored session keeps working
-- exactly as it did.
ALTER TABLE sessions ADD COLUMN agent_id TEXT NOT NULL DEFAULT '';
