-- Which models each node should be holding.
--
-- Desired state, set by the operator, read back by the agent from the reply to
-- its own report. It lives here rather than in the metrics database because
-- that file is explicitly disposable — telemetry decays and is pruned or
-- deleted wholesale — while this is configuration, and a fleet that forgot
-- which machine holds which weights because a metrics database was cleared
-- would be a genuinely bad afternoon.
--
-- The node name is the client the agent authenticates as, matching
-- node_samples.node in the other database. There is deliberately no foreign key
-- anywhere: the two live in different files, and an assignment for a node that
-- has not reported yet is exactly how a new machine gets provisioned before it
-- is switched on for the first time.
--
-- rel_path names a file in the server's own repository, matching
-- model_repo_files.rel_path. It is the only thing the agent is ever told, and
-- the reason it is a repository-relative path rather than anything resembling a
-- local path is that the agent must decide where the file lands from its own
-- configuration. A server that could name a destination on the node could write
-- anywhere on it.
CREATE TABLE IF NOT EXISTS node_models (
    node       TEXT NOT NULL,
    rel_path   TEXT NOT NULL,
    -- Who asked for it and when, so an assignment nobody remembers making can
    -- at least be dated.
    assigned_at TIMESTAMP NOT NULL,
    PRIMARY KEY (node, rel_path)
);

-- The agent's own question — "what should I be holding?" — is by node, which
-- the primary key covers. This covers the other direction: which nodes hold a
-- model, asked by the repository screen before offering to delete one.
CREATE INDEX IF NOT EXISTS idx_node_models_path ON node_models(rel_path);
