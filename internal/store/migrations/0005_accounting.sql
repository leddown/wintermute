-- Accounting: a double-entry general ledger, invoicing with EU VAT, payments,
-- and expenses, for a consultancy of a handful of people.
--
-- Three decisions run through the whole schema and are worth stating once.
--
-- 1. Money is INTEGER minor units — cents — everywhere, never REAL. The CRM
--    tables next door store rates and hours as REAL, which is fine for "how
--    much is roughly outstanding" and wrong here: a ledger's whole claim is
--    that debits equal credits exactly, and binary floating point cannot make
--    that promise across a few thousand rows. Conversion happens once, at the
--    CRM boundary, in Go.
--
-- 2. Issued invoices are immutable. EU VAT requires invoice numbers to be
--    unique, sequential and gap-free, and an issued invoice may not be deleted
--    or amended — a correction is a separate credit note that references it.
--    So `number` is assigned at issue rather than creation, drafts carry '',
--    and nothing in the API rewrites a row once `status` leaves 'draft'.
--
-- 3. Every balance comes from the ledger. Invoices, payments and expenses are
--    *sources*: each posts a balanced journal entry and stores its id. No
--    reader sums an invoice table to learn what revenue was, and no writer
--    adjusts a balance directly. This is the invariant the module exists to
--    protect, so it is enforced in the service layer and, where SQLite can, by
--    CHECK constraints here.

-- ---------------------------------------------------------------------------
-- Chart of accounts
-- ---------------------------------------------------------------------------

-- `type` fixes the normal balance: asset and expense are debit-normal,
-- liability, equity and income are credit-normal. Reports derive sign from it
-- rather than storing one.
--
-- `system_key` names the handful of accounts the code must be able to find
-- without hardcoding a code number the operator is free to renumber — accounts
-- receivable, the VAT control pair, the default sales account. Everything else
-- is the operator's to shape.
CREATE TABLE IF NOT EXISTS acct_accounts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    code       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    type       TEXT NOT NULL CHECK (type IN ('asset','liability','equity','income','expense')),
    parent_id  INTEGER REFERENCES acct_accounts(id) ON DELETE RESTRICT,
    system_key TEXT NOT NULL DEFAULT '',
    -- VAT treatment applied by default when this account is used on an expense.
    default_vat_rate_id INTEGER,
    description TEXT NOT NULL DEFAULT '',
    archived   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT ''
);

-- A system key names exactly one account, but most accounts have none.
CREATE UNIQUE INDEX IF NOT EXISTS idx_acct_accounts_system
    ON acct_accounts(system_key) WHERE system_key <> '';
CREATE INDEX IF NOT EXISTS idx_acct_accounts_type ON acct_accounts(type, code);

-- ---------------------------------------------------------------------------
-- VAT
-- ---------------------------------------------------------------------------

-- Rates are basis points: 2100 = 21%. Integer arithmetic again, for the same
-- reason as everything else here.
--
-- `kind` distinguishes cases that share a 0% rate but are not the same thing on
-- a VAT return: genuinely zero-rated supplies, exempt supplies, and reverse
-- charge where the customer accounts for the VAT. Collapsing them loses the
-- distinction the return depends on.
CREATE TABLE IF NOT EXISTS acct_vat_rates (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    code     TEXT NOT NULL UNIQUE,
    name     TEXT NOT NULL,
    rate_bp  INTEGER NOT NULL DEFAULT 0 CHECK (rate_bp >= 0),
    kind     TEXT NOT NULL DEFAULT 'standard'
             CHECK (kind IN ('standard','reduced','zero','exempt','reverse_charge')),
    archived INTEGER NOT NULL DEFAULT 0
);

-- ---------------------------------------------------------------------------
-- The ledger
-- ---------------------------------------------------------------------------

-- `source_type`/`source_id` point back at whatever caused the entry, so a
-- posting is always traceable to the invoice or expense that produced it, and
-- so a source can find and reverse its own entry without a second index.
CREATE TABLE IF NOT EXISTS acct_journal_entries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_date  TEXT NOT NULL,
    memo        TEXT NOT NULL DEFAULT '',
    source_type TEXT NOT NULL DEFAULT 'manual'
                CHECK (source_type IN ('manual','opening','invoice','credit_note','payment','expense')),
    source_id   INTEGER NOT NULL DEFAULT 0,
    -- A posted entry is never edited or deleted; it is reversed by another
    -- entry, and this records which. Same reasoning as invoice immutability.
    reverses_id INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_acct_entries_date ON acct_journal_entries(entry_date);
