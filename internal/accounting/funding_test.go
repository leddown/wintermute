package accounting

import (
	"strings"
	"testing"
)

// The opening move of an actual business: the owner puts their own money in
// before there is anything to invoice. Capital is equity, so it lands in 3000
// and the business does not owe it back.
func TestCapitalIntroducedPostsToEquity(t *testing.T) {
	svc, _ := newTestService(t)

	f, err := svc.RecordFunding(Funding{
		Kind:       FundingCapital,
		Amount:     2500000, // 25,000.00
		ReceivedOn: "2026-01-02",
		FromName:   "Founder",
		Reference:  "opening deposit",
	})
	if err != nil {
		t.Fatalf("record capital: %v", err)
	}

	bank := mustAccount(t, svc, SysBank)
	capital := mustAccount(t, svc, SysCapital)
	net := netByAccount(t, svc, f.JournalEntry)
	if net[bank.ID] != 2500000 {
		t.Errorf("bank = %s, want debit 25,000.00", net[bank.ID])
	}
	if net[capital.ID] != -2500000 {
		t.Errorf("owner capital = %s, want credit 25,000.00", net[capital.ID])
	}

	// Capital is not a debt, so it must not show up as one.
	owed, err := svc.OwnerLoanOutstanding("2026-01-02")
	if err != nil {
		t.Fatal(err)
	}
	if owed != 0 {
		t.Errorf("loan outstanding = %s after a capital contribution, want zero", owed)
	}
}

// A loan is the same money moving the same way, and a different fact: the
// business owes it. That is the whole reason kind is stored per event.
func TestLoanPostsToLiabilityAndIsOwed(t *testing.T) {
	svc, _ := newTestService(t)

	f, err := svc.RecordFunding(Funding{
		Kind:       FundingLoan,
		Amount:     500000, // 5,000.00
		ReceivedOn: "2026-02-01",
		FromName:   "Founder",
	})
	if err != nil {
		t.Fatalf("record loan: %v", err)
	}

	bank := mustAccount(t, svc, SysBank)
	loan := mustAccount(t, svc, SysOwnerLoan)
	net := netByAccount(t, svc, f.JournalEntry)
	if net[bank.ID] != 500000 {
		t.Errorf("bank = %s, want debit 5,000.00", net[bank.ID])
	}
	if net[loan.ID] != -500000 {
		t.Errorf("owner loan = %s, want credit 5,000.00", net[loan.ID])
	}

	owed, err := svc.OwnerLoanOutstanding("2026-02-01")
	if err != nil {
		t.Fatal(err)
	}
	if owed != 500000 {
		t.Errorf("loan outstanding = %s, want 5,000.00", owed)
	}
}

func TestRepaymentReversesTheLoanAndReducesWhatIsOwed(t *testing.T) {
	svc, _ := newTestService(t)

	if _, err := svc.RecordFunding(Funding{
		Kind: FundingLoan, Amount: 500000, ReceivedOn: "2026-02-01",
	}); err != nil {
		t.Fatal(err)
	}

	r, err := svc.RecordFunding(Funding{
		Kind: FundingRepayment, Amount: 200000, ReceivedOn: "2026-03-01",
	})
	if err != nil {
		t.Fatalf("record repayment: %v", err)
	}

	bank := mustAccount(t, svc, SysBank)
	loan := mustAccount(t, svc, SysOwnerLoan)
	net := netByAccount(t, svc, r.JournalEntry)
	if net[loan.ID] != 200000 {
		t.Errorf("owner loan = %s, want debit 2,000.00", net[loan.ID])
	}
	if net[bank.ID] != -200000 {
		t.Errorf("bank = %s, want credit 2,000.00", net[bank.ID])
	}

	owed, err := svc.OwnerLoanOutstanding("2026-03-01")
	if err != nil {
		t.Fatal(err)
	}
	if owed != 300000 {
		t.Errorf("loan outstanding = %s, want 3,000.00", owed)
	}
}

