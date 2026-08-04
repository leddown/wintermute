-- Claude thinks by default, and the Messages API rejects a tool-use
-- continuation whose assistant turn dropped the thinking blocks that produced
-- the call. The transcript is replayed from this table on every iteration of
-- the turn loop, so the blocks have to be stored verbatim alongside the text.
ALTER TABLE messages ADD COLUMN thinking TEXT NOT NULL DEFAULT '';
