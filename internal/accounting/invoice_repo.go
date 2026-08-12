package accounting

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const invoiceColumns = `i.id, i.kind, i.number, i.client_id, i.engagement_id, i.status,
	i.issue_date, i.due_date, i.terms_days, i.subtotal_minor, i.vat_minor, i.total_minor,
	i.paid_minor, i.reverse_charge, i.customer_vat_number, i.bill_to, i.notes,
	i.corrects_id, i.journal_entry_id, i.created_at, i.issued_at,
	COALESCE(c.name,''), COALESCE(e.name,'')`

// The engagement join is LEFT because an invoice need not belong to one, and
// the client join is LEFT so a listing still renders if referential integrity
// is ever bypassed by a manual repair.
const invoiceFrom = ` FROM acct_invoices i
	LEFT JOIN crm_clients c ON c.id = i.client_id
	LEFT JOIN crm_engagements e ON e.id = i.engagement_id`

func scanInvoice(row interface{ Scan(...any) error }) (Invoice, error) {
	var i Invoice
	var kind, status string
	err := row.Scan(&i.ID, &kind, &i.Number, &i.ClientID, &i.EngagementID, &status,
		&i.IssueDate, &i.DueDate, &i.TermsDays, &i.Subtotal, &i.VAT, &i.Total,
		&i.Paid, &i.ReverseCharge, &i.CustomerVAT, &i.BillTo, &i.Notes,
		&i.CorrectsID, &i.JournalEntry, &i.CreatedAt, &i.IssuedAt,
		&i.ClientName, &i.EngagementName)
	i.Kind = InvoiceKind(kind)
	i.Status = InvoiceStatus(status)
	return i, err
}

