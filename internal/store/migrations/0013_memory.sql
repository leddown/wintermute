-- Memory: provenance on every message, and the two per-conversation switches.
--
-- `messages` already stores what the memory layer needs most: role and content
-- as neutral text, never a prompt with some model's chat template already
-- applied. That is what lets a transcript outlive the model that produced it,
-- and it is why this migration adds columns rather than a second table.
--
-- What the rows could not say is *which* model each line came from. A store
-- meant to be read years from now, across models that will have been replaced
-- several times over, has to record that per message rather than per session:
-- sessions can be repointed at another backend mid-conversation (see
-- SetSessionModel), so the session's current model is not the model that wrote
-- the line above.
--
-- The column names follow OpenTelemetry's GenAI semantic conventions where
-- they line up (gen_ai.request.model, gen_ai.provider.name, token usage), so
-- an archive read by some future tool is describing itself in a vocabulary
-- that already exists rather than one invented here.
--
-- As with 0012, SQLite has no conditional ALTER TABLE ADD COLUMN. The runner
-- applies each file exactly once and a re-run would fail loudly without
-- touching data.

-- Which model produced an assistant message, or was serving the session when a
-- user message arrived. Empty means the row predates this migration.
ALTER TABLE messages ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN backend TEXT NOT NULL DEFAULT '';

-- The provider's own token count for this message where it reported one.
-- 0 means "not reported", not "empty": only assistant messages come back with
-- a usage figure, so user and tool rows carry 0 and are estimated when the
-- retrieval budget is computed. Storing an estimate here would make a guess
-- indistinguishable from a measurement a year later.
ALTER TABLE messages ADD COLUMN token_count INTEGER NOT NULL DEFAULT 0;

-- The two switches, deliberately independent.
--
-- `record` decides whether this conversation is written to the store at all.
-- `recall` decides whether prior context is retrieved into it. Drawing on the
-- full history while leaving no trace of the present conversation is a valid
-- and useful combination, so these are two columns rather than one mode.
--
-- Both default to 1. Being on the record is the default state and going off it
-- is always an explicit act; a conversation is never inferred to be ephemeral.
ALTER TABLE sessions ADD COLUMN record INTEGER NOT NULL DEFAULT 1;
ALTER TABLE sessions ADD COLUMN recall INTEGER NOT NULL DEFAULT 1;

-- Recency is half of retrieval ranking, and it is asked for across every
-- session a client owns rather than within one conversation, which the
-- existing UNIQUE (session_id, seq) index cannot answer.
CREATE INDEX IF NOT EXISTS idx_messages_created ON messages(created_at DESC);
