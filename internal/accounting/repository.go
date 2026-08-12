package accounting

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Repository is the accounting persistence boundary. It matches the CRM
// module's shape — an interface over a SQLite implementation, no context
// plumbing — so the two workspace modules read the same way.
type Repository interface {
	ListAccounts(includeArchived bool) ([]Account, error)
	GetAccount(id int64) (Account, error)
	AccountBySystemKey(key string) (Account, error)
	CreateAccount(a Account) (Account, error)
	UpdateAccount(id int64, a Account) (Account, error)
	SetAccountArchived(id int64, archived bool) error
	AccountHasPostings(id int64) (bool, error)

	ListVATRates(includeArchived bool) ([]VATRate, error)
	GetVATRate(id int64) (VATRate, error)
	VATRateByKind(kind VATKind) (VATRate, error)
	SaveVATRate(v VATRate) (VATRate, error)

	PostEntry(e JournalEntry) (JournalEntry, error)
	GetEntry(id int64) (JournalEntry, error)
	ListEntries(f EntryFilter) ([]JournalEntry, error)

	UnbilledTime(f UnbilledFilter) ([]BillableTime, error)

	ListInvoices(f InvoiceFilter) ([]Invoice, error)
	GetInvoice(id int64) (Invoice, error)
	CreateInvoice(in Invoice) (Invoice, error)
	UpdateDraftInvoice(id int64, in Invoice) (Invoice, error)
	DeleteDraftInvoice(id int64) error
	IssueInvoice(id int64, postings []Posting) (Invoice, error)
	SetInvoiceStatus(id int64, status InvoiceStatus) (Invoice, error)
	RecalcInvoicePayments(id int64) (Invoice, error)

	ListPayments(f PaymentFilter) ([]Payment, error)
	GetPayment(id int64) (Payment, error)
	RecordPayment(p Payment, memo string, postings []Posting) (Payment, error)
	DeletePayment(id int64) error

	ListExpenses(f ExpenseFilter) ([]Expense, error)
	GetExpense(id int64) (Expense, error)
	RecordExpense(e Expense, memo string, postings []Posting) (Expense, error)
	DeleteExpense(id int64) error

	Balances(from, to string) ([]Account, error)
	OutstandingInvoices(asOf string) ([]Invoice, error)
	VATTotals(from, to string) (VATReturn, error)
	VATControlTotals(from, to string) (output, input Money, err error)

	ListPeriods() ([]Period, error)
	SavePeriod(p Period) (Period, error)
	LockCovering(date string) (Period, bool, error)

	Settings() (Settings, error)
	SaveSettings(s Settings) (Settings, error)
}

// EntryFilter narrows a journal listing. Empty fields are ignored.
type EntryFilter struct {
	From       string
	To         string
	AccountID  int64
	SourceType JournalSource
	SourceID   int64
	Limit      int
}

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

// ---- accounts ----

const accountColumns = `id, code, name, type, COALESCE(parent_id,0), system_key,
	description, archived, created_at`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var typ string
	err := row.Scan(&a.ID, &a.Code, &a.Name, &typ, &a.ParentID, &a.SystemKey,
		&a.Description, &a.Archived, &a.CreatedAt)
	a.Type = AccountType(typ)
	return a, err
}

func (r *SQLiteRepository) ListAccounts(includeArchived bool) ([]Account, error) {
	q := `SELECT ` + accountColumns + ` FROM acct_accounts`
	if !includeArchived {
		q += ` WHERE archived = 0`
	}
	q += ` ORDER BY code`
	rows, err := r.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) GetAccount(id int64) (Account, error) {
	a, err := scanAccount(r.db.QueryRow(`SELECT `+accountColumns+` FROM acct_accounts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// AccountBySystemKey resolves one of the accounts the code must be able to
// find. A missing one is a broken install rather than a user error, and the
// error says so.
func (r *SQLiteRepository) AccountBySystemKey(key string) (Account, error) {
	a, err := scanAccount(r.db.QueryRow(`SELECT `+accountColumns+
		` FROM acct_accounts WHERE system_key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: no account carries the system key %q; "+
			"the chart of accounts seed is incomplete", ErrNotFound, key)
	}
	return a, err
}

