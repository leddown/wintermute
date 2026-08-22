-- GPU telemetry.
--
-- Aggregate columns rather than a row per card. A homelab box has one or two
-- GPUs, and what a time series needs to answer is "was this machine's GPU busy
-- at three in the morning" — which the aggregates answer at a fraction of the
-- storage and without a second table to fold, prune and join on every query.
--
-- Utilisation and temperature are maxima across the cards, not means: with one
-- card saturated and another idle, an average says the machine is half busy,
-- which is true of nothing and hides the card that is the actual constraint.
-- Memory and power are summed, because those really are totals for the box.
--
-- Per-card identity lives on the node instead, as a fact about the machine
-- rather than a measurement repeated every fifteen seconds.
ALTER TABLE node_samples ADD COLUMN gpu_util REAL NOT NULL DEFAULT 0;
ALTER TABLE node_samples ADD COLUMN gpu_mem_used INTEGER NOT NULL DEFAULT 0;
ALTER TABLE node_samples ADD COLUMN gpu_mem_total INTEGER NOT NULL DEFAULT 0;
ALTER TABLE node_samples ADD COLUMN gpu_temp REAL NOT NULL DEFAULT 0;
ALTER TABLE node_samples ADD COLUMN gpu_power REAL NOT NULL DEFAULT 0;

ALTER TABLE node_rollup ADD COLUMN gpu_util_sum REAL NOT NULL DEFAULT 0;
ALTER TABLE node_rollup ADD COLUMN gpu_util_max REAL NOT NULL DEFAULT 0;
ALTER TABLE node_rollup ADD COLUMN gpu_mem_used_max INTEGER NOT NULL DEFAULT 0;
ALTER TABLE node_rollup ADD COLUMN gpu_mem_total_max INTEGER NOT NULL DEFAULT 0;
ALTER TABLE node_rollup ADD COLUMN gpu_temp_max REAL NOT NULL DEFAULT 0;
ALTER TABLE node_rollup ADD COLUMN gpu_power_sum REAL NOT NULL DEFAULT 0;

-- The cards themselves, as JSON: index, name and total memory per device.
-- A fact about the host, updated with every report like the kernel version is,
-- so a card added or removed shows up without a separate enrolment step.
ALTER TABLE nodes ADD COLUMN gpus TEXT NOT NULL DEFAULT '';