// Overpaying a loan back would leave the liability with a debit balance — the
// business appearing to have lent the owner money it never received.
func TestRepaymentCannotExceedWhatIsOutstanding(t *testing.T) {
	svc, _ := newTestService(t)

	if _, err := svc.RecordFunding(Funding{
		Kind: FundingLoan, Amount: 100000, ReceivedOn: "2026-02-01",
	}); err != nil {
		t.Fatal(err)
	}

	_, err := svc.RecordFunding(Funding{
		Kind: FundingRepayment, Amount: 100001, ReceivedOn: "2026-02-02",
	})
	if err == nil {
		t.Fatal("a repayment larger than the loan was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("unexpected error: %v", err)
	}

	// And with nothing lent at all there is nothing to repay.
	svc2, _ := newTestService(t)
	if _, err := svc2.RecordFunding(Funding{
		Kind: FundingRepayment, Amount: 100, ReceivedOn: "2026-02-02",
	}); err == nil {
		t.Error("a repayment against no loan was accepted")
	}
}

// The guard reads the ledger as of the repayment date, so a loan made after it
// does not retroactively fund it.
func TestRepaymentIgnoresLaterLoans(t *testing.T) {
	svc, _ := newTestService(t)

	if _, err := svc.RecordFunding(Funding{
		Kind: FundingLoan, Amount: 100000, ReceivedOn: "2026-06-01",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordFunding(Funding{
		Kind: FundingRepayment, Amount: 100000, ReceivedOn: "2026-03-01",
	}); err == nil {
		t.Error("a repayment dated before the loan it draws on was accepted")
	}
}

func TestFundingRefusals(t *testing.T) {
	svc, _ := newTestService(t)

	// No kind. Guessing here would misstate either equity or liabilities, and
	// the mistake surfaces at tax time rather than at the call site.
	if _, err := svc.RecordFunding(Funding{Amount: 1000}); err == nil {
		t.Error("funding with no kind was accepted")
	}
	if _, err := svc.RecordFunding(Funding{Kind: "gift", Amount: 1000}); err == nil {
		t.Error("an unknown funding kind was accepted")
	}
	if _, err := svc.RecordFunding(Funding{Kind: FundingCapital, Amount: 0}); err == nil {
		t.Error("zero-value funding was accepted")
	}
	if _, err := svc.RecordFunding(Funding{Kind: FundingCapital, Amount: -500}); err == nil {
		t.Error("negative funding was accepted")
	}

	// Money has to move through an asset account.
	sales := mustAccount(t, svc, SysSales)
	if _, err := svc.RecordFunding(Funding{
		Kind: FundingCapital, Amount: 1000, CashAccountID: sales.ID,
	}); err == nil {
		t.Error("funding into an income account was accepted")
	}

	// And the owner side has to match what the kind means: capital is equity,
	// so pointing it at the loan liability is refused.
	loan := mustAccount(t, svc, SysOwnerLoan)
	if _, err := svc.RecordFunding(Funding{
		Kind: FundingCapital, Amount: 1000, OwnerAccountID: loan.ID,
	}); err == nil {
		t.Error("capital posted to a liability account was accepted")
	}
}

func TestFundingRespectsLockedPeriods(t *testing.T) {
	svc, _ := newTestService(t)

	if _, err := svc.LockPeriod(Period{
		StartsOn: "2026-01-01", EndsOn: "2026-01-31", Locked: true,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.RecordFunding(Funding{
		Kind: FundingCapital, Amount: 100000, ReceivedOn: "2026-01-15",
	})
	if !IsPeriodLocked(err) {
		t.Errorf("funding into a locked period: got %v, want a period-locked error", err)
	}
}

// Funding is an operational record, not an issued document, so a mistyped one
// can be removed — but the ledger gets a reversal rather than a deletion.
func TestDeleteFundingReversesTheEntry(t *testing.T) {
	svc, _ := newTestService(t)

	f, err := svc.RecordFunding(Funding{
		Kind: FundingLoan, Amount: 400000, ReceivedOn: "2026-02-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteFunding(f.ID); err != nil {
		t.Fatalf("delete funding: %v", err)
	}

	owed, err := svc.OwnerLoanOutstanding("")
	if err != nil {
		t.Fatal(err)
	}
	if owed != 0 {
		t.Errorf("loan outstanding = %s after deleting the loan, want zero", owed)
	}
	// The original entry is still there; a reversal was posted beside it.
	if _, err := svc.GetEntry(f.JournalEntry); err != nil {
		t.Errorf("original entry was deleted rather than reversed: %v", err)
	}
	list, err := svc.ListFunding(FundingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("funding rows = %d, want 0", len(list))
	}
}

// Removing a loan that has been partly repaid would drive the liability
// negative — the mirror of the repayment guard.
func TestDeletingAPartlyRepaidLoanIsRefused(t *testing.T) {
	svc, _ := newTestService(t)

	loan, err := svc.RecordFunding(Funding{
		Kind: FundingLoan, Amount: 500000, ReceivedOn: "2026-02-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordFunding(Funding{
		Kind: FundingRepayment, Amount: 200000, ReceivedOn: "2026-03-01",
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteFunding(loan.ID); err == nil {
		t.Error("deleting a partly repaid loan was accepted")
	} else if !strings.Contains(err.Error(), "outstanding") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestListFundingFiltersByKind(t *testing.T) {
	svc, _ := newTestService(t)

	for _, f := range []Funding{
		{Kind: FundingCapital, Amount: 100000, ReceivedOn: "2026-01-02"},
		{Kind: FundingLoan, Amount: 200000, ReceivedOn: "2026-02-01"},
		{Kind: FundingLoan, Amount: 300000, ReceivedOn: "2026-03-01"},
	} {
		if _, err := svc.RecordFunding(f); err != nil {
			t.Fatal(err)
		}
	}

	loans, err := svc.ListFunding(FundingFilter{Kind: FundingLoan})
	if err != nil {
		t.Fatal(err)
	}
	if len(loans) != 2 {
		t.Fatalf("loans = %d, want 2", len(loans))
	}
	// Newest first, like every other listing in the module.
	if loans[0].ReceivedOn != "2026-03-01" {
		t.Errorf("first loan is %s, want the 2026-03-01 one", loans[0].ReceivedOn)
	}
	if loans[0].OwnerAccountName == "" || loans[0].CashAccountName == "" {
		t.Error("listing did not carry the account names for display")
	}

	all, err := svc.ListFunding(FundingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("all funding = %d, want 3", len(all))
	}
}

// Funding has to reach the reports, or the money is in the bank and nowhere on
// the balance sheet.
func TestFundingLandsOnTheBalanceSheet(t *testing.T) {
	svc, _ := newTestService(t)

	if _, err := svc.RecordFunding(Funding{
		Kind: FundingCapital, Amount: 2500000, ReceivedOn: "2026-01-02",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordFunding(Funding{
		Kind: FundingLoan, Amount: 500000, ReceivedOn: "2026-01-03",
	}); err != nil {
		t.Fatal(err)
	}

	bs, err := svc.BalanceSheet("2026-12-31")
	if err != nil {
		t.Fatal(err)
	}
	if !bs.Balanced {
		t.Errorf("balance sheet does not balance: assets %s, liabilities %s, equity %s",
			bs.TotalAssets, bs.TotalLiabilities, bs.TotalEquity)
	}
	if bs.TotalAssets != 3000000 {
		t.Errorf("assets = %s, want 30,000.00", bs.TotalAssets)
	}
	if bs.TotalLiabilities != 500000 {
		t.Errorf("liabilities = %s, want 5,000.00 (the loan)", bs.TotalLiabilities)
	}
	if bs.TotalEquity != 2500000 {
		t.Errorf("equity = %s, want 25,000.00 (the capital)", bs.TotalEquity)
	}
}
