package accounting

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Payment is money received against an invoice. Partial payment is the normal
// case for consulting work, so this is a row against the invoice rather than a
// flag on it, and the invoice's status is derived from the sum.
type Payment struct {
	ID               int64  `json:"id"`
	InvoiceID        int64  `json:"invoice_id"`
	PaidOn           string `json:"paid_on"`
	Amount           Money  `json:"amount"`
	Method           string `json:"method"`
	Reference        string `json:"reference"`
	DepositAccountID int64  `json:"deposit_account_id"`
	JournalEntry     int64  `json:"journal_entry_id"`
	CreatedAt        string `json:"created_at"`

	// Derived for display.
	InvoiceNumber string `json:"invoice_number"`
	ClientName    string `json:"client_name"`
}

// PaymentFilter narrows a listing.
type PaymentFilter struct {
	InvoiceID int64
	ClientID  int64
	From      string
	To        string
	Limit     int
}

// ---- service ----

func (s *Service) ListPayments(f PaymentFilter) ([]Payment, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	return s.repo.ListPayments(f)
}

// RecordPayment applies money to an invoice and posts it.
//
//	Dr  Bank              amount
//	  Cr  Accounts Receivable  amount
//
// The invoice's paid total and status are recomputed from the payment rows
// afterwards, so the denormalised figure can never disagree with what is
// actually recorded.
func (s *Service) RecordPayment(p Payment) (Payment, error) {
	inv, err := s.repo.GetInvoice(p.InvoiceID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Payment{}, invalid("no invoice with id %d", p.InvoiceID)
		}
		return Payment{}, err
	}
	switch inv.Status {
	case StatusDraft:
		return Payment{}, invalid(
			"invoice is still a draft; issue it before recording payment against it")
	case StatusVoid:
		return Payment{}, invalid("invoice %s is void", inv.Number)
	case StatusPaid:
		return Payment{}, invalid("invoice %s is already paid in full", inv.Number)
	}

	if p.Amount <= 0 {
		return Payment{}, invalid("a payment must be for a positive amount")
	}
	// Overpayment is refused rather than absorbed. A credit balance on a sales
	// ledger is a real thing, but it needs somewhere to live and a decision
	// about what to do with it; silently posting it would leave AR wrong.
	if outstanding := inv.Outstanding(); p.Amount > outstanding {
		return Payment{}, invalid(
			"payment of %s exceeds the %s outstanding on %s; "+
				"record the exact amount, or credit the difference",
			p.Amount, outstanding, inv.Number)
	}

	if p.PaidOn == "" {
		p.PaidOn = today()
	}
	if err := validDate(p.PaidOn); err != nil {
		return Payment{}, err
	}
	if err := s.checkPeriodOpen(p.PaidOn); err != nil {
		return Payment{}, err
	}

	if p.DepositAccountID == 0 {
		bank, err := s.repo.AccountBySystemKey(SysBank)
		if err != nil {
			return Payment{}, err
		}
		p.DepositAccountID = bank.ID
	}
	deposit, err := s.repo.GetAccount(p.DepositAccountID)
	if err != nil {
		return Payment{}, invalid("deposit account %d does not exist", p.DepositAccountID)
	}
	if deposit.Type != AccountAsset {
		return Payment{}, invalid("payments must land in an asset account; %s (%s) is a %s account",
			deposit.Code, deposit.Name, deposit.Type)
	}

	ar, err := s.repo.AccountBySystemKey(SysAR)
	if err != nil {
		return Payment{}, err
	}

	p.Method = strings.TrimSpace(p.Method)
	if p.Method == "" {
		p.Method = "bank"
	}
	p.Reference = strings.TrimSpace(p.Reference)
	p.CreatedAt = nowStamp()

	memo := fmt.Sprintf("Payment for %s", inv.Number)
	if inv.ClientName != "" {
		memo += " — " + inv.ClientName
	}
	postings := []Posting{
		Debit(p.DepositAccountID, p.Amount, memo),
		Credit(ar.ID, p.Amount, memo),
	}
	return s.repo.RecordPayment(p, memo, postings)
}

