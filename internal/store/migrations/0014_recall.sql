-- Recall: the retrieval index over the memory store.
--
-- The index is *derived*. Every row here can be rebuilt from `messages`, which
-- is the source of truth and the thing the archive carries. That separation is
-- deliberate and it is what makes the store survivable: an embedding model can
-- be changed, an index format can be abandoned, a vector library can go
-- unmaintained, and none of it touches the conversation history.
--
-- Which is also why vectors live in an ordinary BLOB column rather than in a
-- vector extension's virtual table. sqlite-vec is now available to this build
-- CGo-free (modernc.org/sqlite/vec), and it may well be worth adding later as
-- an accelerator — but it describes itself as pre-v1 with breaking changes
-- expected, and the canonical copy of anything meant to outlive several
-- generations of tooling should not be stored in a format that expects to
-- break. A float32 array in a BLOB is readable by anything, forever. If the
-- scan ever becomes the bottleneck, a vec0 index can be added beside this and
-- rebuilt from it at will, which is what a derived index is for.

-- The embedder pin.
--
-- One row. It records which model produced every vector in recall_vectors and
-- how wide those vectors are. Comparing a query embedded by model B against
-- documents embedded by model A is the classic silent failure of a vector
-- store — the distances are meaningless but nothing errors, so retrieval just
-- quietly gets worse. The server compares this against its configuration at
-- startup and refuses to run on a mismatch rather than retrieving nonsense.
CREATE TABLE IF NOT EXISTS recall_meta (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    embedding_model TEXT NOT NULL,
    dimension       INTEGER NOT NULL,
    created_at      TIMESTAMP NOT NULL
);

-- One vector per indexed message.
--
-- client_id and agent_id are denormalised from the session rather than joined
-- at query time. Scoping is a security boundary here, not an optimisation:
-- every retrieval filters by client_id, and having the column on the row being
-- scanned means the filter cannot be forgotten in a join that some later query
-- writes differently.
CREATE TABLE IF NOT EXISTS recall_vectors (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    client_id  INTEGER NOT NULL,
    agent_id   TEXT NOT NULL DEFAULT '',
    role       TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    dim        INTEGER NOT NULL,
    vector     BLOB NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_recall_scope
    ON recall_vectors(client_id, agent_id, created_at DESC);

-- The lexical half of retrieval.
--
-- Dense vectors handle paraphrase and miss exact rare terms; BM25 does the
-- opposite. Fusing both is worth appreciably more recall than either alone,
-- and this database can do BM25 natively.
--
-- It is contentless (content='') so the index stores terms but no copy of the
-- message text. That saves the store from holding two copies of every
-- conversation, and it means deleting a message does not leave its words
-- sitting in a shadow table — which matters when "delete this conversation"
-- has to mean it. contentless_delete=1 is what allows DELETE against such a
-- table; it needs SQLite 3.43 or newer, and this build ships 3.53.
CREATE VIRTUAL TABLE IF NOT EXISTS recall_fts USING fts5(
    content,
    content='',
    contentless_delete=1
);

-- Deleting a message must take its index entries with it, or the content stays
-- retrievable after the thing it came from is gone — which would defeat the
-- point of being able to delete a conversation at all.
--
-- recall_vectors is covered by ON DELETE CASCADE above. recall_fts is a
-- virtual table and cannot carry a foreign key, so it needs this trigger.
-- SQLite fires delete triggers for rows removed by a foreign-key cascade, so
-- deleting a whole session reaches all the way through: session → messages →
-- vectors and index entries.
CREATE TRIGGER IF NOT EXISTS recall_fts_after_message_delete
AFTER DELETE ON messages
BEGIN
    DELETE FROM recall_fts WHERE rowid = old.id;
END;

-- Work not yet done.
--
-- A message is queued here when it is written and removed once it has been
-- embedded. The write path never waits for an embedding: losing an embedding
-- is recoverable — it can be recomputed from the message — while losing the
-- message is not. So the message is committed first and indexed after, and if
-- the embedder is down or slow the conversation carries on regardless with a
-- backlog to work through later.
CREATE TABLE IF NOT EXISTS recall_queue (
    message_id  INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    enqueued_at TIMESTAMP NOT NULL,
    -- attempts lets a message that keeps failing to embed be recognised rather
    -- than retried forever at the front of the queue.
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_recall_queue_order ON recall_queue(enqueued_at);
