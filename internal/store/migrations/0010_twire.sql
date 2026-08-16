-- twire: the honeypot / canary tripwire moved here from morpheus. Fake
-- well-known services listen on their usual ports and record — and optionally
-- email-alert on — any connection attempt. The catalog of service profiles
-- lives in code (internal/twire/catalog.go); these tables hold runtime state.
--
-- As with the fintech tables in 0008, there are no owner columns: morpheus
-- had signed-in users, wintermute authenticates registered clients by bearer
-- token, and twire was already a global resource in morpheus anyway — every
-- user there saw and managed the same canaries.
--
-- Two shapes differ from the Postgres original because SQLite is not Postgres:
--
--   * Booleans are INTEGER 0/1, and timestamps are TEXT in RFC 3339 written by
--     the repository, matching the fintech and workspace tables next door.
--     Nothing here uses now(); SQLite's CURRENT_TIMESTAMP has a different
--     format from the one Go parses back.
--   * The encrypted SMTP password is BLOB rather than BYTEA.
--
-- The smtp_host and smtp_port columns the original carried are absent: Google
-- SMTP (smtp.gmail.com:587) has been hard-coded in the code for longer than
-- those columns have been read, and this is a fresh table with no legacy rows
-- to keep consistent.

-- twire_canaries: which profiles (by stable key, built-in or custom) are
-- enabled. A profile absent from this table defaults to disabled.
CREATE TABLE IF NOT EXISTS twire_canaries (
    profile_key TEXT PRIMARY KEY,
    enabled     INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT ''
);

-- twire_events: one row per connection attempt against a canary.
CREATE TABLE IF NOT EXISTS twire_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    profile_key  TEXT NOT NULL,
    service_name TEXT NOT NULL,
    port         INTEGER NOT NULL,
    remote_ip    TEXT NOT NULL,
    remote_port  INTEGER NOT NULL,
    data_preview TEXT NOT NULL DEFAULT '',
    occurred_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_twire_events_occurred_at ON twire_events (occurred_at DESC);

-- twire_alert_config: singleton (id = 1) SMTP/email alert configuration. The
-- SMTP password is stored AES-256-GCM encrypted under a key derived from
-- WINTERMUTE_SECRET (see internal/twire/crypto.go), never in plaintext;
-- recipients is a comma-separated address list. A saved row overrides the
-- SMTP_* / TWIRE_ALERT_TO environment defaults.
--
-- This is the one place wintermute stores a secret at rest, and it is why
-- WINTERMUTE_SECRET exists. Without that variable set the rest of twire still
-- works — canaries listen and hits are recorded — and only saving a password
-- is refused, rather than the password being written somewhere unprotected.
CREATE TABLE IF NOT EXISTS twire_alert_config (
    id                  INTEGER PRIMARY KEY CHECK (id = 1),
    enabled             INTEGER NOT NULL DEFAULT 0,
    smtp_username       TEXT NOT NULL DEFAULT '',
    smtp_password_enc   BLOB,
    smtp_password_nonce BLOB,
    smtp_from           TEXT NOT NULL DEFAULT '',
    recipients          TEXT NOT NULL DEFAULT '',
    updated_at          TEXT NOT NULL DEFAULT ''
);

-- twire_custom_canaries: operator-defined fake services on arbitrary ports,
-- complementing the built-in code catalog.
--
-- The profile_key is generated as "custom-<port>" so it never collides with a
-- built-in key. Enabled-state and recorded events still live in twire_canaries
-- / twire_events keyed by that profile_key, so the enable/disable/status/event
-- paths work unchanged for custom canaries. Deleting a row here is paired (in
-- code) with clearing its twire_canaries enabled row; historical twire_events
-- are intentionally left as a log.
CREATE TABLE IF NOT EXISTS twire_custom_canaries (
    profile_key TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    port        INTEGER NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    banner      TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT ''
);
