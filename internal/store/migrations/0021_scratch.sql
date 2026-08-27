-- The scratch pad: free text with a name on it.
--
-- Notes already exist, but a note is a task on a reserved list — one line,
-- 4000 characters, either outstanding or dealt with. That shape is wrong for
-- somewhere to drop the output of a turn and work it over: there is no status
-- to set, the text runs to pages, and the point is the editing rather than the
-- doing. So this is its own table rather than a longer note.
--
-- It lives in the memory database beside the conversations rather than in the
-- metrics file: a document someone typed cannot be rebuilt from anything, so
-- it belongs where the backups already reach.
CREATE TABLE IF NOT EXISTS scratch_docs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT ''
);

-- The sidebar is ordered most-recently-touched first, which is the only
-- ordering the pad is ever read in.
CREATE INDEX IF NOT EXISTS idx_scratch_docs_updated ON scratch_docs(updated_at DESC);