func (r *SQLiteRepository) GetInvoice(id int64) (Invoice, error) {
	inv, err := scanInvoice(r.db.QueryRow(`SELECT `+invoiceColumns+invoiceFrom+` WHERE i.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, err
	}
	lines, err := r.invoiceLines(id)
	if err != nil {
		return Invoice{}, err
	}
	inv.Lines = lines
	return inv, nil
}

func (r *SQLiteRepository) invoiceLines(invoiceID int64) ([]InvoiceLine, error) {
	rows, err := r.db.Query(`SELECT id, invoice_id, description, quantity_milli,
		unit_price_minor, net_minor, COALESCE(vat_rate_id,0), vat_rate_bp, vat_minor,
		income_account_id, time_entry_id, expense_id, ordinal
		FROM acct_invoice_lines WHERE invoice_id = ? ORDER BY ordinal, id`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("invoice lines: %w", err)
	}
	defer rows.Close()

	out := []InvoiceLine{}
	for rows.Next() {
		var l InvoiceLine
		if err := rows.Scan(&l.ID, &l.InvoiceID, &l.Description, &l.Quantity,
			&l.UnitPrice, &l.Net, &l.VATRateID, &l.VATRateBP, &l.VAT,
			&l.IncomeAccountID, &l.TimeEntryID, &l.ExpenseID, &l.Ordinal); err != nil {
			return nil, fmt.Errorf("invoice lines: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) ListInvoices(f InvoiceFilter) ([]Invoice, error) {
	var where []string
	var args []any
	if f.ClientID != 0 {
		where = append(where, "i.client_id = ?")
		args = append(args, f.ClientID)
	}
	if f.EngagementID != 0 {
		where = append(where, "i.engagement_id = ?")
		args = append(args, f.EngagementID)
	}
	if f.Status != "" {
		where = append(where, "i.status = ?")
		args = append(args, string(f.Status))
	}
	if f.Kind != "" {
		where = append(where, "i.kind = ?")
		args = append(args, string(f.Kind))
	}
	if f.From != "" {
		where = append(where, "i.issue_date >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		where = append(where, "i.issue_date <= ?")
		args = append(args, f.To)
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		where = append(where, "(i.number LIKE ? OR c.name LIKE ? OR i.notes LIKE ?)")
		like := "%" + s + "%"
		args = append(args, like, like, like)
	}

	q := `SELECT ` + invoiceColumns + invoiceFrom
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// Drafts have no number and no meaningful order among themselves, so the id
	// breaks the tie and keeps listings stable.
	q += fmt.Sprintf(" ORDER BY i.issue_date DESC, i.id DESC LIMIT %d", f.Limit)

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list invoices: %w", err)
	}
	defer rows.Close()

	out := []Invoice{}
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, fmt.Errorf("list invoices: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) CreateInvoice(in Invoice) (Invoice, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return Invoice{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec(`INSERT INTO acct_invoices
		(kind, number, client_id, engagement_id, status, issue_date, due_date, terms_days,
		 subtotal_minor, vat_minor, total_minor, paid_minor, reverse_charge,
		 customer_vat_number, bill_to, notes, corrects_id, journal_entry_id, created_at, issued_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'')`,
		string(in.Kind), in.Number, in.ClientID, in.EngagementID, string(in.Status),
		in.IssueDate, in.DueDate, in.TermsDays, int64(in.Subtotal), int64(in.VAT),
		int64(in.Total), int64(in.Paid), boolInt(in.ReverseCharge), in.CustomerVAT,
		in.BillTo, in.Notes, in.CorrectsID, in.JournalEntry, in.CreatedAt)
	if err != nil {
		return Invoice{}, mapConstraint(err, "invoice")
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Invoice{}, err
	}
	if err := insertLinesTx(tx, id, in.Lines); err != nil {
		return Invoice{}, err
	}
	if err := tx.Commit(); err != nil {
		return Invoice{}, err
	}
	return r.GetInvoice(id)
}

func (r *SQLiteRepository) UpdateDraftInvoice(id int64, in Invoice) (Invoice, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return Invoice{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	// The WHERE clause repeats the draft check the service already made. Two
	// requests issuing and editing the same draft at once would otherwise race,
	// and the loser would rewrite a document that had already gone out.
	res, err := tx.Exec(`UPDATE acct_invoices SET
		client_id = ?, engagement_id = ?, issue_date = ?, due_date = ?, terms_days = ?,
		subtotal_minor = ?, vat_minor = ?, total_minor = ?, reverse_charge = ?,
		customer_vat_number = ?, bill_to = ?, notes = ?
		WHERE id = ? AND status = 'draft'`,
		in.ClientID, in.EngagementID, in.IssueDate, in.DueDate, in.TermsDays,
		int64(in.Subtotal), int64(in.VAT), int64(in.Total), boolInt(in.ReverseCharge),
		in.CustomerVAT, in.BillTo, in.Notes, id)
	if err != nil {
		return Invoice{}, mapConstraint(err, "invoice")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Invoice{}, invalid("invoice is no longer a draft")
	}

	if _, err := tx.Exec(`DELETE FROM acct_invoice_lines WHERE invoice_id = ?`, id); err != nil {
		return Invoice{}, err
	}
	if err := insertLinesTx(tx, id, in.Lines); err != nil {
		return Invoice{}, err
	}
	if err := tx.Commit(); err != nil {
		return Invoice{}, err
	}
	return r.GetInvoice(id)
}

func insertLinesTx(tx *sql.Tx, invoiceID int64, lines []InvoiceLine) error {
	for i, l := range lines {
		if _, err := tx.Exec(`INSERT INTO acct_invoice_lines
			(invoice_id, description, quantity_milli, unit_price_minor, net_minor,
			 vat_rate_id, vat_rate_bp, vat_minor, income_account_id, time_entry_id,
			 expense_id, ordinal)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			invoiceID, l.Description, int64(l.Quantity), int64(l.UnitPrice), int64(l.Net),
			nullZero(l.VATRateID), l.VATRateBP, int64(l.VAT), l.IncomeAccountID,
			l.TimeEntryID, l.ExpenseID, i); err != nil {
			return fmt.Errorf("invoice line %d: %w", i+1, mapConstraint(err, "invoice line"))
		}
	}
	return nil
}

