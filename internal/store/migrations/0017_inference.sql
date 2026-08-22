-- What the models actually did, one row per completed call.
--
-- Every figure comes from a call the server already made, so gathering it costs
-- a clock reading. It answers the question the model registry cannot: which
-- model is genuinely quicker on this hardware, at the context lengths and under
-- the concurrency it is really used at, rather than what a benchmark said once.
--
-- Volume is modest compared with host telemetry — one row per model call rather
-- than one per host every few seconds — so raw rows are worth keeping long
-- enough to answer "why was that turn slow". They are not worth keeping
-- forever, and the shape here is chosen so the rollup job in the fleet work can
-- fold them without changing anything:
--
--   * facts are additive. Durations and token counts are summed and counted,
--     never averaged into the row, because an average cannot be re-aggregated
--     into a coarser bucket without lying about it.
--   * the index is on time alone, so ageing rows out is a range delete rather
--     than a search, and so a dashboard reading a window never scans the table.
CREATE TABLE IF NOT EXISTS inference_samples (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    backend           TEXT NOT NULL,
    model             TEXT NOT NULL,
    -- Token counts as the provider reported them. Zero means "not reported":
    -- several OpenAI-compatible servers omit usage entirely, and a rate
    -- derived from those rows would be a fabrication rather than a slow model.
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    -- Wall time of the whole call: queueing, prompt processing and generation
    -- together. Not time-to-first-token, which needs streaming and which
    -- neither provider here currently requests.
    duration_ms       INTEGER NOT NULL DEFAULT 0,
    -- A failed call is kept rather than dropped. A backend that fails in two
    -- seconds would otherwise look faster than one that succeeds in twenty.
    failed            INTEGER NOT NULL DEFAULT 0,
    -- Set when this call was served by the fallback after the intended backend
    -- failed — the flag that explains traffic on a cloud backend nobody chose.
    fell_back         INTEGER NOT NULL DEFAULT 0,
    created_at        TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_inference_created ON inference_samples(created_at);

-- The dashboard groups by backend and model over a window, which this covers
-- without touching the table itself.
CREATE INDEX IF NOT EXISTS idx_inference_model
    ON inference_samples(backend, model, created_at);