// DeletePayment removes a receipt and reverses its ledger entry. Payments are
// operational records rather than issued documents, so unlike an invoice they
// may be removed — a mistyped bank reference should not need a credit note.
// The ledger still gets a reversal rather than a deletion.
func (s *Service) DeletePayment(id int64) error {
	p, err := s.repo.GetPayment(id)
	if err != nil {
		return err
	}
	if err := s.checkPeriodOpen(p.PaidOn); err != nil {
		return err
	}
	if p.JournalEntry != 0 {
		if _, err := s.Reverse(p.JournalEntry, today(),
			fmt.Sprintf("Reversal of payment for %s", p.InvoiceNumber)); err != nil {
			return err
		}
	}
	return s.repo.DeletePayment(id)
}

// ---- repository ----

const paymentColumns = `p.id, p.invoice_id, p.paid_on, p.amount_minor, p.method,
	p.reference, p.deposit_account_id, p.journal_entry_id, p.created_at,
	COALESCE(i.number,''), COALESCE(c.name,'')`

const paymentFrom = ` FROM acct_payments p
	LEFT JOIN acct_invoices i ON i.id = p.invoice_id
	LEFT JOIN crm_clients c ON c.id = i.client_id`

func scanPayment(row interface{ Scan(...any) error }) (Payment, error) {
	var p Payment
	err := row.Scan(&p.ID, &p.InvoiceID, &p.PaidOn, &p.Amount, &p.Method,
		&p.Reference, &p.DepositAccountID, &p.JournalEntry, &p.CreatedAt,
		&p.InvoiceNumber, &p.ClientName)
	return p, err
}

func (r *SQLiteRepository) GetPayment(id int64) (Payment, error) {
	p, err := scanPayment(r.db.QueryRow(`SELECT `+paymentColumns+paymentFrom+
		` WHERE p.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	return p, err
}

func (r *SQLiteRepository) ListPayments(f PaymentFilter) ([]Payment, error) {
	var where []string
	var args []any
	if f.InvoiceID != 0 {
		where = append(where, "p.invoice_id = ?")
		args = append(args, f.InvoiceID)
	}
	if f.ClientID != 0 {
		where = append(where, "i.client_id = ?")
		args = append(args, f.ClientID)
	}
	if f.From != "" {
		where = append(where, "p.paid_on >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		where = append(where, "p.paid_on <= ?")
		args = append(args, f.To)
	}

	q := `SELECT ` + paymentColumns + paymentFrom
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY p.paid_on DESC, p.id DESC LIMIT %d", f.Limit)

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list payments: %w", err)
	}
	defer rows.Close()

	out := []Payment{}
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, fmt.Errorf("list payments: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordPayment writes the receipt and its ledger entry together, then refreshes
// the invoice. All three in one transaction: a payment without its posting would
// overstate cash, and a posting without its payment would understate what the
// client still owes.
func (r *SQLiteRepository) RecordPayment(p Payment, memo string, postings []Posting) (Payment, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return Payment{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	entry := JournalEntry{
		EntryDate:  p.PaidOn,
		Memo:       memo,
		SourceType: SourcePayment,
		CreatedAt:  p.CreatedAt,
	}
	for _, ps := range postings {
		entry.Lines = append(entry.Lines, JournalLine{
			AccountID: ps.AccountID, Debit: ps.Debit, Credit: ps.Credit,
			Description: ps.Description,
		})
	}
	entryID, err := insertEntryTx(tx, entry)
	if err != nil {
		return Payment{}, err
	}

	res, err := tx.Exec(`INSERT INTO acct_payments
		(invoice_id, paid_on, amount_minor, method, reference, deposit_account_id,
		 journal_entry_id, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		p.InvoiceID, p.PaidOn, int64(p.Amount), p.Method, p.Reference,
		p.DepositAccountID, entryID, p.CreatedAt)
	if err != nil {
		return Payment{}, mapConstraint(err, "payment")
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Payment{}, err
	}
	// The entry knows which payment it came from only once the payment has an id.
	if _, err := tx.Exec(`UPDATE acct_journal_entries SET source_id = ? WHERE id = ?`,
		id, entryID); err != nil {
		return Payment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Payment{}, err
	}

	if _, err := r.RecalcInvoicePayments(p.InvoiceID); err != nil {
		return Payment{}, err
	}
	return r.GetPayment(id)
}

func (r *SQLiteRepository) DeletePayment(id int64) error {
	p, err := r.GetPayment(id)
	if err != nil {
		return err
	}
	if _, err := r.db.Exec(`DELETE FROM acct_payments WHERE id = ?`, id); err != nil {
		return err
	}
	_, err = r.RecalcInvoicePayments(p.InvoiceID)
	return err
}
