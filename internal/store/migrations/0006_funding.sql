-- Owner funding: money the owner puts into the business, and money paid back
-- out of it.
--
-- Until now the only ways money could arrive were an invoice payment and, at a
-- stretch, a hand-written journal entry. Neither describes how a business
-- actually starts: somebody puts their own money in before there is anything to
-- invoice. That event was possible to record only by knowing double entry well
-- enough to write it by hand, which is the one thing this module exists to
-- avoid asking.
--
-- The distinction that earns the table is capital versus loan. Both are the
-- owner moving money in, and they are not the same fact:
--
--   * Capital introduced is equity. The business does not owe it back. It sits
--     under 3000 Owner Capital and nets against drawings.
--   * A loan is a liability. The business owes it, and the balance of 2500 is
--     the answer to "how much am I still owed" — which is a question with tax
--     consequences, and one an equity account cannot answer.
--
-- Recording a loan as capital understates liabilities and overstates what the
-- owner has genuinely risked; recording capital as a loan invents a debt. So
-- the kind is stored per event rather than inferred from the account, and the
-- account each kind posts to follows from it.

-- ---------------------------------------------------------------------------
-- Widen the journal's source_type CHECK to admit 'funding'
-- ---------------------------------------------------------------------------
--
-- 0005 pinned source_type to a fixed list, and SQLite cannot alter a CHECK
-- constraint in place, so the table has to be rebuilt.
--
-- The obvious rebuild — copy aside, DROP TABLE, recreate — would destroy the
-- ledger. Migrations run inside a transaction with foreign_keys=ON, and under
-- foreign keys a DROP TABLE performs an implicit DELETE FROM first, which fires
-- the ON DELETE CASCADE on acct_journal_lines.entry_id. Every line of every
-- entry would go with it, the migration would report success, and the damage
-- would show up as a trial balance of zero. The documented way around that is
-- PRAGMA foreign_keys=OFF, which is a no-op inside a transaction and so is not
-- available here.
--
-- So both tables are rebuilt, ordered so that nothing ever references a table
-- at the moment it is dropped: rename the parent aside, build the new parent
-- and copy into it, rebuild the child pointing at the new parent, and only then
-- drop the two renamed originals — the child first, which is what leaves the
-- old parent unreferenced and safe to drop.
--
-- The copy is a full rewrite of the two largest tables. At a consultancy's
-- volumes that is a fraction of a second, and it happens once.

ALTER TABLE acct_journal_entries RENAME TO acct_journal_entries_old;

CREATE TABLE acct_journal_entries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_date  TEXT NOT NULL,
    memo        TEXT NOT NULL DEFAULT '',
    source_type TEXT NOT NULL DEFAULT 'manual'
                CHECK (source_type IN ('manual','opening','invoice','credit_note','payment','expense','funding')),
    source_id   INTEGER NOT NULL DEFAULT 0,
    -- A posted entry is never edited or deleted; it is reversed by another
    -- entry, and this records which. Same reasoning as invoice immutability.
    reverses_id INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT ''
);

INSERT INTO acct_journal_entries
    (id, entry_date, memo, source_type, source_id, reverses_id, created_at)
SELECT id, entry_date, memo, source_type, source_id, reverses_id, created_at
  FROM acct_journal_entries_old;

-- The child's foreign key followed the rename above and now points at
-- _old, so it has to be rebuilt too — otherwise new lines would attach
-- themselves to a table that is about to be dropped.
ALTER TABLE acct_journal_lines RENAME TO acct_journal_lines_old;

CREATE TABLE acct_journal_lines (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id     INTEGER NOT NULL REFERENCES acct_journal_entries(id) ON DELETE CASCADE,
    account_id   INTEGER NOT NULL REFERENCES acct_accounts(id) ON DELETE RESTRICT,
    debit_minor  INTEGER NOT NULL DEFAULT 0 CHECK (debit_minor >= 0),
    credit_minor INTEGER NOT NULL DEFAULT 0 CHECK (credit_minor >= 0),
    description  TEXT NOT NULL DEFAULT '',
    ordinal      INTEGER NOT NULL DEFAULT 0,
    CHECK ((debit_minor = 0) <> (credit_minor = 0))
);

