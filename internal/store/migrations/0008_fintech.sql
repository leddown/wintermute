-- Fintech: the investment ledger moved here from morpheus — instruments,
-- transactions, derived holdings, AI price forecasts scored against what
-- actually happened, and the periodic position review.
--
-- Owner columns are absent, for the reason 0004_workspace.sql gives: morpheus
-- scoped every one of these tables to a signed-in user, and wintermute
-- authenticates registered clients by bearer token instead. One install, one
-- portfolio. The `user_id` that keyed transactions, forecasts, the watchlist
-- and the reviews would be a constant here, and a uniqueness constraint on
-- (user_id, dedupe_hash) would be a constraint on the hash alone — so that is
-- what it now is.
--
-- Two shapes differ from the Postgres original because SQLite is not Postgres:
--
--   * Quantities stay TEXT, and are summed in Go with math/big rather than by
--     the database. Postgres had NUMERIC(28,10) and could add fractions of a
--     bitcoin exactly in SQL; SQLite would coerce them to float64 and lose the
--     eighth decimal on the way. Prices, fees and totals were always integer
--     cents and stay INTEGER, which is exact everywhere.
--   * Timestamps are TEXT in RFC 3339, written by the service, matching the
--     workspace tables next door.
--
-- The market-data and broker API keys have no table here. Morpheus stored them
-- AES-encrypted in the database because it had a key to encrypt them with (its
-- JWT secret) and a settings page to type them into. Wintermute takes its
-- secrets from the environment, as it does for the GRC and SearXNG
-- integrations, so the keys live there and nothing has to be encrypted at rest
-- by an application that has nowhere safe to keep the key.

-- A tradable symbol. Shared by everything below, and the one table that is
-- purely reference data.
CREATE TABLE IF NOT EXISTS fintech_instruments (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    symbol      TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL DEFAULT '',
    asset_class TEXT NOT NULL CHECK (asset_class IN ('equity', 'etf', 'crypto')),
    created_at  TEXT NOT NULL DEFAULT ''
);

-- The single source of truth. Holdings, cost basis and realised P&L are always
-- derived from this table on read, never stored as a running balance that
-- something else has to remember to update.
--
-- dedupe_hash is what makes a re-imported CSV or a re-run Kraken sync a no-op
-- rather than a doubled position, and it is unique on its own now that there is
-- one portfolio.
CREATE TABLE IF NOT EXISTS fintech_transactions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    instrument_id   INTEGER NOT NULL REFERENCES fintech_instruments(id),
    side            TEXT NOT NULL CHECK (side IN ('buy', 'sell')),
    quantity        TEXT NOT NULL,
    price_cents     INTEGER NOT NULL CHECK (price_cents >= 0),
    fee_cents       INTEGER NOT NULL DEFAULT 0 CHECK (fee_cents >= 0),
    total_cents     INTEGER NOT NULL,
    source          TEXT NOT NULL CHECK (source IN ('manual', 'csv_import', 'paper', 'kraken_sync')),
    executed_at     TEXT NOT NULL,
    broker_order_id TEXT NOT NULL DEFAULT '',
    external_id     TEXT NOT NULL DEFAULT '',
    dedupe_hash     TEXT NOT NULL UNIQUE,
    notes           TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_fintech_transactions_instrument
    ON fintech_transactions (instrument_id);
CREATE INDEX IF NOT EXISTS idx_fintech_transactions_executed
    ON fintech_transactions (executed_at DESC);

-- What a CSV import brought in, kept so a bad import can be recognised after
-- the fact.
CREATE TABLE IF NOT EXISTS fintech_import_batches (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    filename    TEXT NOT NULL,
    row_count   INTEGER NOT NULL DEFAULT 0,
    imported_at TEXT NOT NULL DEFAULT ''
);

-- The remembered column mapping for CSV imports. One row, because there is one
-- portfolio and one broker export format anyone reuses.
CREATE TABLE IF NOT EXISTS fintech_import_mapping (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    date_column     TEXT NOT NULL DEFAULT '',
    symbol_column   TEXT NOT NULL DEFAULT '',
    side_column     TEXT NOT NULL DEFAULT '',
    quantity_column TEXT NOT NULL DEFAULT '',
    price_column    TEXT NOT NULL DEFAULT '',
    fee_column      TEXT NOT NULL DEFAULT '',
    date_format     TEXT NOT NULL DEFAULT '',
    has_header      INTEGER NOT NULL DEFAULT 1,
    updated_at      TEXT NOT NULL DEFAULT ''
);