CREATE INDEX IF NOT EXISTS idx_acct_entries_source ON acct_journal_entries(source_type, source_id);

-- One line, one side. Storing debit and credit as separate non-negative columns
-- rather than a single signed amount keeps the reports readable and makes the
-- "exactly one side" rule expressible as a CHECK: a line with both sides set,
-- or neither, is a bug, and the database refuses it rather than quietly
-- contributing zero to a trial balance that then looks fine.
CREATE TABLE IF NOT EXISTS acct_journal_lines (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id     INTEGER NOT NULL REFERENCES acct_journal_entries(id) ON DELETE CASCADE,
    account_id   INTEGER NOT NULL REFERENCES acct_accounts(id) ON DELETE RESTRICT,
    debit_minor  INTEGER NOT NULL DEFAULT 0 CHECK (debit_minor >= 0),
    credit_minor INTEGER NOT NULL DEFAULT 0 CHECK (credit_minor >= 0),
    description  TEXT NOT NULL DEFAULT '',
    ordinal      INTEGER NOT NULL DEFAULT 0,
    CHECK ((debit_minor = 0) <> (credit_minor = 0))
);

CREATE INDEX IF NOT EXISTS idx_acct_lines_entry ON acct_journal_lines(entry_id, ordinal);
CREATE INDEX IF NOT EXISTS idx_acct_lines_account ON acct_journal_lines(account_id);

-- ---------------------------------------------------------------------------
-- Numbering
-- ---------------------------------------------------------------------------

-- Gap-free sequential numbering is a legal requirement, not a preference, so
-- the next number lives in a row that is read and bumped inside the same
-- transaction that issues the document. AUTOINCREMENT would not do: it leaves
-- gaps when a transaction rolls back, and a gap in an invoice sequence is
-- exactly what an auditor asks about.
CREATE TABLE IF NOT EXISTS acct_sequences (
    name       TEXT PRIMARY KEY,
    prefix     TEXT NOT NULL DEFAULT '',
    next_value INTEGER NOT NULL DEFAULT 1 CHECK (next_value >= 1),
    padding    INTEGER NOT NULL DEFAULT 4 CHECK (padding >= 0)
);

-- ---------------------------------------------------------------------------
-- Invoices
-- ---------------------------------------------------------------------------

-- The counterparty is a CRM client. There is deliberately no separate customer
-- table: the CRM already knows who the work is for, and a second list of the
-- same companies would drift apart within a quarter.
--
-- ON DELETE RESTRICT rather than CASCADE: a client with invoices against them
-- cannot be deleted. Losing an issued invoice because someone tidied the client
-- list would destroy the sequence and the audit trail.
--
-- Totals are stored rather than derived. They are the figures that appeared on
-- a document that has left the building; recomputing them later from lines and
-- current VAT rates would let a rate change rewrite history.
CREATE TABLE IF NOT EXISTS acct_invoices (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT NOT NULL DEFAULT 'invoice' CHECK (kind IN ('invoice','credit_note')),
    -- Assigned at issue, empty while draft.
    number        TEXT NOT NULL DEFAULT '',
    client_id     INTEGER NOT NULL REFERENCES crm_clients(id) ON DELETE RESTRICT,
    -- Optional: an invoice may span engagements or none at all.
    engagement_id INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft','issued','part_paid','paid','void')),
    issue_date    TEXT NOT NULL DEFAULT '',
    due_date      TEXT NOT NULL DEFAULT '',
    terms_days    INTEGER NOT NULL DEFAULT 14,

    subtotal_minor INTEGER NOT NULL DEFAULT 0,
    vat_minor      INTEGER NOT NULL DEFAULT 0,
    total_minor    INTEGER NOT NULL DEFAULT 0,
    -- Sum of payments applied. Denormalised so an AR aging report does not need
    -- a correlated subquery per row; the service keeps it in step.
    paid_minor     INTEGER NOT NULL DEFAULT 0,

    -- Cross-border EU B2B: no VAT charged, customer accounts for it. The
    -- customer's VAT number must appear on the document, so it is snapshotted
    -- here rather than read from the client record at print time.
    reverse_charge      INTEGER NOT NULL DEFAULT 0,
    customer_vat_number TEXT NOT NULL DEFAULT '',

    -- Snapshot of who the invoice was addressed to, taken at issue. The client
    -- record is free to change afterwards; the document is not.
    bill_to        TEXT NOT NULL DEFAULT '',
    notes          TEXT NOT NULL DEFAULT '',
    -- For a credit note: the invoice it corrects.
    corrects_id    INTEGER NOT NULL DEFAULT 0,
    journal_entry_id INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL DEFAULT '',
    issued_at      TEXT NOT NULL DEFAULT ''
);

