-- Rolled-up telemetry, and the watermark that makes folding cheap.
--
-- Raw samples live two hours. Everything older is answered from here, and the
-- rule that follows is the whole point of this file: **no query outside the raw
-- window ever touches a raw row**. A chart of yesterday reads minute buckets, a
-- month reads hours, a year reads days — each a few hundred rows regardless of
-- how many hosts report or how often. That is what keeps an old window as cheap
-- as a recent one instead of getting slower every week the fleet runs.
--
-- One table rather than three, with the granularity as part of the key. The
-- rollup job is then one function parameterised by bucket size, and the tiers
-- cannot drift apart in shape — hours are built by re-aggregating minutes, and
-- days by re-aggregating hours, which only works if every tier stores the same
-- facts.
--
-- Those facts are sums, counts and maxima. Never averages: an average cannot be
-- re-aggregated into a coarser bucket without lying about it, because it has
-- forgotten how many readings it stood for. A mean is recovered at query time
-- by dividing the sum by the count, which is correct at every tier. Maxima are
-- carried separately because a peak is not recoverable from a mean and is
-- usually the thing worth seeing — a box that averaged 40% but touched 100%
-- for a minute is a different box from one that sat at 40% throughout.
CREATE TABLE IF NOT EXISTS node_rollup (
    node    TEXT NOT NULL,
    -- 'minute', 'hour' or 'day'.
    bucket  TEXT NOT NULL,
    -- The start of the bucket, truncated to its granularity.
    at      TIMESTAMP NOT NULL,
    -- How many readings this row stands for. The divisor for every mean, and
    -- the weight when this row is folded into a coarser one.
    samples INTEGER NOT NULL,

    cpu_sum        REAL NOT NULL DEFAULT 0,
    cpu_max        REAL NOT NULL DEFAULT 0,
    load1_sum      REAL NOT NULL DEFAULT 0,
    load1_max      REAL NOT NULL DEFAULT 0,
    mem_used_sum   REAL NOT NULL DEFAULT 0,
    mem_used_max   INTEGER NOT NULL DEFAULT 0,
    -- Total memory is a fact about the machine rather than a measurement, so
    -- the maximum seen in the bucket is the honest summary: it changes only
    -- when RAM is added.
    mem_total_max  INTEGER NOT NULL DEFAULT 0,
    swap_used_max  INTEGER NOT NULL DEFAULT 0,
    disk_read_sum  REAL NOT NULL DEFAULT 0,
    disk_read_max  REAL NOT NULL DEFAULT 0,
    disk_write_sum REAL NOT NULL DEFAULT 0,
    disk_write_max REAL NOT NULL DEFAULT 0,
    net_rx_sum     REAL NOT NULL DEFAULT 0,
    net_rx_max     REAL NOT NULL DEFAULT 0,
    net_tx_sum     REAL NOT NULL DEFAULT 0,
    net_tx_max     REAL NOT NULL DEFAULT 0,

    PRIMARY KEY (node, bucket, at)
);

-- Reading a window for one host, and ageing a whole tier out, are the two
-- queries this table serves. The primary key covers the first; this covers the
-- second, so retention is a range delete rather than a scan.
CREATE INDEX IF NOT EXISTS idx_node_rollup_age ON node_rollup(bucket, at);

-- How far each tier has been folded.
--
-- This is what makes the job cheap: it processes only what arrived since it
-- last ran, rather than re-scanning a window every time. It is also what makes
-- deleting raw samples safe — raw is never aged out past the point the minute
-- tier has confirmed, so a sweep cannot destroy readings that were never
-- summarised.
CREATE TABLE IF NOT EXISTS node_rollup_watermark (
    bucket  TEXT PRIMARY KEY,
    -- Everything strictly before this instant has been folded into `bucket`.
    through TIMESTAMP NOT NULL
);