func (r *SQLiteRepository) CreateAccount(a Account) (Account, error) {
	res, err := r.db.Exec(`INSERT INTO acct_accounts
		(code, name, type, parent_id, system_key, description, archived, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		a.Code, a.Name, string(a.Type), nullZero(a.ParentID), a.SystemKey,
		a.Description, boolInt(a.Archived), a.CreatedAt)
	if err != nil {
		return Account{}, mapConstraint(err, "account")
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Account{}, err
	}
	return r.GetAccount(id)
}

func (r *SQLiteRepository) UpdateAccount(id int64, a Account) (Account, error) {
	// system_key is deliberately not updatable: it is the code's handle on the
	// account, and moving it at runtime would silently repoint future postings.
	res, err := r.db.Exec(`UPDATE acct_accounts
		SET code = ?, name = ?, type = ?, parent_id = ?, description = ?, archived = ?
		WHERE id = ?`,
		a.Code, a.Name, string(a.Type), nullZero(a.ParentID), a.Description,
		boolInt(a.Archived), id)
	if err != nil {
		return Account{}, mapConstraint(err, "account")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Account{}, ErrNotFound
	}
	return r.GetAccount(id)
}

func (r *SQLiteRepository) SetAccountArchived(id int64, archived bool) error {
	res, err := r.db.Exec(`UPDATE acct_accounts SET archived = ? WHERE id = ?`, boolInt(archived), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AccountHasPostings reports whether anything has ever been posted to the
// account. An account with history is archived rather than deleted — deleting
// it would orphan a line the trial balance depends on.
func (r *SQLiteRepository) AccountHasPostings(id int64) (bool, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM acct_journal_lines WHERE account_id = ?`, id).Scan(&n)
	return n > 0, err
}

// ---- VAT rates ----

func (r *SQLiteRepository) ListVATRates(includeArchived bool) ([]VATRate, error) {
	q := `SELECT id, code, name, rate_bp, kind, archived FROM acct_vat_rates`
	if !includeArchived {
		q += ` WHERE archived = 0`
	}
	q += ` ORDER BY rate_bp DESC, code`
	rows, err := r.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("list vat rates: %w", err)
	}
	defer rows.Close()

	out := []VATRate{}
	for rows.Next() {
		var v VATRate
		var kind string
		if err := rows.Scan(&v.ID, &v.Code, &v.Name, &v.RateBP, &kind, &v.Archived); err != nil {
			return nil, fmt.Errorf("list vat rates: %w", err)
		}
		v.Kind = VATKind(kind)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) GetVATRate(id int64) (VATRate, error) {
	var v VATRate
	var kind string
	err := r.db.QueryRow(`SELECT id, code, name, rate_bp, kind, archived
		FROM acct_vat_rates WHERE id = ?`, id).
		Scan(&v.ID, &v.Code, &v.Name, &v.RateBP, &kind, &v.Archived)
	if errors.Is(err, sql.ErrNoRows) {
		return VATRate{}, ErrNotFound
	}
	v.Kind = VATKind(kind)
	return v, err
}

func (r *SQLiteRepository) SaveVATRate(v VATRate) (VATRate, error) {
	if v.ID == 0 {
		res, err := r.db.Exec(`INSERT INTO acct_vat_rates (code, name, rate_bp, kind, archived)
			VALUES (?,?,?,?,?)`, v.Code, v.Name, v.RateBP, string(v.Kind), boolInt(v.Archived))
		if err != nil {
			return VATRate{}, mapConstraint(err, "VAT rate")
		}
		id, err := res.LastInsertId()
		if err != nil {
			return VATRate{}, err
		}
		return r.GetVATRate(id)
	}
	// rate_bp is intentionally updatable, but every document snapshots the rate
	// it used, so an edit here changes future invoices only.
	if _, err := r.db.Exec(`UPDATE acct_vat_rates
		SET code = ?, name = ?, rate_bp = ?, kind = ?, archived = ? WHERE id = ?`,
		v.Code, v.Name, v.RateBP, string(v.Kind), boolInt(v.Archived), v.ID); err != nil {
		return VATRate{}, mapConstraint(err, "VAT rate")
	}
	return r.GetVATRate(v.ID)
}

// ---- journal ----