-- One row per forecast request: a symbol, the price it was asked about, and
-- the model that answered.
CREATE TABLE IF NOT EXISTS fintech_forecasts (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    instrument_id         INTEGER NOT NULL REFERENCES fintech_instruments(id),
    requested_at          TEXT NOT NULL DEFAULT '',
    reference_price_cents INTEGER NOT NULL,
    model_name            TEXT NOT NULL DEFAULT '',
    rationale             TEXT NOT NULL DEFAULT '',
    -- The deep-dive enrichment, as the JSON the model returned. Read whole,
    -- never queried into, so it is a document rather than columns.
    enrichment            TEXT NOT NULL DEFAULT '',
    enriched_at           TEXT NOT NULL DEFAULT '',
    created_at            TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_fintech_forecasts_requested
    ON fintech_forecasts (requested_at DESC);

-- One row per horizon: a {3,5,10}-day forecast writes three. The actual_*
-- columns stay NULL until the target date has passed and the outcome is
-- scored, so this row is both the prediction and, later, the mark against it.
CREATE TABLE IF NOT EXISTS fintech_forecast_horizons (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    forecast_id            INTEGER NOT NULL REFERENCES fintech_forecasts(id) ON DELETE CASCADE,
    horizon_days           INTEGER NOT NULL CHECK (horizon_days IN (3, 5, 10, 14, 21, 30, 60, 90)),
    target_date            TEXT NOT NULL,
    predicted_direction    TEXT NOT NULL CHECK (predicted_direction IN ('up', 'down', 'flat')),
    predicted_low_cents    INTEGER NOT NULL,
    predicted_high_cents   INTEGER NOT NULL,
    confidence             REAL NOT NULL CHECK (confidence BETWEEN 0 AND 1),
    actual_price_cents     INTEGER,
    actual_direction       TEXT CHECK (actual_direction IS NULL OR actual_direction IN ('up', 'down', 'flat')),
    within_predicted_range INTEGER,
    evaluated_at           TEXT,
    UNIQUE (forecast_id, horizon_days)
);

-- The index the evaluation sweep runs on: everything predicted and not yet
-- scored, ordered by when it came due.
CREATE INDEX IF NOT EXISTS idx_fintech_forecast_horizons_due
    ON fintech_forecast_horizons (target_date) WHERE evaluated_at IS NULL;

-- Symbols the scheduler forecasts and scores on its own. horizons is a
-- comma-separated subset of the day counts above, validated in the service.
CREATE TABLE IF NOT EXISTS fintech_watchlist (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    instrument_id    INTEGER NOT NULL UNIQUE REFERENCES fintech_instruments(id),
    horizons         TEXT NOT NULL,
    enabled          INTEGER NOT NULL DEFAULT 1,
    last_forecast_at TEXT,
    created_at       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_fintech_watchlist_enabled
    ON fintech_watchlist (enabled) WHERE enabled = 1;

-- One row per symbol per review cycle. symbol and rating are copied rather
-- than joined: a review is a point-in-time record and has to still read
-- correctly after the forecast behind it is gone, which is why forecast_id is
-- nullable and cleared rather than cascaded.
--
-- reported_at IS NULL is the carry-over queue. A cycle that runs inside the
-- quiet window still writes its rows, and the next send outside the window
-- picks up everything still unreported — in the database rather than in memory,
-- so a restart never loses a pending digest.
CREATE TABLE IF NOT EXISTS fintech_reviews (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    instrument_id INTEGER NOT NULL REFERENCES fintech_instruments(id),
    forecast_id   INTEGER REFERENCES fintech_forecasts(id) ON DELETE SET NULL,
    symbol        TEXT NOT NULL,
    source        TEXT NOT NULL CHECK (source IN ('watchlist', 'holding')),
    rating        TEXT NOT NULL CHECK (rating IN ('max_sell', 'sell', 'hold', 'buy', 'max_buy')),
    rationale     TEXT NOT NULL DEFAULT '',
    reviewed_at   TEXT NOT NULL DEFAULT '',
    reported_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_fintech_reviews_unreported
    ON fintech_reviews (reviewed_at) WHERE reported_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_fintech_reviews_reviewed
    ON fintech_reviews (reviewed_at DESC);

-- Settings, one row each. No secrets in either, so nothing here is encrypted.
CREATE TABLE IF NOT EXISTS fintech_forecast_prompt (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    prompt     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS fintech_review_config (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    alert_enabled INTEGER NOT NULL DEFAULT 1,
    quiet_enabled INTEGER NOT NULL DEFAULT 1,
    quiet_start   TEXT NOT NULL DEFAULT '22:00',
    quiet_end     TEXT NOT NULL DEFAULT '07:00',
    timezone      TEXT NOT NULL DEFAULT '',
    updated_at    TEXT NOT NULL DEFAULT ''
);

-- What the forecasting and review calls cost. Kept because a local model is
-- free and a cloud one is not, and the difference is worth being able to see.
CREATE TABLE IF NOT EXISTS fintech_ai_usage (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT NOT NULL CHECK (kind IN ('forecast', 'enrichment', 'review')),
    backend       TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_fintech_ai_usage_created
    ON fintech_ai_usage (created_at);
