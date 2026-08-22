-- The master switch for shared memory.
--
-- The per-session `recall` flag (0013) decides whether one conversation draws
-- on the others. This is the switch above it: one place to turn the whole
-- thing off without editing configuration, restarting the server, or visiting
-- every session.
--
-- It exists because a memory that spans every conversation is exactly the
-- feature you want to be able to stop instantly — while shaking down a new
-- install and filling it with test rubbish, while debugging whether retrieval
-- is what is making a model answer oddly, or simply while doing something the
-- assistant has no business remembering later.
--
-- It is a row rather than an environment variable on purpose: an operator
-- turning memory off in a hurry should not have to restart the process that is
-- misbehaving, and the setting should survive the restart when they do.
--
-- One row, id 1, matching how twire_alert_config and fintech_review_config
-- store their single configurations.
CREATE TABLE IF NOT EXISTS recall_config (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    -- When 0, no conversation is given prior context, whatever its own recall
    -- flag says. Indexing deliberately carries on regardless: switching recall
    -- off is about what the model is shown, not about throwing away what is
    -- being said, and turning it back on should not reveal a gap covering
    -- however long it was off.
    recall_enabled INTEGER NOT NULL DEFAULT 1,
    updated_at     TIMESTAMP NOT NULL
);

-- Default to on, so an existing install behaves exactly as it did before this
-- switch existed.
INSERT OR IGNORE INTO recall_config (id, recall_enabled, updated_at)
VALUES (1, 1, CURRENT_TIMESTAMP);
