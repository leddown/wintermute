-- Model backends and the catalog of what they serve.
--
-- The server does not manage inference processes or download weights: it
-- observes backends, records what it found, and routes turns. These tables are
-- therefore a cache of an external truth, refreshed by probing, not the
-- authoritative record of anything.
CREATE TABLE IF NOT EXISTS backends (
    name        TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    base_url    TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',
    cloud       INTEGER NOT NULL DEFAULT 0,
    -- Health of the last probe: 'ok', 'unreachable', or 'unknown'.
    status      TEXT NOT NULL DEFAULT 'unknown',
    status_note TEXT NOT NULL DEFAULT '',
    probed_at   TIMESTAMP
);

-- One row per model a backend reports. Fields the backend does not expose are
-- left zero rather than guessed; the fit calculator infers what it can and
-- says when it is estimating.
CREATE TABLE IF NOT EXISTS catalog_models (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    backend_name TEXT NOT NULL REFERENCES backends(name) ON DELETE CASCADE,
    model_id     TEXT NOT NULL,
    family       TEXT NOT NULL DEFAULT '',
    params_b     REAL NOT NULL DEFAULT 0,
    quant        TEXT NOT NULL DEFAULT '',
    ctx_len      INTEGER NOT NULL DEFAULT 0,
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    -- Comma-separated capability flags: tools, vision, embedding, reasoning.
    capabilities TEXT NOT NULL DEFAULT '',
    -- Whether the backend reported the model resident when last probed.
    loaded       INTEGER NOT NULL DEFAULT 0,
    vram_bytes   INTEGER NOT NULL DEFAULT 0,
    last_seen_at TIMESTAMP NOT NULL,
    UNIQUE (backend_name, model_id)
);

CREATE INDEX IF NOT EXISTS idx_catalog_backend ON catalog_models(backend_name, model_id);

-- A session pins the backend and model that serve it, so switching models
-- mid-conversation is an explicit act and the transcript records which model
-- produced which turn. Empty means "use the configured default".
ALTER TABLE sessions ADD COLUMN backend TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN model TEXT NOT NULL DEFAULT '';
