-- Backends declared through the UI, as opposed to backends.json.
--
-- This is deliberately a separate table from `backends` in 0003. That one is
-- described there as "a cache of an external truth, refreshed by probing, not
-- the authoritative record of anything", and Catalog.Refresh rewrites its rows
-- on every sweep. A declaration must outlive a probe, so it cannot live in a
-- table that probing owns.
--
-- The two layer at startup: backends.json is read first, then these rows are
-- added. A name declared in both belongs to the file, because the file is the
-- one an operator can inspect and put in version control, and a row in a
-- database silently shadowing it is how a config stops matching its host.
--
-- There is no api_key column and there will not be one. api_key_env names an
-- environment variable, exactly as backends.json does, so a credential still
-- reaches the process only through its environment and never through a browser
-- POST, this table, or a backup of it.
CREATE TABLE IF NOT EXISTS backend_config (
    name        TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    base_url    TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',
    api_key_env TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);
