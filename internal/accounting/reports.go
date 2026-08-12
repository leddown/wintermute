package accounting

import (
	"fmt"
	"time"
)

// Reports read the ledger and nothing else. Not the invoice table, not the
// expense table — those are sources that already posted what they meant. A
// report that summed documents instead would quietly disagree with the ledger
// the moment anything was posted by hand, and the disagreement would be
// invisible until someone reconciled by eye.

// Open-ended date bounds. SQLite compares dates as strings, so these sort
// correctly at both extremes without special-casing the SQL.
const (
	dateFloor   = "0000-01-01"
	dateCeiling = "9999-12-31"
)

// ReportLine is one account's contribution to a statement, already signed in
// the direction the statement wants to read.
type ReportLine struct {
	AccountID   int64       `json:"account_id"`
	Code        string      `json:"code"`
	Name        string      `json:"name"`
	Type        AccountType `json:"type"`
	Amount      Money       `json:"amount"`
	AmountLabel string      `json:"amount_label"`
}

// TrialBalanceRow shows both sides, which is the point of a trial balance.
type TrialBalanceRow struct {
	AccountID int64       `json:"account_id"`
	Code      string      `json:"code"`
	Name      string      `json:"name"`
	Type      AccountType `json:"type"`
	Debit     Money       `json:"debit"`
	Credit    Money       `json:"credit"`
}

// TrialBalance is every account with a movement, and the proof that the two
// sides agree. Balanced is computed, not asserted: if it is ever false the
// ledger has been written to by something that bypassed this package.
type TrialBalance struct {
	From        string            `json:"from"`
	To          string            `json:"to"`
	Rows        []TrialBalanceRow `json:"rows"`
	TotalDebit  Money             `json:"total_debit"`
	TotalCredit Money             `json:"total_credit"`
	Balanced    bool              `json:"balanced"`
}

// ProfitAndLoss covers a window. Accrual basis: revenue when invoiced, cost
// when incurred, which is what the invoice-driven model already implies.
type ProfitAndLoss struct {
	From          string       `json:"from"`
	To            string       `json:"to"`
	Income        []ReportLine `json:"income"`
	Expenses      []ReportLine `json:"expenses"`
	TotalIncome   Money        `json:"total_income"`
	TotalExpenses Money        `json:"total_expenses"`
	NetProfit     Money        `json:"net_profit"`
}

// BalanceSheet is a position at a date.
type BalanceSheet struct {
	AsOf             string       `json:"as_of"`
	Assets           []ReportLine `json:"assets"`
	Liabilities      []ReportLine `json:"liabilities"`
	Equity           []ReportLine `json:"equity"`
	TotalAssets      Money        `json:"total_assets"`
	TotalLiabilities Money        `json:"total_liabilities"`
	TotalEquity      Money        `json:"total_equity"`
	// Profit earned since the last close and not yet moved to retained
	// earnings. Without it the sheet does not balance, because this module
	// posts no year-end closing entry.
	CurrentEarnings Money `json:"current_earnings"`
	Balanced        bool  `json:"balanced"`
}

// ---- trial balance ----

func (s *Service) TrialBalance(from, to string) (TrialBalance, error) {
	from, to = boundOrDefault(from, to)
	accounts, err := s.repo.Balances(from, to)
	if err != nil {
		return TrialBalance{}, err
	}

	tb := TrialBalance{From: from, To: to}
	for _, a := range accounts {
		// Net the two sides so an account shows on the side it actually sits,
		// rather than gross turnover on both.
		net := a.DebitTotal - a.CreditTotal
		// Skip both the untouched accounts and the ones whose activity cancels
		// out — a receivable raised and settled in the same window has moved
		// 1210 through it and holds nothing, and a trial balance lists balances.
		if net == 0 {
			continue
		}
		row := TrialBalanceRow{AccountID: a.ID, Code: a.Code, Name: a.Name, Type: a.Type}
		if net >= 0 {
			row.Debit = net
		} else {
			row.Credit = -net
		}
		tb.TotalDebit += row.Debit
		tb.TotalCredit += row.Credit
		tb.Rows = append(tb.Rows, row)
	}
	tb.Balanced = tb.TotalDebit == tb.TotalCredit
	return tb, nil
}