// PostEntry writes an entry and its lines in one transaction. The caller has
// already checked that it balances; this is the step that makes "all of it or
// none of it" true, so a crash between the two inserts cannot leave a header
// with no lines claiming to be a balanced transaction.
func (r *SQLiteRepository) PostEntry(e JournalEntry) (JournalEntry, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return JournalEntry{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	id, err := insertEntryTx(tx, e)
	if err != nil {
		return JournalEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return JournalEntry{}, err
	}
	return r.GetEntry(id)
}

// insertEntryTx is the shared body used both by PostEntry and by the document
// operations in later files, which need the entry written inside the same
// transaction as the invoice or payment that caused it.
func insertEntryTx(tx *sql.Tx, e JournalEntry) (int64, error) {
	res, err := tx.Exec(`INSERT INTO acct_journal_entries
		(entry_date, memo, source_type, source_id, reverses_id, created_at)
		VALUES (?,?,?,?,?,?)`,
		e.EntryDate, e.Memo, string(e.SourceType), e.SourceID, e.ReversesID, e.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("post entry: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for i, l := range e.Lines {
		if _, err := tx.Exec(`INSERT INTO acct_journal_lines
			(entry_id, account_id, debit_minor, credit_minor, description, ordinal)
			VALUES (?,?,?,?,?,?)`,
			id, l.AccountID, int64(l.Debit), int64(l.Credit), l.Description, i); err != nil {
			return 0, fmt.Errorf("post entry line %d: %w", i+1, err)
		}
	}
	return id, nil
}

func (r *SQLiteRepository) GetEntry(id int64) (JournalEntry, error) {
	var e JournalEntry
	var src string
	err := r.db.QueryRow(`SELECT id, entry_date, memo, source_type, source_id, reverses_id, created_at
		FROM acct_journal_entries WHERE id = ?`, id).
		Scan(&e.ID, &e.EntryDate, &e.Memo, &src, &e.SourceID, &e.ReversesID, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return JournalEntry{}, ErrNotFound
	}
	if err != nil {
		return JournalEntry{}, err
	}
	e.SourceType = JournalSource(src)

	lines, err := r.linesFor(id)
	if err != nil {
		return JournalEntry{}, err
	}
	e.Lines = lines
	return e, nil
}

func (r *SQLiteRepository) linesFor(entryID int64) ([]JournalLine, error) {
	rows, err := r.db.Query(`SELECT l.id, l.entry_id, l.account_id, l.debit_minor,
		l.credit_minor, l.description, l.ordinal, a.code, a.name, a.type
		FROM acct_journal_lines l
		JOIN acct_accounts a ON a.id = l.account_id
		WHERE l.entry_id = ? ORDER BY l.ordinal, l.id`, entryID)
	if err != nil {
		return nil, fmt.Errorf("entry lines: %w", err)
	}
	defer rows.Close()

	out := []JournalLine{}
	for rows.Next() {
		var l JournalLine
		var typ string
		if err := rows.Scan(&l.ID, &l.EntryID, &l.AccountID, &l.Debit, &l.Credit,
			&l.Description, &l.Ordinal, &l.AccountCode, &l.AccountName, &typ); err != nil {
			return nil, fmt.Errorf("entry lines: %w", err)
		}
		l.AccountType = AccountType(typ)
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) ListEntries(f EntryFilter) ([]JournalEntry, error) {
	var where []string
	var args []any
	if f.From != "" {
		where = append(where, "e.entry_date >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		where = append(where, "e.entry_date <= ?")
		args = append(args, f.To)
	}
	if f.SourceType != "" {
		where = append(where, "e.source_type = ?")
		args = append(args, string(f.SourceType))
	}
	if f.SourceID != 0 {
		where = append(where, "e.source_id = ?")
		args = append(args, f.SourceID)
	}
	if f.AccountID != 0 {
		where = append(where, `EXISTS (SELECT 1 FROM acct_journal_lines l
			WHERE l.entry_id = e.id AND l.account_id = ?)`)
		args = append(args, f.AccountID)
	}

	q := `SELECT e.id, e.entry_date, e.memo, e.source_type, e.source_id, e.reverses_id, e.created_at
		FROM acct_journal_entries e`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY e.entry_date DESC, e.id DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()

	out := []JournalEntry{}
	for rows.Next() {
		var e JournalEntry
		var src string
		if err := rows.Scan(&e.ID, &e.EntryDate, &e.Memo, &src, &e.SourceID,
			&e.ReversesID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("list entries: %w", err)
		}
		e.SourceType = JournalSource(src)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		lines, err := r.linesFor(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Lines = lines
	}
	return out, nil
}

// ---- periods ----

func (r *SQLiteRepository) ListPeriods() ([]Period, error) {
	rows, err := r.db.Query(`SELECT id, starts_on, ends_on, locked, locked_at, note
		FROM acct_periods ORDER BY starts_on DESC`)
	if err != nil {
		return nil, fmt.Errorf("list periods: %w", err)
	}
	defer rows.Close()

	out := []Period{}
	for rows.Next() {
		var p Period
		if err := rows.Scan(&p.ID, &p.StartsOn, &p.EndsOn, &p.Locked, &p.LockedAt, &p.Note); err != nil {
			return nil, fmt.Errorf("list periods: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) SavePeriod(p Period) (Period, error) {
	if p.ID == 0 {
		res, err := r.db.Exec(`INSERT INTO acct_periods (starts_on, ends_on, locked, locked_at, note)
			VALUES (?,?,?,?,?)`, p.StartsOn, p.EndsOn, boolInt(p.Locked), p.LockedAt, p.Note)
		if err != nil {
			return Period{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return Period{}, err
		}
		p.ID = id
		return p, nil
	}
	if _, err := r.db.Exec(`UPDATE acct_periods
		SET starts_on = ?, ends_on = ?, locked = ?, locked_at = ?, note = ? WHERE id = ?`,
		p.StartsOn, p.EndsOn, boolInt(p.Locked), p.LockedAt, p.Note, p.ID); err != nil {
		return Period{}, err
	}
	return p, nil
}

// LockCovering finds a locked period containing the date, if there is one. This
// is the check every dated write runs, and the backdated case is the one that
// matters: posting into last quarter after its VAT return went out is exactly
// what a lock exists to stop.
func (r *SQLiteRepository) LockCovering(date string) (Period, bool, error) {
	var p Period
	err := r.db.QueryRow(`SELECT id, starts_on, ends_on, locked, locked_at, note
		FROM acct_periods
		WHERE locked = 1 AND starts_on <= ? AND ends_on >= ?
		ORDER BY starts_on DESC LIMIT 1`, date, date).
		Scan(&p.ID, &p.StartsOn, &p.EndsOn, &p.Locked, &p.LockedAt, &p.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return Period{}, false, nil
	}
	if err != nil {
		return Period{}, false, err
	}
	return p, true, nil
}

// ---- settings ----

func (r *SQLiteRepository) Settings() (Settings, error) {
	var s Settings
	var vatID sql.NullInt64
	err := r.db.QueryRow(`SELECT currency, currency_symbol, default_terms_days,
		default_vat_rate_id, fiscal_year_start_month, updated_at
		FROM acct_settings WHERE id = 1`).
		Scan(&s.Currency, &s.CurrencySymbol, &s.DefaultTermsDays, &vatID,
			&s.FiscalYearStartMonth, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, ErrNotFound
	}
	s.DefaultVATRateID = vatID.Int64
	return s, err
}

func (r *SQLiteRepository) SaveSettings(s Settings) (Settings, error) {
	if _, err := r.db.Exec(`UPDATE acct_settings
		SET currency = ?, currency_symbol = ?, default_terms_days = ?,
		    default_vat_rate_id = ?, fiscal_year_start_month = ?, updated_at = ?
		WHERE id = 1`,
		s.Currency, s.CurrencySymbol, s.DefaultTermsDays, nullZero(s.DefaultVATRateID),
		s.FiscalYearStartMonth, s.UpdatedAt); err != nil {
		return Settings{}, err
	}
	return r.Settings()
}

// ---- helpers ----

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullZero maps a zero id to SQL NULL, so an optional foreign key is absent
// rather than pointing at a row that does not exist.
func nullZero(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// mapConstraint turns SQLite's constraint text into something a user can act
// on. The raw message names a column and an index, which tells an operator
// nothing about what they typed.
func mapConstraint(err error, what string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: acct_accounts.code"):
		return invalid("an account with that code already exists")
	case strings.Contains(msg, "UNIQUE constraint failed: acct_accounts.system_key"):
		return invalid("another account already holds that system key")
	case strings.Contains(msg, "UNIQUE constraint failed: acct_vat_rates.code"):
		return invalid("a VAT rate with that code already exists")
	case strings.Contains(msg, "CHECK constraint failed"):
		return invalid("%s has a value the database rejected: %s", what, msg)
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return invalid("%s references something that does not exist", what)
	}
	return err
}

// VATRateByKind finds the rate representing a treatment. Reverse charge needs
// this: the line carries no percentage, but it must still be tagged with the
// treatment or the VAT return counts it as an ordinary standard-rated sale.
func (r *SQLiteRepository) VATRateByKind(kind VATKind) (VATRate, error) {
	var v VATRate
	var k string
	err := r.db.QueryRow(`SELECT id, code, name, rate_bp, kind, archived
		FROM acct_vat_rates WHERE kind = ? AND archived = 0 ORDER BY id LIMIT 1`, string(kind)).
		Scan(&v.ID, &v.Code, &v.Name, &v.RateBP, &k, &v.Archived)
	if errors.Is(err, sql.ErrNoRows) {
		return VATRate{}, fmt.Errorf("%w: no VAT rate is configured for %s treatment", ErrNotFound, kind)
	}
	v.Kind = VATKind(k)
	return v, err
}
