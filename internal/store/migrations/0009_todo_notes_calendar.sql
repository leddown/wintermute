-- Notes and the calendar, moved here from morpheus.
--
-- Morpheus kept these as two separate modules with two tables: `notes` (a
-- short body, a todo/done status, an optional event_date pinning it to a day)
-- and `calendar_events`, joined at read time into a merged feed. Neither is
-- reproduced verbatim here, because this server already has the task module
-- and a note was a task in all but name: a line of text that is either
-- outstanding or dealt with, optionally landing on a date.
--
-- So notes fold into todo_tasks on a reserved list, and only the piece with no
-- equivalent — a scheduled event, which has a start, an end and a duration,
-- and is not a thing to be done — gets a table of its own.

-- slug names a list the code has to be able to find again. An empty slug is
-- the normal case: lists people make are theirs, and nothing looks them up by
-- anything but id. The notes inbox is the one list the server creates and
-- reopens, and matching it by title would hand it to whoever first types
-- "Notes" into the new-list box.
ALTER TABLE todo_lists ADD COLUMN slug TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_todo_lists_slug ON todo_lists(slug) WHERE slug <> '';

-- Calendar events. Dates are TEXT like the rest of the task schema: an all-day
-- event stores "YYYY-MM-DD" and a timed one an RFC3339 instant normalised to
-- UTC. Both sort and range-compare correctly against a plain date bound
-- because RFC3339 carries the date as its leading ten characters, which is
-- what lets one BETWEEN serve a month view over a mix of the two.
CREATE TABLE IF NOT EXISTS todo_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    start_at    TEXT NOT NULL,
    -- Empty rather than NULL: an event with no stated end is the common case
    -- (morpheus made end_at nullable and every read had to unwrap it), and the
    -- rest of this schema already uses '' for "not set".
    end_at      TEXT NOT NULL DEFAULT '',
    all_day     INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_todo_events_start ON todo_events(start_at);
