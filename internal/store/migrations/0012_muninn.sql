-- Rename tool_audit to muninn.
--
-- Muninn is one of Odin's two ravens — Huginn is thought, Muninn is memory —
-- and this table is about to become the memory half of exactly that pairing.
-- It already holds the durable record of what the assistant did rather than
-- what it said: every proposed tool call, what the approval policy decided,
-- and what actually happened. The memory layer built on top of this treats
-- that as *episodic* memory (actions and outcomes) alongside the *semantic*
-- memory in `messages` (what was said), so the table earns a name that says
-- what it is rather than how it was first used.
--
-- The rename is data-preserving: ALTER TABLE ... RENAME TO keeps every row,
-- every column and the foreign key onto sessions, so the audit trail is
-- continuous across this migration. Nothing is dropped and nothing is copied.
--
-- Unlike the other migrations here this one cannot be written with
-- IF NOT EXISTS guards: SQLite has no conditional form of ALTER TABLE RENAME.
-- The runner in store.go records each file in schema_migrations and skips
-- anything already applied, so this executes exactly once. If it somehow ran
-- twice it would fail on the missing tool_audit and roll back inside its own
-- transaction — which for an audit table is the correct failure: loud, and
-- with the data untouched.
ALTER TABLE tool_audit RENAME TO muninn;

-- The index follows the table through the rename but keeps its old name.
-- Recreating it keeps the schema readable, and costs nothing on a table this
-- size.
DROP INDEX IF EXISTS idx_audit_session;
CREATE INDEX IF NOT EXISTS idx_muninn_session ON muninn(session_id, created_at DESC);
