package accounting

import (
	"strings"
	"testing"
)

// issuedInvoice sets up a client and an issued invoice for the payment tests.
func issuedInvoice(t *testing.T, svc *Service, repo *SQLiteRepository, amount Money) Invoice {
	t.Helper()
	client := newClient(t, repo, "Acme")
	d, err := svc.CreateDraft(draftFor(client, line("Work", 1000, amount)))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := svc.Issue(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func netByAccount(t *testing.T, svc *Service, entryID int64) map[int64]Money {
	t.Helper()
	e, err := svc.GetEntry(entryID)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Balanced() {
		t.Errorf("entry %d does not balance", entryID)
	}
	out := map[int64]Money{}
	for _, l := range e.Lines {
		out[l.AccountID] += l.Debit - l.Credit
	}
	return out
}

func TestRecordPaymentPostsAndUpdatesStatus(t *testing.T) {
	svc, repo := newTestService(t)
	inv := issuedInvoice(t, svc, repo, 100000) // 1000.00 net, 1210.00 gross

	p, err := svc.RecordPayment(Payment{InvoiceID: inv.ID, Amount: 50000, PaidOn: "2026-08-20"})
	if err != nil {
		t.Fatalf("record payment: %v", err)
	}

	bank := mustAccount(t, svc, SysBank)
	ar := mustAccount(t, svc, SysAR)
	net := netByAccount(t, svc, p.JournalEntry)
	if net[bank.ID] != 50000 {
		t.Errorf("bank = %s, want debit 500.00", net[bank.ID])
	}
	if net[ar.ID] != -50000 {
		t.Errorf("AR = %s, want credit 500.00", net[ar.ID])
	}

	after, err := svc.GetInvoice(inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusPartPaid {
		t.Errorf("status = %s, want part_paid", after.Status)
	}
	if after.Paid != 50000 {
		t.Errorf("paid = %s, want 500.00", after.Paid)
	}

	// Settle the rest.
	if _, err := svc.RecordPayment(Payment{InvoiceID: inv.ID, Amount: after.Outstanding()}); err != nil {
		t.Fatal(err)
	}
	settled, _ := svc.GetInvoice(inv.ID)
	if settled.Status != StatusPaid {
		t.Errorf("status = %s, want paid", settled.Status)
	}
	if settled.Outstanding() != 0 {
		t.Errorf("outstanding = %s, want zero", settled.Outstanding())
	}
}

func TestPaymentRefusals(t *testing.T) {
	svc, repo := newTestService(t)
	inv := issuedInvoice(t, svc, repo, 100000)

	// Overpaying is refused rather than absorbed: the excess needs a decision,
	// and silently posting it would leave receivables wrong.
	if _, err := svc.RecordPayment(Payment{InvoiceID: inv.ID, Amount: inv.Total + 1}); err == nil {
		t.Error("an overpayment was accepted")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("unexpected error: %v", err)
	}

	if _, err := svc.RecordPayment(Payment{InvoiceID: inv.ID, Amount: 0}); err == nil {
		t.Error("a zero payment was accepted")
	}

	// Against a draft there is nothing owed yet.
	client := newClient(t, repo, "Globex")
	draft, _ := svc.CreateDraft(draftFor(client, line("Work", 1000, 10000)))
	if _, err := svc.RecordPayment(Payment{InvoiceID: draft.ID, Amount: 100}); err == nil {
		t.Error("payment against a draft was accepted")
	}

	// Payments must land in an asset account.
	rent := accountByCode(t, repo, "6100")
	if _, err := svc.RecordPayment(Payment{
		InvoiceID: inv.ID, Amount: 100, DepositAccountID: rent,
	}); err == nil {
		t.Error("a payment into an expense account was accepted")
	}
}

func TestDeletePaymentReversesAndRestoresStatus(t *testing.T) {
	svc, repo := newTestService(t)
	inv := issuedInvoice(t, svc, repo, 100000)

	p, err := svc.RecordPayment(Payment{InvoiceID: inv.ID, Amount: inv.Total})
	if err != nil {
		t.Fatal(err)
	}
	paid, _ := svc.GetInvoice(inv.ID)
	if paid.Status != StatusPaid {
		t.Fatalf("setup: status %s", paid.Status)
	}

	if err := svc.DeletePayment(p.ID); err != nil {
		t.Fatalf("delete payment: %v", err)
	}

	back, _ := svc.GetInvoice(inv.ID)
	if back.Status != StatusIssued {
		t.Errorf("status = %s, want issued again", back.Status)
	}
	if back.Paid != 0 {
		t.Errorf("paid = %s, want zero", back.Paid)
	}

	// The ledger keeps both the payment and its reversal; the pair nets to zero
	// rather than the original entry vanishing.
	entries, err := svc.ListEntries(EntryFilter{SourceType: SourcePayment})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("want the payment and its reversal, got %d entries", len(entries))
	}
	net := map[int64]Money{}
	for _, e := range entries {
		for _, l := range e.Lines {
			net[l.AccountID] += l.Debit - l.Credit
		}
	}
	for acct, n := range net {
		if n != 0 {
			t.Errorf("account %d holds %s after the reversal", acct, n)
		}
	}
}

func TestExpenseSplitsReclaimableVAT(t *testing.T) {
	svc, repo := newTestService(t)
	software := accountByCode(t, repo, "6200")
	std := vatRateByCode(t, repo, "STD")

	e, err := svc.RecordExpense(Expense{
		SpentOn: "2026-08-10", Vendor: "Hosting Co", AccountID: software,
		Net: 10000, VATRateID: std, VATReclaimable: true,
	})
	if err != nil {
		t.Fatalf("record expense: %v", err)
	}
	if e.VAT != 2100 || e.Total != 12100 {
		t.Errorf("VAT %s total %s, want 21.00 and 121.00", e.VAT, e.Total)
	}

	bank := mustAccount(t, svc, SysBank)
	vatIn := mustAccount(t, svc, SysVATInput)
	net := netByAccount(t, svc, e.JournalEntry)

	if net[software] != 10000 {
		t.Errorf("expense account = %s, want debit 100.00 net of VAT", net[software])
	}
	if net[vatIn.ID] != 2100 {
		t.Errorf("input VAT = %s, want debit 21.00", net[vatIn.ID])
	}
	if net[bank.ID] != -12100 {
		t.Errorf("bank = %s, want credit 121.00", net[bank.ID])
	}
}

// Irrecoverable VAT is part of the cost, not a claim against the authority.
func TestExpenseWithIrrecoverableVATCapitalisesIt(t *testing.T) {
	svc, repo := newTestService(t)
	meals := accountByCode(t, repo, "6410")
	std := vatRateByCode(t, repo, "STD")

	e, err := svc.RecordExpense(Expense{
		SpentOn: "2026-08-10", Vendor: "Restaurant", AccountID: meals,
		Net: 10000, VATRateID: std, VATReclaimable: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	vatIn := mustAccount(t, svc, SysVATInput)
	net := netByAccount(t, svc, e.JournalEntry)
	if net[meals] != 12100 {
		t.Errorf("expense = %s, want the full 121.00 including VAT", net[meals])
	}
	if _, touched := net[vatIn.ID]; touched {
		t.Error("irrecoverable VAT reached the input VAT account")
	}
}

func TestExpenseCategoryRules(t *testing.T) {
	svc, repo := newTestService(t)
	sales := mustAccount(t, svc, SysSales)
	laptop := accountByCode(t, repo, "1500") // asset

	// Income is not a category for money going out.
	if _, err := svc.RecordExpense(Expense{
		Vendor: "X", AccountID: sales.ID, Net: 100,
	}); err == nil {
		t.Error("an income account was accepted as an expense category")
	}

	// An asset is: buying a laptop is money out that lands on the balance sheet.
	if _, err := svc.RecordExpense(Expense{
		Vendor: "Laptop shop", AccountID: laptop, Net: 150000,
	}); err != nil {
		t.Errorf("capitalising an asset purchase failed: %v", err)
	}

	// A billable expense with nobody to bill would be lost silently.
	if _, err := svc.RecordExpense(Expense{
		Vendor: "Travel", AccountID: accountByCode(t, repo, "6400"), Net: 5000, Billable: true,
	}); err == nil {
		t.Error("a billable expense without a client was accepted")
	}
}

func TestExpensePaidFromCreditCardIsALiability(t *testing.T) {
	svc, repo := newTestService(t)
	software := accountByCode(t, repo, "6200")
	card := accountByCode(t, repo, "2200")

	e, err := svc.RecordExpense(Expense{
		Vendor: "SaaS", AccountID: software, PaidFromID: card, Net: 5000,
	})
	if err != nil {
		t.Fatalf("card expense: %v", err)
	}
	net := netByAccount(t, svc, e.JournalEntry)
	// The card balance grows: a credit to a liability.
	if net[card] != -5000 {
		t.Errorf("credit card = %s, want credit 50.00", net[card])
	}
}

func vatRateByCode(t *testing.T, repo *SQLiteRepository, code string) int64 {
	t.Helper()
	var id int64
	if err := repo.db.QueryRow(`SELECT id FROM acct_vat_rates WHERE code = ?`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