INSERT INTO acct_journal_lines
    (id, entry_id, account_id, debit_minor, credit_minor, description, ordinal)
SELECT id, entry_id, account_id, debit_minor, credit_minor, description, ordinal
  FROM acct_journal_lines_old;

DROP TABLE acct_journal_lines_old;
DROP TABLE acct_journal_entries_old;

-- Indexes travelled with the renamed tables and were dropped along with them,
-- so they are recreated here rather than left to 0005, which will not run again.
CREATE INDEX IF NOT EXISTS idx_acct_entries_date ON acct_journal_entries(entry_date);
CREATE INDEX IF NOT EXISTS idx_acct_entries_source ON acct_journal_entries(source_type, source_id);
CREATE INDEX IF NOT EXISTS idx_acct_lines_entry ON acct_journal_lines(entry_id, ordinal);
CREATE INDEX IF NOT EXISTS idx_acct_lines_account ON acct_journal_lines(account_id);

-- ---------------------------------------------------------------------------
-- The funding record
-- ---------------------------------------------------------------------------

-- A repayment is the same table running backwards: money out, debited against
-- the loan. It is a kind rather than a negative amount because a negative
-- amount would defeat the CHECK that keeps a typo from posting a credit where a
-- debit belongs, and because "show me what was repaid" should not require
-- knowing the sign convention.
CREATE TABLE IF NOT EXISTS acct_funding (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    received_on   TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL CHECK (kind IN ('capital','loan','repayment')),
    amount_minor  INTEGER NOT NULL CHECK (amount_minor > 0),
    -- Who the money came from or went to. Free text: this module has no notion
    -- of a person, and the CRM next door describes customers, not owners.
    from_name     TEXT NOT NULL DEFAULT '',
    reference     TEXT NOT NULL DEFAULT '',
    note          TEXT NOT NULL DEFAULT '',
    -- The asset account the money moved through: into it for capital and loan,
    -- out of it for a repayment.
    cash_account_id  INTEGER NOT NULL REFERENCES acct_accounts(id) ON DELETE RESTRICT,
    -- The other side: the equity account for capital, the loan liability for a
    -- loan or a repayment. Stored rather than derived at read time, so a later
    -- change to which account a kind posts to cannot silently restate history.
    owner_account_id INTEGER NOT NULL REFERENCES acct_accounts(id) ON DELETE RESTRICT,
    journal_entry_id INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_acct_funding_date ON acct_funding(received_on);
CREATE INDEX IF NOT EXISTS idx_acct_funding_kind ON acct_funding(kind, received_on);

-- The liability side of an owner loan. The 0005 seed has equity accounts for
-- capital and drawings but nothing for money lent, so a loan had nowhere
-- correct to land — the reason this migration exists at all.
--
-- 2500 sits in the liabilities block between payroll (2400) and the tax
-- accounts, and carries a system key because the service has to find it to post
-- a loan without hardcoding the code, which the operator is free to renumber.
INSERT OR IGNORE INTO acct_accounts (code, name, type, system_key, description) VALUES
    ('2500', 'Loan from Owner', 'liability', 'owner_loan',
     'Money the owner lent the business rather than contributed. Repayable, so it is a liability and not equity; the balance is what is still owed.');

-- The equity side already exists — 0005 seeded 3000 Owner Capital — but with no
-- system key, so the service had no way to find it. Claim it here rather than
-- editing 0005, which has shipped.
--
-- Both conditions matter. `system_key = ''` leaves an operator who already
-- pointed the key somewhere else alone, and the NOT EXISTS keeps this from
-- failing against the partial unique index if they pointed it at another
-- account entirely. An install that has renamed or renumbered 3000 gets no
-- system account and a clear error when funding is first recorded, which is
-- better than this quietly seizing an account the operator repurposed.
UPDATE acct_accounts SET system_key = 'capital'
 WHERE code = '3000'
   AND type = 'equity'
   AND system_key = ''
   AND NOT EXISTS (SELECT 1 FROM acct_accounts WHERE system_key = 'capital');