// ---- profit and loss ----

func (s *Service) ProfitAndLoss(from, to string) (ProfitAndLoss, error) {
	from, to = boundOrDefault(from, to)
	accounts, err := s.repo.Balances(from, to)
	if err != nil {
		return ProfitAndLoss{}, err
	}

	pl := ProfitAndLoss{From: from, To: to, Income: []ReportLine{}, Expenses: []ReportLine{}}
	for _, a := range accounts {
		switch a.Type {
		case AccountIncome:
			// Income is credit-normal, so a credit balance is positive revenue.
			amount := a.CreditTotal - a.DebitTotal
			if amount == 0 {
				continue
			}
			pl.Income = append(pl.Income, reportLine(a, amount))
			pl.TotalIncome += amount
		case AccountExpense:
			amount := a.DebitTotal - a.CreditTotal
			if amount == 0 {
				continue
			}
			pl.Expenses = append(pl.Expenses, reportLine(a, amount))
			pl.TotalExpenses += amount
		}
	}
	pl.NetProfit = pl.TotalIncome - pl.TotalExpenses
	return pl, nil
}

// ---- balance sheet ----

func (s *Service) BalanceSheet(asOf string) (BalanceSheet, error) {
	if asOf == "" {
		asOf = today()
	}
	if err := validDate(asOf); err != nil {
		return BalanceSheet{}, err
	}
	accounts, err := s.repo.Balances(dateFloor, asOf)
	if err != nil {
		return BalanceSheet{}, err
	}

	bs := BalanceSheet{
		AsOf: asOf, Assets: []ReportLine{}, Liabilities: []ReportLine{}, Equity: []ReportLine{},
	}
	var income, expense Money
	for _, a := range accounts {
		debitNet := a.DebitTotal - a.CreditTotal
		creditNet := -debitNet
		switch a.Type {
		case AccountAsset:
			if debitNet == 0 {
				continue
			}
			bs.Assets = append(bs.Assets, reportLine(a, debitNet))
			bs.TotalAssets += debitNet
		case AccountLiability:
			if creditNet == 0 {
				continue
			}
			bs.Liabilities = append(bs.Liabilities, reportLine(a, creditNet))
			bs.TotalLiabilities += creditNet
		case AccountEquity:
			if creditNet == 0 {
				continue
			}
			bs.Equity = append(bs.Equity, reportLine(a, creditNet))
			bs.TotalEquity += creditNet
		case AccountIncome:
			income += creditNet
		case AccountExpense:
			expense += debitNet
		}
	}

	bs.CurrentEarnings = income - expense
	bs.Balanced = bs.TotalAssets == bs.TotalLiabilities+bs.TotalEquity+bs.CurrentEarnings
	return bs, nil
}

func reportLine(a Account, amount Money) ReportLine {
	return ReportLine{
		AccountID: a.ID, Code: a.Code, Name: a.Name, Type: a.Type,
		Amount: amount, AmountLabel: amount.String(),
	}
}

func boundOrDefault(from, to string) (string, string) {
	if from == "" {
		from = dateFloor
	}
	if to == "" {
		to = dateCeiling
	}
	return from, to
}

// ---- accounts receivable aging ----

// ARBuckets is the standard 30-day ladder. "Current" is not yet due; the rest
// count days past the due date.
type ARBuckets struct {
	Current Money `json:"current"`
	Days30  Money `json:"days_1_30"`
	Days60  Money `json:"days_31_60"`
	Days90  Money `json:"days_61_90"`
	Older   Money `json:"days_90_plus"`
	Total   Money `json:"total"`
}

func (b *ARBuckets) add(daysOverdue int, amount Money) {
	switch {
	case daysOverdue <= 0:
		b.Current += amount
	case daysOverdue <= 30:
		b.Days30 += amount
	case daysOverdue <= 60:
		b.Days60 += amount
	case daysOverdue <= 90:
		b.Days90 += amount
	default:
		b.Older += amount
	}
	b.Total += amount
}