-- Unique across issued documents only; every draft carries ''.
CREATE UNIQUE INDEX IF NOT EXISTS idx_acct_invoices_number
    ON acct_invoices(number) WHERE number <> '';
CREATE INDEX IF NOT EXISTS idx_acct_invoices_client ON acct_invoices(client_id, status);
CREATE INDEX IF NOT EXISTS idx_acct_invoices_status ON acct_invoices(status, due_date);

-- quantity_milli is thousandths — 1.5 hours is 1500 — so an hours figure that
-- came from a REAL column in the CRM lands on an exact integer before it is
-- multiplied by a price.
--
-- vat_rate_bp is snapshotted alongside vat_rate_id for the same reason the CRM
-- snapshots hourly rates on a time entry: editing the rate table next year must
-- not restate last year's invoice.
CREATE TABLE IF NOT EXISTS acct_invoice_lines (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    invoice_id       INTEGER NOT NULL REFERENCES acct_invoices(id) ON DELETE CASCADE,
    description      TEXT NOT NULL DEFAULT '',
    quantity_milli   INTEGER NOT NULL DEFAULT 1000,
    unit_price_minor INTEGER NOT NULL DEFAULT 0,
    net_minor        INTEGER NOT NULL DEFAULT 0,
    vat_rate_id      INTEGER REFERENCES acct_vat_rates(id) ON DELETE RESTRICT,
    vat_rate_bp      INTEGER NOT NULL DEFAULT 0,
    vat_minor        INTEGER NOT NULL DEFAULT 0,
    income_account_id INTEGER NOT NULL REFERENCES acct_accounts(id) ON DELETE RESTRICT,
    -- Provenance: which time entry or expense became this line. Nullable in
    -- spirit (0 = none) so a hand-written line is equally valid.
    time_entry_id    INTEGER NOT NULL DEFAULT 0,
    expense_id       INTEGER NOT NULL DEFAULT 0,
    ordinal          INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_acct_invoice_lines_invoice ON acct_invoice_lines(invoice_id, ordinal);
-- "Has this time entry already been billed?" must not scan every line ever.
CREATE INDEX IF NOT EXISTS idx_acct_invoice_lines_time
    ON acct_invoice_lines(time_entry_id) WHERE time_entry_id <> 0;

-- ---------------------------------------------------------------------------
-- Payments
-- ---------------------------------------------------------------------------

-- Partial payments are the norm, so a payment is a row against an invoice
-- rather than a flag on it. RESTRICT on the invoice: a paid invoice cannot be
-- removed out from under its receipts.
CREATE TABLE IF NOT EXISTS acct_payments (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    invoice_id      INTEGER NOT NULL REFERENCES acct_invoices(id) ON DELETE RESTRICT,
    paid_on         TEXT NOT NULL DEFAULT '',
    amount_minor    INTEGER NOT NULL CHECK (amount_minor > 0),
    method          TEXT NOT NULL DEFAULT 'bank',
    reference       TEXT NOT NULL DEFAULT '',
    -- Which asset account received it.
    deposit_account_id INTEGER NOT NULL REFERENCES acct_accounts(id) ON DELETE RESTRICT,
    journal_entry_id INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_acct_payments_invoice ON acct_payments(invoice_id);
CREATE INDEX IF NOT EXISTS idx_acct_payments_date ON acct_payments(paid_on);

-- ---------------------------------------------------------------------------
-- Expenses
-- ---------------------------------------------------------------------------

-- An expense is money out with a category (the expense account it lands in) and
-- a source (the bank or card it left). Recoverable input VAT is split out so it
-- reaches the VAT control account rather than inflating the cost.
--
-- A billable expense can be rebilled to a client, which is why it carries the
-- CRM linkage and the invoice it ended up on.
CREATE TABLE IF NOT EXISTS acct_expenses (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    spent_on       TEXT NOT NULL DEFAULT '',
    vendor         TEXT NOT NULL DEFAULT '',
    description    TEXT NOT NULL DEFAULT '',
    account_id     INTEGER NOT NULL REFERENCES acct_accounts(id) ON DELETE RESTRICT,
    paid_from_id   INTEGER NOT NULL REFERENCES acct_accounts(id) ON DELETE RESTRICT,
    net_minor      INTEGER NOT NULL DEFAULT 0 CHECK (net_minor >= 0),
    vat_rate_id    INTEGER REFERENCES acct_vat_rates(id) ON DELETE RESTRICT,
    vat_rate_bp    INTEGER NOT NULL DEFAULT 0,
    vat_minor      INTEGER NOT NULL DEFAULT 0 CHECK (vat_minor >= 0),
    total_minor    INTEGER NOT NULL DEFAULT 0 CHECK (total_minor >= 0),
    -- Not all input VAT is recoverable (entertainment, private use). When this
    -- is 0 the VAT is posted to the expense account instead of VAT control.
    vat_reclaimable INTEGER NOT NULL DEFAULT 1,
    billable       INTEGER NOT NULL DEFAULT 0,
    client_id      INTEGER NOT NULL DEFAULT 0,
    engagement_id  INTEGER NOT NULL DEFAULT 0,
    rebilled_invoice_id INTEGER NOT NULL DEFAULT 0,
    receipt_note   TEXT NOT NULL DEFAULT '',
    journal_entry_id INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_acct_expenses_date ON acct_expenses(spent_on);
CREATE INDEX IF NOT EXISTS idx_acct_expenses_account ON acct_expenses(account_id);
-- The rebilling screen asks only this question.
CREATE INDEX IF NOT EXISTS idx_acct_expenses_billable
    ON acct_expenses(client_id, rebilled_invoice_id) WHERE billable = 1;

-- ---------------------------------------------------------------------------
-- Period locking
-- ---------------------------------------------------------------------------

-- Once a VAT return is filed or a year is signed off, nothing may be posted
-- into that window. A locked period is checked on every write that carries a
-- date, including backdated ones — which is the case that actually matters.
CREATE TABLE IF NOT EXISTS acct_periods (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    starts_on TEXT NOT NULL,
    ends_on   TEXT NOT NULL,
    locked    INTEGER NOT NULL DEFAULT 0,
    locked_at TEXT NOT NULL DEFAULT '',
    note      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_acct_periods_range ON acct_periods(starts_on, ends_on);

-- ---------------------------------------------------------------------------
-- Settings
-- ---------------------------------------------------------------------------

-- One row, like company_profile. Single currency by design: no FX rates, no
-- revaluation, no realised/unrealised gain accounts.
CREATE TABLE IF NOT EXISTS acct_settings (
    id                  INTEGER PRIMARY KEY CHECK (id = 1),
    currency            TEXT NOT NULL DEFAULT 'EUR',
    -- Symbol and decimals are presentation only; the ledger is always minor units.
    currency_symbol     TEXT NOT NULL DEFAULT '€',
    default_terms_days  INTEGER NOT NULL DEFAULT 14,
    default_vat_rate_id INTEGER REFERENCES acct_vat_rates(id),
    fiscal_year_start_month INTEGER NOT NULL DEFAULT 1
                        CHECK (fiscal_year_start_month BETWEEN 1 AND 12),
    updated_at          TEXT NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- Seed data
-- ---------------------------------------------------------------------------
-- INSERT OR IGNORE against UNIQUE keys: re-running the migration leaves an
-- edited chart of accounts alone rather than resurrecting deleted rows or
-- overwriting renamed ones.

-- VAT rates. The percentages here are a starting point, not advice: standard
-- rates differ by member state and the operator must set their own. The three
-- 0% rows are distinct on purpose — see the `kind` note above.
INSERT OR IGNORE INTO acct_vat_rates (code, name, rate_bp, kind) VALUES
    ('STD', 'Standard rate',            2100, 'standard'),
    ('RED', 'Reduced rate',              900, 'reduced'),
    ('ZERO', 'Zero-rated',                 0, 'zero'),
    ('EXEMPT', 'Exempt',                   0, 'exempt'),
    ('RC', 'Reverse charge (EU B2B)',      0, 'reverse_charge');

-- Chart of accounts: around 35 accounts, which is the range small-business
-- guidance converges on. Enough to file a return and read a P&L; not the
-- several hundred a full ERP template ships with, most of which would sit at
-- zero forever in a consultancy.
INSERT OR IGNORE INTO acct_accounts (code, name, type, system_key, description) VALUES
    -- Assets
    ('1000', 'Business Bank Account',     'asset',     'bank',       'Primary current account.'),
    ('1010', 'Petty Cash',                'asset',     '',           'Cash on hand.'),
    ('1020', 'Savings Account',           'asset',     '',           ''),
    ('1200', 'Accounts Receivable',       'asset',     'ar',         'Issued invoices not yet paid.'),
    ('1240', 'Prepaid Expenses',          'asset',     '',           'Costs paid in advance of the period they belong to.'),
    ('1500', 'Computer Equipment',        'asset',     '',           'Capitalised hardware.'),
    ('1510', 'Office Furniture',          'asset',     '',           ''),
    ('1590', 'Accumulated Depreciation',  'asset',     '',           'Contra-asset; carries a credit balance.'),

    -- Liabilities
    ('2000', 'Accounts Payable',          'liability', 'ap',         'Supplier bills not yet paid.'),
    ('2100', 'VAT on Sales',              'liability', 'vat_output', 'Output VAT charged to customers and owed to the authority.'),
    ('2110', 'VAT on Purchases',          'liability', 'vat_input',  'Input VAT paid to suppliers and reclaimable. Debit-balanced within a credit-normal account.'),
    ('2200', 'Credit Card',               'liability', '',           ''),
    ('2300', 'Corporation Tax Payable',   'liability', '',           ''),
    ('2400', 'Payroll Liabilities',       'liability', '',           'Wages, social contributions and withholdings due.'),

    -- Equity
    ('3000', 'Owner Capital',             'equity',    '',           'Contributed capital.'),
    ('3100', 'Owner Drawings',            'equity',    '',           'Distributions taken.'),
    ('3900', 'Retained Earnings',         'equity',    'retained',   'Accumulated result of prior periods.'),

    -- Income
    ('4000', 'Consulting Fees',           'income',    'sales',      'Billable professional services. Default for invoice lines.'),
    ('4100', 'Recharged Expenses',        'income',    'recharged',  'Client-billable costs passed through at cost.'),
    ('4900', 'Other Income',              'income',    '',           ''),

    -- Cost of sales
    ('5000', 'Subcontractors',            'expense',   '',           'Associates and contract delivery staff.'),
    ('5100', 'Direct Project Costs',      'expense',   '',           'Costs incurred specifically for an engagement.'),

    -- Operating expenses
    ('6000', 'Salaries',                  'expense',   '',           ''),
    ('6010', 'Employer Contributions',    'expense',   '',           'Employer-side social security and pension.'),
    ('6100', 'Rent',                      'expense',   '',           ''),
    ('6110', 'Utilities',                 'expense',   '',           ''),
    ('6200', 'Software and Subscriptions','expense',   '',           'SaaS, licences, hosting.'),
    ('6210', 'Equipment (expensed)',      'expense',   '',           'Hardware below the capitalisation threshold.'),
    ('6300', 'Professional Fees',         'expense',   '',           'Accountancy, legal, advisory.'),
    ('6310', 'Insurance',                 'expense',   '',           'Professional indemnity and general.'),
    ('6400', 'Travel',                    'expense',   '',           ''),
    ('6410', 'Meals and Entertainment',   'expense',   '',           'Input VAT is often irrecoverable here — see vat_reclaimable.'),
    ('6500', 'Marketing',                 'expense',   '',           ''),
    ('6600', 'Telephone and Internet',    'expense',   '',           ''),
    ('6700', 'Bank Charges',              'expense',   '',           ''),
    ('6800', 'Training',                  'expense',   '',           ''),
    ('6900', 'Office Supplies',           'expense',   '',           ''),
    ('6950', 'Depreciation',              'expense',   '',           ''),
    -- Integer VAT arithmetic on many lines can leave a unit or two that has to
    -- land somewhere for the entry to balance. Without a named home it would be
    -- silently absorbed into revenue.
    ('6990', 'Rounding Differences',      'expense',   'rounding',   'Sub-cent residue from VAT rounding across invoice lines.');

INSERT OR IGNORE INTO acct_sequences (name, prefix, next_value, padding) VALUES
    ('invoice',     'INV-', 1, 4),
    ('credit_note', 'CN-',  1, 4),
    ('journal',     'JNL-', 1, 4);

INSERT OR IGNORE INTO acct_settings (id, currency, currency_symbol, default_terms_days, fiscal_year_start_month)
    VALUES (1, 'EUR', '€', 14, 1);

-- Point settings and the two VAT-bearing defaults at the standard rate now that
-- both rows exist. Guarded so a later edit is not undone on the next start.
UPDATE acct_settings
   SET default_vat_rate_id = (SELECT id FROM acct_vat_rates WHERE code = 'STD')
 WHERE id = 1 AND default_vat_rate_id IS NULL;
