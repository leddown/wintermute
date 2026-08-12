package accounting

import "testing"

// A full quarter in miniature: bill some work, get paid, buy something. Every
// report is then checked against figures worked out by hand, because a report
// that agrees with the code that produced it proves nothing.
func seedQuarter(t *testing.T, svc *Service, repo *SQLiteRepository) {
	t.Helper()
	client := newClient(t, repo, "Acme")

	// Invoice 1000.00 net, 210.00 VAT, 1210.00 gross.
	d, err := svc.CreateDraft(draftFor(client, line("Consulting", 1000, 100000)))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := svc.Issue(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordPayment(Payment{
		InvoiceID: inv.ID, Amount: inv.Total, PaidOn: "2026-08-20",
	}); err != nil {
		t.Fatal(err)
	}

	// Expense 100.00 net, 21.00 recoverable VAT, 121.00 out of the bank.
	if _, err := svc.RecordExpense(Expense{
		SpentOn: "2026-08-15", Vendor: "Hosting", AccountID: accountByCode(t, repo, "6200"),
		Net: 10000, VATRateID: vatRateByCode(t, repo, "STD"), VATReclaimable: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTrialBalanceBalances(t *testing.T) {
	svc, repo := newTestService(t)
	seedQuarter(t, svc, repo)

	tb, err := svc.TrialBalance("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !tb.Balanced {
		t.Errorf("trial balance does not balance: debits %s, credits %s",
			tb.TotalDebit, tb.TotalCredit)
	}
	if len(tb.Rows) == 0 {
		t.Fatal("trial balance is empty")
	}
	// Accounts nobody used must not appear as rows of zeros.
	for _, r := range tb.Rows {
		if r.Debit == 0 && r.Credit == 0 {
			t.Errorf("account %s has a zero row", r.Code)
		}
	}
}

func TestProfitAndLoss(t *testing.T) {
	svc, repo := newTestService(t)
	seedQuarter(t, svc, repo)

	pl, err := svc.ProfitAndLoss("2026-01-01", "2026-12-31")
	if err != nil {
		t.Fatal(err)
	}
	if pl.TotalIncome != 100000 {
		t.Errorf("income = %s, want 1000.00 (net of VAT)", pl.TotalIncome)
	}
	if pl.TotalExpenses != 10000 {
		t.Errorf("expenses = %s, want 100.00 (net of recoverable VAT)", pl.TotalExpenses)
	}
	if pl.NetProfit != 90000 {
		t.Errorf("profit = %s, want 900.00", pl.NetProfit)
	}

	// VAT is a liability and a receivable, never income or cost.
	for _, l := range append(pl.Income, pl.Expenses...) {
		if l.Code == "2100" || l.Code == "2110" {
			t.Errorf("VAT account %s appeared in the P&L", l.Code)
		}
	}
}

// The window must actually filter.
func TestProfitAndLossRespectsItsPeriod(t *testing.T) {
	svc, repo := newTestService(t)
	seedQuarter(t, svc, repo)

	pl, err := svc.ProfitAndLoss("2026-01-01", "2026-06-30")
	if err != nil {
		t.Fatal(err)
	}
	if pl.TotalIncome != 0 || pl.TotalExpenses != 0 {
		t.Errorf("a period before any activity reported income %s and expenses %s",
			pl.TotalIncome, pl.TotalExpenses)
	}
}

func TestBalanceSheetBalances(t *testing.T) {
	svc, repo := newTestService(t)
	seedQuarter(t, svc, repo)

	bs, err := svc.BalanceSheet("2026-12-31")
	if err != nil {
		t.Fatal(err)
	}

	// Bank: 1210.00 in, 121.00 out.
	if bs.TotalAssets != 108900 {
		t.Errorf("assets = %s, want 1089.00", bs.TotalAssets)
	}
	// VAT owed 210.00 less VAT reclaimable 21.00.
	if bs.TotalLiabilities != 18900 {
		t.Errorf("liabilities = %s, want 189.00", bs.TotalLiabilities)
	}
	// Profit not yet closed to retained earnings.
	if bs.CurrentEarnings != 90000 {
		t.Errorf("current earnings = %s, want 900.00", bs.CurrentEarnings)
	}
	if !bs.Balanced {
		t.Errorf("balance sheet does not balance: assets %s against %s + %s + %s",
			bs.TotalAssets, bs.TotalLiabilities, bs.TotalEquity, bs.CurrentEarnings)
	}
}

// AR must be empty once everything is settled, and the receivable must have
// existed before that.
func TestAgedReceivables(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	// Issued 60 days before the reporting date, 14-day terms: 46 days overdue.
	d, err := svc.CreateDraft(Invoice{
		ClientID:  client,
		IssueDate: "2026-06-01",
		Lines:     []InvoiceLine{line("Work", 1000, 100000)},
	})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := svc.Issue(d.ID)
	if err != nil {
		t.Fatal(err)
	}

	aging, err := svc.AgedReceivables("2026-07-31")
	if err != nil {
		t.Fatal(err)
	}
	if aging.Totals.Total != inv.Total {
		t.Errorf("total = %s, want %s", aging.Totals.Total, inv.Total)
	}
	// Due 2026-06-15, reported 2026-07-31: 46 days.
	if aging.Totals.Days60 != inv.Total {
		t.Errorf("expected the whole balance in the 31–60 bucket, got %+v", aging.Totals)
	}
	if len(aging.Rows) != 1 || aging.Rows[0].ClientName != "Acme" {
		t.Fatalf("expected one client row, got %+v", aging.Rows)
	}
	if len(aging.Rows[0].Invoices) != 1 || aging.Rows[0].Invoices[0].DaysOverdue != 46 {
		t.Errorf("invoice detail wrong: %+v", aging.Rows[0].Invoices)
	}

	// Not yet due at issue time.
	early, err := svc.AgedReceivables("2026-06-02")
	if err != nil {
		t.Fatal(err)
	}
	if early.Totals.Current != inv.Total {
		t.Errorf("an invoice inside its terms should sit in Current, got %+v", early.Totals)
	}

	// Settled: nothing outstanding.
	if _, err := svc.RecordPayment(Payment{
		InvoiceID: inv.ID, Amount: inv.Total, PaidOn: "2026-07-31",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := svc.AgedReceivables("2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	if after.Totals.Total != 0 {
		t.Errorf("paid invoice still showing %s outstanding", after.Totals.Total)
	}
}

func TestVATReturnSummary(t *testing.T) {
	svc, repo := newTestService(t)
	seedQuarter(t, svc, repo)

	v, err := svc.VATReturnSummary("2026-07-01", "2026-09-30")
	if err != nil {
		t.Fatal(err)
	}
	if v.NetSalesStandard != 100000 {
		t.Errorf("standard-rated sales = %s, want 1000.00", v.NetSalesStandard)
	}
	if v.OutputVAT != 21000 {
		t.Errorf("output VAT = %s, want 210.00", v.OutputVAT)
	}
	if v.InputVAT != 2100 {
		t.Errorf("input VAT = %s, want 21.00", v.InputVAT)
	}
	if v.NetDue != 18900 {
		t.Errorf("net due = %s, want 189.00", v.NetDue)
	}
	// Documents and control accounts agree, so there is nothing to warn about.
	if v.Note != "" {
		t.Errorf("unexpected reconciliation note: %s", v.Note)
	}
}

// VAT posted by hand must be surfaced, not silently absorbed into the return.
func TestVATReturnFlagsManualJournalActivity(t *testing.T) {
	svc, repo := newTestService(t)
	seedQuarter(t, svc, repo)

	vatOut := mustAccount(t, svc, SysVATOutput)
	bank := mustAccount(t, svc, SysBank)
	if _, err := svc.PostFrom("2026-08-25", "adjustment", SourceManual, 0, []Posting{
		Debit(bank.ID, 5000, ""),
		Credit(vatOut.ID, 5000, ""),
	}); err != nil {
		t.Fatal(err)
	}

	v, err := svc.VATReturnSummary("2026-07-01", "2026-09-30")
	if err != nil {
		t.Fatal(err)
	}
	if v.Note == "" {
		t.Error("a manual posting to the VAT control account was not flagged")
	}
}

func TestReverseChargeSalesReportSeparately(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Bruxelles SA")

	in := draftFor(client, line("Advisory", 1000, 100000))
	in.ReverseCharge = true
	in.CustomerVAT = "BE0123456789"
	d, err := svc.CreateDraft(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Issue(d.ID); err != nil {
		t.Fatal(err)
	}

	v, err := svc.VATReturnSummary("2026-01-01", "2026-12-31")
	if err != nil {
		t.Fatal(err)
	}
	if v.NetSalesReverse != 100000 {
		t.Errorf("reverse-charge sales = %s, want 1000.00", v.NetSalesReverse)
	}
	if v.NetSalesStandard != 0 {
		t.Errorf("reverse-charge sales leaked into standard-rated: %s", v.NetSalesStandard)
	}
	if v.OutputVAT != 0 {
		t.Errorf("output VAT = %s, want zero on a reverse-charge supply", v.OutputVAT)
	}
}

// A credit note has to pull the quarter back down, or a corrected period
// overstates both revenue and the VAT owed on it.
func TestCreditNoteReducesTheVATReturn(t *testing.T) {
	svc, repo := newTestService(t)
	seedQuarter(t, svc, repo)

	invoices, err := svc.ListInvoices(InvoiceFilter{Kind: KindInvoice})
	if err != nil || len(invoices) == 0 {
		t.Fatalf("setup: %v", err)
	}
	note, err := svc.CreditNote(invoices[0].ID, nil, "cancelled scope")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Issue(note.ID); err != nil {
		t.Fatal(err)
	}

	v, err := svc.VATReturnSummary("2026-01-01", "2026-12-31")
	if err != nil {
		t.Fatal(err)
	}
	if v.NetSalesStandard != 0 {
		t.Errorf("net sales = %s, want zero after a full credit", v.NetSalesStandard)
	}
	if v.OutputVAT != 0 {
		t.Errorf("output VAT = %s, want zero after a full credit", v.OutputVAT)
	}
}

// The dashboard reports "as of now", so the clock is pinned. Without this the
// test passes or fails depending on the date it is run: the seeded payment is
// dated 2026-08-20, and a run before that sees a bank balance that has not
// happened yet.
func pinToday(t *testing.T, date string) {
	t.Helper()
	prev := today
	today = func() string { return date }
	t.Cleanup(func() { today = prev })
}

func TestDashboard(t *testing.T) {
	svc, repo := newTestService(t)
	pinToday(t, "2026-09-30")
	seedQuarter(t, svc, repo)

	// Some unbilled time waiting to go out.
	client := newClient(t, repo, "Globex")
	eng := newEngagement(t, repo, client, "Retainer", 100)
	newTimeEntry(t, repo, eng, "2026-08-01", 5, 100, "Advisory")

	d, err := svc.Dashboard()
	if err != nil {
		t.Fatal(err)
	}
	if d.UnbilledTimeAmount != 50000 {
		t.Errorf("unbilled = %s, want 500.00", d.UnbilledTimeAmount)
	}
	if d.UnbilledTimeHours != 5000 {
		t.Errorf("unbilled hours = %s, want 5", d.UnbilledTimeHours)
	}
	if d.BankBalance != 108900 {
		t.Errorf("bank = %s, want 1089.00", d.BankBalance)
	}
	if d.VATPosition != 18900 {
		t.Errorf("VAT position = %s, want 189.00 payable", d.VATPosition)
	}
	if d.OutstandingTotal != 0 {
		t.Errorf("outstanding = %s, want zero once paid", d.OutstandingTotal)
	}
}

func TestFiscalYearStart(t *testing.T) {
	cases := []struct {
		date  string
		month int
		want  string
	}{
		{"2026-08-12", 1, "2026-01-01"},
		{"2026-08-12", 4, "2026-04-01"},
		{"2026-02-12", 4, "2025-04-01"}, // before the start, so the prior year
		{"2026-04-01", 4, "2026-04-01"},
	}
	for _, c := range cases {
		if got := fiscalYearStart(c.date, c.month); got != c.want {
			t.Errorf("fiscalYearStart(%s, %d) = %s, want %s", c.date, c.month, got, c.want)
		}
	}
}