// ARAgingRow is one client's outstanding position.
type ARAgingRow struct {
	ClientID   int64     `json:"client_id"`
	ClientName string    `json:"client_name"`
	Buckets    ARBuckets `json:"buckets"`
	Invoices   []ARItem  `json:"invoices"`
}

// ARItem is a single unpaid document.
type ARItem struct {
	InvoiceID   int64  `json:"invoice_id"`
	Number      string `json:"number"`
	IssueDate   string `json:"issue_date"`
	DueDate     string `json:"due_date"`
	Total       Money  `json:"total"`
	Paid        Money  `json:"paid"`
	Outstanding Money  `json:"outstanding"`
	DaysOverdue int    `json:"days_overdue"`
}

// ARAging is who owes what, and for how long.
type ARAging struct {
	AsOf   string       `json:"as_of"`
	Rows   []ARAgingRow `json:"rows"`
	Totals ARBuckets    `json:"totals"`
}

// AgedReceivables buckets everything issued and not settled.
//
// Credit notes are included with their negative outstanding, so a client who
// has been over-invoiced and credited shows a net position rather than a debt
// alongside an invisible offset.
func (s *Service) AgedReceivables(asOf string) (ARAging, error) {
	if asOf == "" {
		asOf = today()
	}
	if err := validDate(asOf); err != nil {
		return ARAging{}, err
	}
	invoices, err := s.repo.OutstandingInvoices(asOf)
	if err != nil {
		return ARAging{}, err
	}

	ref, _ := time.Parse("2006-01-02", asOf)
	byClient := map[int64]*ARAgingRow{}
	order := []int64{}
	aging := ARAging{AsOf: asOf, Rows: []ARAgingRow{}}

	for _, inv := range invoices {
		outstanding := inv.Outstanding()
		if outstanding == 0 {
			continue
		}
		days := 0
		if due, err := time.Parse("2006-01-02", inv.DueDate); err == nil {
			days = int(ref.Sub(due).Hours() / 24)
		}

		row, ok := byClient[inv.ClientID]
		if !ok {
			row = &ARAgingRow{ClientID: inv.ClientID, ClientName: inv.ClientName}
			byClient[inv.ClientID] = row
			order = append(order, inv.ClientID)
		}
		row.Buckets.add(days, outstanding)
		row.Invoices = append(row.Invoices, ARItem{
			InvoiceID: inv.ID, Number: inv.Number, IssueDate: inv.IssueDate,
			DueDate: inv.DueDate, Total: inv.Total, Paid: inv.Paid,
			Outstanding: outstanding, DaysOverdue: days,
		})
		aging.Totals.add(days, outstanding)
	}

	for _, id := range order {
		aging.Rows = append(aging.Rows, *byClient[id])
	}
	return aging, nil
}

// ---- VAT ----

// VATReturn is the figures a return is filled in from. It is a summary, not a
// filing: nothing here submits anything anywhere, and the boxes on the actual
// form differ by member state.
type VATReturn struct {
	From string `json:"from"`
	To   string `json:"to"`

	// Sales, split the way a return asks for them.
	NetSalesStandard Money `json:"net_sales_standard"`
	NetSalesZero     Money `json:"net_sales_zero"`
	NetSalesExempt   Money `json:"net_sales_exempt"`
	NetSalesReverse  Money `json:"net_sales_reverse_charge"`
	OutputVAT        Money `json:"output_vat"`

	NetPurchases Money `json:"net_purchases"`
	InputVAT     Money `json:"input_vat"`

	// Positive means payable to the authority, negative means reclaimable.
	NetDue Money `json:"net_due"`

	Note string `json:"note"`
}

