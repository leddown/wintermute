package accounting

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Expense is money out: a cost with a category (the expense account it lands
// in) and a source (the bank or card it left). Recoverable input VAT is split
// out so it reaches the VAT control account instead of inflating the cost.
type Expense struct {
	ID             int64  `json:"id"`
	SpentOn        string `json:"spent_on"`
	Vendor         string `json:"vendor"`
	Description    string `json:"description"`
	AccountID      int64  `json:"account_id"`
	PaidFromID     int64  `json:"paid_from_id"`
	Net            Money  `json:"net"`
	VATRateID      int64  `json:"vat_rate_id"`
	VATRateBP      int64  `json:"vat_rate_bp"`
	VAT            Money  `json:"vat"`
	Total          Money  `json:"total"`
	VATReclaimable bool   `json:"vat_reclaimable"`
	Billable       bool   `json:"billable"`
	ClientID       int64  `json:"client_id"`
	EngagementID   int64  `json:"engagement_id"`
	RebilledOn     int64  `json:"rebilled_invoice_id"`
	ReceiptNote    string `json:"receipt_note"`
	JournalEntry   int64  `json:"journal_entry_id"`
	CreatedAt      string `json:"created_at"`

	// Derived for display.
	AccountName  string `json:"account_name"`
	PaidFromName string `json:"paid_from_name"`
	ClientName   string `json:"client_name"`
}

// ExpenseFilter narrows a listing.
type ExpenseFilter struct {
	From         string
	To           string
	AccountID    int64
	ClientID     int64
	BillableOnly bool
	Unrebilled   bool
	Search       string
	Limit        int
}

// ---- service ----

func (s *Service) ListExpenses(f ExpenseFilter) ([]Expense, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	return s.repo.ListExpenses(f)
}

func (s *Service) GetExpense(id int64) (Expense, error) { return s.repo.GetExpense(id) }

// RecordExpense stores a cost and posts it.
//
//	Dr  Expense category     net
//	Dr  VAT on Purchases     vat      (only when reclaimable)
//	  Cr  Bank or card         gross
//
// When the VAT is not reclaimable — entertainment, private-use portions — it is
// added to the cost instead of the VAT account, because that is what it is: an
// expense, not a claim against the authority.
func (s *Service) RecordExpense(e Expense) (Expense, error) {
	if e.SpentOn == "" {
		e.SpentOn = today()
	}
	if err := validDate(e.SpentOn); err != nil {
		return Expense{}, err
	}
	if err := s.checkPeriodOpen(e.SpentOn); err != nil {
		return Expense{}, err
	}

	e.Vendor = strings.TrimSpace(e.Vendor)
	e.Description = strings.TrimSpace(e.Description)
	e.ReceiptNote = strings.TrimSpace(e.ReceiptNote)
	if e.Vendor == "" && e.Description == "" {
		return Expense{}, invalid("an expense needs a vendor or a description")
	}
	if e.Net <= 0 {
		return Expense{}, invalid("an expense needs a positive net amount")
	}

	category, err := s.repo.GetAccount(e.AccountID)
	if err != nil {
		return Expense{}, invalid("expense category %d does not exist", e.AccountID)
	}
	// Assets are allowed deliberately: buying a laptop is money out that lands
	// on the balance sheet, and forcing it through an expense account would
	// misstate both the P&L and the asset register.
	if category.Type != AccountExpense && category.Type != AccountAsset {
		return Expense{}, invalid(
			"expenses must be categorised to an expense or asset account; %s (%s) is a %s account",
			category.Code, category.Name, category.Type)
	}

	if e.PaidFromID == 0 {
		bank, err := s.repo.AccountBySystemKey(SysBank)
		if err != nil {
			return Expense{}, err
		}
		e.PaidFromID = bank.ID
	}
	source, err := s.repo.GetAccount(e.PaidFromID)
	if err != nil {
		return Expense{}, invalid("payment source %d does not exist", e.PaidFromID)
	}
	// An asset (money left the bank) or a liability (it went on the card, or is
	// owed to a supplier). Anything else is not a way of paying for something.
	if source.Type != AccountAsset && source.Type != AccountLiability {
		return Expense{}, invalid(
			"an expense must be paid from an asset or liability account; %s (%s) is a %s account",
			source.Code, source.Name, source.Type)
	}

	if e.VATRateID != 0 {
		rate, err := s.repo.GetVATRate(e.VATRateID)
		if err != nil {
			return Expense{}, invalid("VAT rate %d does not exist", e.VATRateID)
		}
		e.VATRateBP = rate.RateBP
	}
	e.VAT = VAT(e.Net, e.VATRateBP)
	e.Total = e.Net + e.VAT

	if e.Billable && e.ClientID == 0 {
		return Expense{}, invalid("a billable expense needs a client to bill it to")
	}

	postings := []Posting{}
	memo := e.Vendor
	if memo == "" {
		memo = e.Description
	}

	if e.VAT != 0 && e.VATReclaimable {
		vatAcct, err := s.repo.AccountBySystemKey(SysVATInput)
		if err != nil {
			return Expense{}, err
		}
		postings = append(postings,
			Debit(e.AccountID, e.Net, memo),
			Debit(vatAcct.ID, e.VAT, memo+" VAT"))
	} else {
		// Irrecoverable VAT is part of the cost.
		postings = append(postings, Debit(e.AccountID, e.Total, memo))
	}
	postings = append(postings, Credit(e.PaidFromID, e.Total, memo))

	e.CreatedAt = nowStamp()
	return s.repo.RecordExpense(e, memo, postings)
}

// DeleteExpense removes a cost and reverses its posting.
func (s *Service) DeleteExpense(id int64) error {
	e, err := s.repo.GetExpense(id)
	if err != nil {
		return err
	}
	if e.RebilledOn != 0 {
		return invalid("this expense has been rebilled on an invoice; credit the invoice first")
	}
	if err := s.checkPeriodOpen(e.SpentOn); err != nil {
		return err
	}
	if e.JournalEntry != 0 {
		if _, err := s.Reverse(e.JournalEntry, today(), "Reversal of expense: "+e.Vendor); err != nil {
			return err
		}
	}
	return s.repo.DeleteExpense(id)
}

// ---- repository ----

const expenseColumns = `x.id, x.spent_on, x.vendor, x.description, x.account_id,
	x.paid_from_id, x.net_minor, COALESCE(x.vat_rate_id,0), x.vat_rate_bp, x.vat_minor,
	x.total_minor, x.vat_reclaimable, x.billable, x.client_id, x.engagement_id,
	x.rebilled_invoice_id, x.receipt_note, x.journal_entry_id, x.created_at,
	COALESCE(a.name,''), COALESCE(s.name,''), COALESCE(c.name,'')`

const expenseFrom = ` FROM acct_expenses x
	LEFT JOIN acct_accounts a ON a.id = x.account_id
	LEFT JOIN acct_accounts s ON s.id = x.paid_from_id
	LEFT JOIN crm_clients c ON c.id = x.client_id`

func scanExpense(row interface{ Scan(...any) error }) (Expense, error) {
	var e Expense
	err := row.Scan(&e.ID, &e.SpentOn, &e.Vendor, &e.Description, &e.AccountID,
		&e.PaidFromID, &e.Net, &e.VATRateID, &e.VATRateBP, &e.VAT, &e.Total,
		&e.VATReclaimable, &e.Billable, &e.ClientID, &e.EngagementID,
		&e.RebilledOn, &e.ReceiptNote, &e.JournalEntry, &e.CreatedAt,
		&e.AccountName, &e.PaidFromName, &e.ClientName)
	return e, err
}

func (r *SQLiteRepository) GetExpense(id int64) (Expense, error) {
	e, err := scanExpense(r.db.QueryRow(`SELECT `+expenseColumns+expenseFrom+
		` WHERE x.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Expense{}, ErrNotFound
	}
	return e, err
}

func (r *SQLiteRepository) ListExpenses(f ExpenseFilter) ([]Expense, error) {
	var where []string
	var args []any
	if f.From != "" {
		where = append(where, "x.spent_on >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		where = append(where, "x.spent_on <= ?")
		args = append(args, f.To)
	}
	if f.AccountID != 0 {
		where = append(where, "x.account_id = ?")
		args = append(args, f.AccountID)
	}
	if f.ClientID != 0 {
		where = append(where, "x.client_id = ?")
		args = append(args, f.ClientID)
	}
	if f.BillableOnly {
		where = append(where, "x.billable = 1")
	}
	if f.Unrebilled {
		where = append(where, "x.rebilled_invoice_id = 0")
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		where = append(where, "(x.vendor LIKE ? OR x.description LIKE ?)")
		like := "%" + s + "%"
		args = append(args, like, like)
	}

	q := `SELECT ` + expenseColumns + expenseFrom
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY x.spent_on DESC, x.id DESC LIMIT %d", f.Limit)

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list expenses: %w", err)
	}
	defer rows.Close()

	out := []Expense{}
	for rows.Next() {
		e, err := scanExpense(rows)
		if err != nil {
			return nil, fmt.Errorf("list expenses: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) RecordExpense(e Expense, memo string, postings []Posting) (Expense, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return Expense{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	entry := JournalEntry{
		EntryDate:  e.SpentOn,
		Memo:       memo,
		SourceType: SourceExpense,
		CreatedAt:  e.CreatedAt,
	}
	for _, p := range postings {
		entry.Lines = append(entry.Lines, JournalLine{
			AccountID: p.AccountID, Debit: p.Debit, Credit: p.Credit,
			Description: p.Description,
		})
	}
	entryID, err := insertEntryTx(tx, entry)
	if err != nil {
		return Expense{}, err
	}

	res, err := tx.Exec(`INSERT INTO acct_expenses
		(spent_on, vendor, description, account_id, paid_from_id, net_minor,
		 vat_rate_id, vat_rate_bp, vat_minor, total_minor, vat_reclaimable, billable,
		 client_id, engagement_id, rebilled_invoice_id, receipt_note, journal_entry_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.SpentOn, e.Vendor, e.Description, e.AccountID, e.PaidFromID, int64(e.Net),
		nullZero(e.VATRateID), e.VATRateBP, int64(e.VAT), int64(e.Total),
		boolInt(e.VATReclaimable), boolInt(e.Billable), e.ClientID, e.EngagementID,
		e.RebilledOn, e.ReceiptNote, entryID, e.CreatedAt)
	if err != nil {
		return Expense{}, mapConstraint(err, "expense")
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Expense{}, err
	}
	if _, err := tx.Exec(`UPDATE acct_journal_entries SET source_id = ? WHERE id = ?`,
		id, entryID); err != nil {
		return Expense{}, err
	}
	if err := tx.Commit(); err != nil {
		return Expense{}, err
	}
	return r.GetExpense(id)
}

func (r *SQLiteRepository) DeleteExpense(id int64) error {
	res, err := r.db.Exec(`DELETE FROM acct_expenses WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
