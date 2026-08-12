package accounting

import "fmt"

// Balances returns every account with its debit and credit totals over a date
// window.
//
// The date filter sits in an EXISTS against the entry rather than in the WHERE
// clause. A WHERE would drop accounts with no movement in the period, which is
// wrong for a balance sheet — an account holding a balance from last year with
// nothing posted this month still belongs on it. Putting the filter on the join
// condition instead would be worse still: lines outside the window would join
// to a NULL entry and be counted anyway.
func (r *SQLiteRepository) Balances(from, to string) ([]Account, error) {
	rows, err := r.db.Query(`
		SELECT a.id, a.code, a.name, a.type, a.system_key, a.archived,
		       COALESCE(SUM(l.debit_minor), 0), COALESCE(SUM(l.credit_minor), 0)
		FROM acct_accounts a
		LEFT JOIN acct_journal_lines l ON l.account_id = a.id
		     AND EXISTS (SELECT 1 FROM acct_journal_entries e
		                 WHERE e.id = l.entry_id AND e.entry_date >= ? AND e.entry_date <= ?)
		GROUP BY a.id
		ORDER BY a.code`, from, to)
	if err != nil {
		return nil, fmt.Errorf("balances: %w", err)
	}
	defer rows.Close()

	out := []Account{}
	for rows.Next() {
		var a Account
		var typ string
		if err := rows.Scan(&a.ID, &a.Code, &a.Name, &typ, &a.SystemKey, &a.Archived,
			&a.DebitTotal, &a.CreditTotal); err != nil {
			return nil, fmt.Errorf("balances: %w", err)
		}
		a.Type = AccountType(typ)
		// Signed in the account's own normal direction, so a reader never has to
		// know which side the type sits on.
		if a.Type.DebitNormal() {
			a.Balance = a.DebitTotal - a.CreditTotal
		} else {
			a.Balance = a.CreditTotal - a.DebitTotal
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// OutstandingInvoices lists issued documents that are not settled as of a date.
// Drafts are excluded (nothing is owed on a document that has not gone out) and
// so are voids (nothing is owed on a cancelled one).
func (r *SQLiteRepository) OutstandingInvoices(asOf string) ([]Invoice, error) {
	rows, err := r.db.Query(`SELECT `+invoiceColumns+invoiceFrom+`
		WHERE i.status IN ('issued','part_paid')
		  AND i.issue_date <= ?
		ORDER BY c.name, i.due_date, i.id`, asOf)
	if err != nil {
		return nil, fmt.Errorf("outstanding invoices: %w", err)
	}
	defer rows.Close()

	out := []Invoice{}
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, fmt.Errorf("outstanding invoices: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// VATTotals sums the period from the documents, split by treatment.
//
// Sales come from invoice lines rather than the invoice header so that a
// document mixing rates is counted correctly on each. Credit notes are summed
// with the opposite sign, which is what makes a corrected quarter come out
// right.
func (r *SQLiteRepository) VATTotals(from, to string) (VATReturn, error) {
	var v VATReturn

	rows, err := r.db.Query(`
		SELECT COALESCE(vr.kind, 'standard'), i.kind,
		       COALESCE(SUM(il.net_minor), 0), COALESCE(SUM(il.vat_minor), 0)
		FROM acct_invoice_lines il
		JOIN acct_invoices i ON i.id = il.invoice_id
		LEFT JOIN acct_vat_rates vr ON vr.id = il.vat_rate_id
		WHERE i.status <> 'draft' AND i.status <> 'void'
		  AND i.issue_date >= ? AND i.issue_date <= ?
		GROUP BY COALESCE(vr.kind, 'standard'), i.kind`, from, to)
	if err != nil {
		return v, fmt.Errorf("vat totals: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var kind, docKind string
		var net, vat Money
		if err := rows.Scan(&kind, &docKind, &net, &vat); err != nil {
			return v, fmt.Errorf("vat totals: %w", err)
		}
		if InvoiceKind(docKind) == KindCreditNote {
			net, vat = -net, -vat
		}
		switch VATKind(kind) {
		case VATZero:
			v.NetSalesZero += net
		case VATExempt:
			v.NetSalesExempt += net
		case VATReverseCharge:
			v.NetSalesReverse += net
		default:
			v.NetSalesStandard += net
		}
		v.OutputVAT += vat
	}
	if err := rows.Err(); err != nil {
		return v, err
	}

	// Purchases. Only reclaimable VAT counts as input tax; the rest was posted
	// into the cost and is not recoverable from anyone.
	err = r.db.QueryRow(`
		SELECT COALESCE(SUM(net_minor), 0),
		       COALESCE(SUM(CASE WHEN vat_reclaimable = 1 THEN vat_minor ELSE 0 END), 0)
		FROM acct_expenses
		WHERE spent_on >= ? AND spent_on <= ?`, from, to).
		Scan(&v.NetPurchases, &v.InputVAT)
	if err != nil {
		return v, fmt.Errorf("vat purchases: %w", err)
	}
	return v, nil
}

// VATControlTotals reads the same period straight off the two control accounts.
// Comparing this with the document totals is what catches VAT posted by hand.
func (r *SQLiteRepository) VATControlTotals(from, to string) (output, input Money, err error) {
	q := `SELECT COALESCE(SUM(l.credit_minor), 0) - COALESCE(SUM(l.debit_minor), 0)
	      FROM acct_journal_lines l
	      JOIN acct_journal_entries e ON e.id = l.entry_id
	      JOIN acct_accounts a ON a.id = l.account_id
	      WHERE a.system_key = ? AND e.entry_date >= ? AND e.entry_date <= ?`

	if err = r.db.QueryRow(q, SysVATOutput, from, to).Scan(&output); err != nil {
		return 0, 0, fmt.Errorf("vat control (output): %w", err)
	}
	// Input VAT is a debit balance sitting in a credit-normal account, so the
	// same query returns it negated.
	var inputCredit Money
	if err = r.db.QueryRow(q, SysVATInput, from, to).Scan(&inputCredit); err != nil {
		return 0, 0, fmt.Errorf("vat control (input): %w", err)
	}
	return output, -inputCredit, nil
}