// VATReturnSummary totals the period from the documents, because a return needs
// the net amounts split by treatment and the ledger only carries the tax. The
// VAT figures are cross-checked against the control accounts, and a mismatch is
// reported rather than hidden — it means something was posted to a VAT account
// by hand.
func (s *Service) VATReturnSummary(from, to string) (VATReturn, error) {
	if err := validDate(from); err != nil {
		return VATReturn{}, err
	}
	if err := validDate(to); err != nil {
		return VATReturn{}, err
	}
	if to < from {
		return VATReturn{}, invalid("the period ends (%s) before it starts (%s)", to, from)
	}

	v, err := s.repo.VATTotals(from, to)
	if err != nil {
		return VATReturn{}, err
	}
	v.From, v.To = from, to
	v.NetDue = v.OutputVAT - v.InputVAT

	ledgerOut, ledgerIn, err := s.repo.VATControlTotals(from, to)
	if err != nil {
		return VATReturn{}, err
	}
	if ledgerOut != v.OutputVAT || ledgerIn != v.InputVAT {
		v.Note = fmt.Sprintf(
			"The VAT control accounts hold %s output and %s input, against %s and %s on the "+
				"documents. The difference is manual journal activity on those accounts — "+
				"check it before filing.",
			ledgerOut, ledgerIn, v.OutputVAT, v.InputVAT)
	}
	return v, nil
}

// ---- dashboard ----

// Dashboard is the landing summary: what is owed, what was earned, what is
// waiting to be billed.
type Dashboard struct {
	AsOf string `json:"as_of"`

	OutstandingTotal   Money `json:"outstanding_total"`
	OverdueTotal       Money `json:"overdue_total"`
	DraftInvoiceCount  int   `json:"draft_invoice_count"`
	UnbilledTimeAmount Money `json:"unbilled_time_amount"`
	UnbilledTimeHours  Milli `json:"unbilled_time_hours"`

	IncomeThisYear  Money `json:"income_this_year"`
	ExpenseThisYear Money `json:"expense_this_year"`
	ProfitThisYear  Money `json:"profit_this_year"`

	BankBalance Money `json:"bank_balance"`
	VATPosition Money `json:"vat_position"`

	RecentInvoices []Invoice `json:"recent_invoices"`
}

func (s *Service) Dashboard() (Dashboard, error) {
	now := today()
	d := Dashboard{AsOf: now, RecentInvoices: []Invoice{}}

	settings, err := s.repo.Settings()
	if err != nil {
		return Dashboard{}, err
	}
	yearStart := fiscalYearStart(now, settings.FiscalYearStartMonth)

	pl, err := s.ProfitAndLoss(yearStart, now)
	if err != nil {
		return Dashboard{}, err
	}
	d.IncomeThisYear, d.ExpenseThisYear, d.ProfitThisYear =
		pl.TotalIncome, pl.TotalExpenses, pl.NetProfit

	aging, err := s.AgedReceivables(now)
	if err != nil {
		return Dashboard{}, err
	}
	d.OutstandingTotal = aging.Totals.Total
	d.OverdueTotal = aging.Totals.Total - aging.Totals.Current

	unbilled, err := s.repo.UnbilledTime(UnbilledFilter{})
	if err != nil {
		return Dashboard{}, err
	}
	for _, u := range unbilled {
		d.UnbilledTimeAmount += u.Amount
		d.UnbilledTimeHours += u.Hours
	}

	drafts, err := s.repo.ListInvoices(InvoiceFilter{Status: StatusDraft, Limit: 500})
	if err != nil {
		return Dashboard{}, err
	}
	d.DraftInvoiceCount = len(drafts)

	balances, err := s.repo.Balances(dateFloor, now)
	if err != nil {
		return Dashboard{}, err
	}
	for _, a := range balances {
		switch a.SystemKey {
		case SysBank:
			d.BankBalance = a.DebitTotal - a.CreditTotal
		case SysVATOutput:
			d.VATPosition += a.CreditTotal - a.DebitTotal
		case SysVATInput:
			d.VATPosition -= a.DebitTotal - a.CreditTotal
		}
	}

	recent, err := s.repo.ListInvoices(InvoiceFilter{Limit: 8})
	if err != nil {
		return Dashboard{}, err
	}
	d.RecentInvoices = recent
	return d, nil
}

// fiscalYearStart returns the first day of the fiscal year containing date.
func fiscalYearStart(date string, startMonth int) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	if startMonth < 1 || startMonth > 12 {
		startMonth = 1
	}
	year := t.Year()
	if int(t.Month()) < startMonth {
		year--
	}
	return fmt.Sprintf("%04d-%02d-01", year, startMonth)
}
