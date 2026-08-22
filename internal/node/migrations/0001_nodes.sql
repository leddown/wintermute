-- Fleet telemetry, in its own database file.
--
-- Separate from the main store on purpose. Host metrics are a time series and
-- behave nothing like anything else this server keeps: they arrive constantly,
-- they are worth little within days, and they would outgrow the conversation
-- memory by orders of magnitude inside a year. That memory is the thing that
-- must survive the years and is snapshotted by VACUUM INTO on a schedule —
-- putting telemetry beside it would inflate every backup and slow every
-- snapshot, for data whose value has already decayed by the time it is copied.
--
-- Keeping them apart means the memory backup stays small and quick, and this
-- file can be aggressively pruned, or deleted outright, without touching
-- anything that matters.

-- One row per known host.
--
-- The name is the client the agent authenticates as, issued by
-- `wintermuted -add-client <name> -kind node`. It is never taken from the
-- request body: a node that could name itself could write samples attributed
-- to another.
CREATE TABLE IF NOT EXISTS nodes (
    name          TEXT PRIMARY KEY,
    hostname      TEXT NOT NULL DEFAULT '',
    os            TEXT NOT NULL DEFAULT '',
    kernel        TEXT NOT NULL DEFAULT '',
    cores         INTEGER NOT NULL DEFAULT 0,
    agent_version TEXT NOT NULL DEFAULT '',
    first_seen_at TIMESTAMP NOT NULL,
    last_seen_at  TIMESTAMP NOT NULL
);

-- Raw samples, at full resolution.
--
-- These live two hours and are then folded into buckets and deleted. The shape
-- is chosen so that folding is cheap and so that nothing outside the live
-- window ever reads this table:
--
--   * the index is on time alone, so ageing rows out is a range delete rather
--     than a search;
--   * every column is an additive fact or a rate that can be re-derived from
--     one, so a bucket can be built by summing and counting without needing to
--     revisit what it summarised;
--   * no averages are stored, because an average cannot be re-aggregated into
--     a coarser bucket without lying about it.
CREATE TABLE IF NOT EXISTS node_samples (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    node           TEXT NOT NULL,
    at             TIMESTAMP NOT NULL,
    cpu_percent    REAL NOT NULL DEFAULT 0,
    load_1         REAL NOT NULL DEFAULT 0,
    load_5         REAL NOT NULL DEFAULT 0,
    load_15        REAL NOT NULL DEFAULT 0,
    mem_total      INTEGER NOT NULL DEFAULT 0,
    mem_used       INTEGER NOT NULL DEFAULT 0,
    swap_used      INTEGER NOT NULL DEFAULT 0,
    disk_read_bps  REAL NOT NULL DEFAULT 0,
    disk_write_bps REAL NOT NULL DEFAULT 0,
    net_rx_bps     REAL NOT NULL DEFAULT 0,
    net_tx_bps     REAL NOT NULL DEFAULT 0,
    uptime_seconds INTEGER NOT NULL DEFAULT 0,
    -- One report may be a backlog replayed after an outage, so the same
    -- (node, at) can arrive twice if an agent resends. Unique on the pair, and
    -- inserts ignore duplicates, so a replay is idempotent rather than
    -- doubling a spike.
    UNIQUE (node, at)
);

CREATE INDEX IF NOT EXISTS idx_node_samples_at ON node_samples(at);
CREATE INDEX IF NOT EXISTS idx_node_samples_node_at ON node_samples(node, at DESC);