func (r *SQLiteRepository) DeleteDraftInvoice(id int64) error {
	res, err := r.db.Exec(`DELETE FROM acct_invoices WHERE id = ? AND status = 'draft'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return invalid("invoice is not a draft")
	}
	return nil
}

// IssueInvoice is the irreversible step, and everything it does happens in one
// transaction: allocate the next number, post the ledger entry, stamp the
// invoice, and flag the time entries it billed.
//
// If any of that fails, none of it happened — which matters most for the
// number. A number allocated by a transaction that then rolled back would leave
// a gap in a sequence that is required by law to have none.
func (r *SQLiteRepository) IssueInvoice(id int64, postings []Posting) (Invoice, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return Invoice{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	var kind, status, issueDate, clientName string
	err = tx.QueryRow(`SELECT i.kind, i.status, i.issue_date, COALESCE(c.name,'')
		FROM acct_invoices i LEFT JOIN crm_clients c ON c.id = i.client_id
		WHERE i.id = ?`, id).Scan(&kind, &status, &issueDate, &clientName)
	if errors.Is(err, sql.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, err
	}
	if status != string(StatusDraft) {
		return Invoice{}, invalid("invoice is %s, not a draft", status)
	}

	seq := "invoice"
	if InvoiceKind(kind) == KindCreditNote {
		seq = "credit_note"
	}
	number, err := allocateNumberTx(tx, seq)
	if err != nil {
		return Invoice{}, err
	}

	memo := number
	if clientName != "" {
		memo += " — " + clientName
	}
	entry := JournalEntry{
		EntryDate:  issueDate,
		Memo:       memo,
		SourceType: SourceInvoice,
		SourceID:   id,
		CreatedAt:  nowStamp(),
	}
	if InvoiceKind(kind) == KindCreditNote {
		entry.SourceType = SourceCreditNote
	}
	for _, p := range postings {
		if p.Debit == 0 && p.Credit == 0 {
			continue
		}
		desc := p.Description
		// The postings were built before a number existed; give them the real
		// one now so the ledger reads the same as the document.
		desc = strings.Replace(desc, "invoice", number, 1)
		entry.Lines = append(entry.Lines, JournalLine{
			AccountID:   p.AccountID,
			Debit:       p.Debit,
			Credit:      p.Credit,
			Description: desc,
		})
	}
	entryID, err := insertEntryTx(tx, entry)
	if err != nil {
		return Invoice{}, err
	}

	if _, err := tx.Exec(`UPDATE acct_invoices
		SET number = ?, status = ?, journal_entry_id = ?, issued_at = ?
		WHERE id = ? AND status = 'draft'`,
		number, string(StatusIssued), entryID, nowStamp(), id); err != nil {
		return Invoice{}, mapConstraint(err, "invoice")
	}

	// Flag the CRM time entries this invoice billed. This is what stops the same
	// hours being billed twice, and it belongs in this transaction: entries
	// marked by an issue that then failed would be unbillable forever.
	if _, err := tx.Exec(`UPDATE crm_time_entries SET invoiced = 1
		WHERE id IN (SELECT time_entry_id FROM acct_invoice_lines
		             WHERE invoice_id = ? AND time_entry_id <> 0)`, id); err != nil {
		return Invoice{}, fmt.Errorf("flag billed time entries: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Invoice{}, err
	}
	return r.GetInvoice(id)
}

// allocateNumberTx reads and bumps a sequence inside the caller's transaction.
// Read-then-write under SQLite's write lock is what makes it gap-free and
// collision-free; AUTOINCREMENT would give neither.
func allocateNumberTx(tx *sql.Tx, name string) (string, error) {
	var prefix string
	var next, padding int64
	err := tx.QueryRow(`SELECT prefix, next_value, padding FROM acct_sequences WHERE name = ?`, name).
		Scan(&prefix, &next, &padding)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: no number sequence called %q", ErrNotFound, name)
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE acct_sequences SET next_value = next_value + 1 WHERE name = ?`, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%0*d", prefix, padding, next), nil
}

func (r *SQLiteRepository) SetInvoiceStatus(id int64, status InvoiceStatus) (Invoice, error) {
	if _, err := r.db.Exec(`UPDATE acct_invoices SET status = ? WHERE id = ?`, string(status), id); err != nil {
		return Invoice{}, err
	}
	return r.GetInvoice(id)
}

// RecalcInvoicePayments refreshes the denormalised paid total and the status
// that follows from it. Called after any change to payments so the two never
// disagree.
func (r *SQLiteRepository) RecalcInvoicePayments(id int64) (Invoice, error) {
	inv, err := r.GetInvoice(id)
	if err != nil {
		return Invoice{}, err
	}
	var paid sql.NullInt64
	if err := r.db.QueryRow(`SELECT SUM(amount_minor) FROM acct_payments WHERE invoice_id = ?`, id).
		Scan(&paid); err != nil {
		return Invoice{}, err
	}
	inv.Paid = Money(paid.Int64)
	status := paymentStatus(inv)
	if _, err := r.db.Exec(`UPDATE acct_invoices SET paid_minor = ?, status = ? WHERE id = ?`,
		int64(inv.Paid), string(status), id); err != nil {
		return Invoice{}, err
	}
	return r.GetInvoice(id)
}
