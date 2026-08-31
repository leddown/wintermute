package accounting

import (
	"strings"
	"testing"

	"wintermute/internal/store/storetest"
)

// newTestService gives a service over a real migrated SQLite database. The
// seeded chart of accounts is part of what is under test — the module resolves
// accounts by system key, and a seed that stopped providing one would break it
// in a way an in-memory fake would hide.
func newTestService(t *testing.T) (*Service, *SQLiteRepository) {
	t.Helper()
	st := storetest.New(t)

	repo := NewSQLiteRepository(st.DB())
	return NewService(repo), repo
}

func mustAccount(t *testing.T, s *Service, key string) Account {
	t.Helper()
	a, err := s.SystemAccount(key)
	if err != nil {
		t.Fatalf("system account %q: %v", key, err)
	}
	return a
}

// Every account the code addresses by key must exist in the seed, or the module
// fails at runtime on a fresh install rather than here.
func TestSeededSystemAccountsResolve(t *testing.T) {
	svc, _ := newTestService(t)
	for _, key := range []string{
		SysBank, SysAR, SysAP, SysVATOutput, SysVATInput,
		SysSales, SysRecharged, SysRetained, SysRounding,
		SysCapital, SysOwnerLoan,
	} {
		a := mustAccount(t, svc, key)
		if a.ID == 0 || a.Code == "" {
			t.Errorf("system key %q resolved to an empty account", key)
		}
	}
}

func TestPostBalancedEntry(t *testing.T) {
	svc, _ := newTestService(t)
	bank := mustAccount(t, svc, SysBank)
	sales := mustAccount(t, svc, SysSales)

	e, err := svc.PostFrom("2026-08-12", "Consulting fee", SourceManual, 0, []Posting{
		Debit(bank.ID, 121_00, "cash in"),
		Credit(sales.ID, 121_00, "fee"),
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if e.ID == 0 {
		t.Fatal("entry was not assigned an id")
	}
	if len(e.Lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(e.Lines))
	}
	if !e.Balanced() {
		t.Error("stored entry does not balance")
	}
	if got := e.Total(); got != 121_00 {
		t.Errorf("total = %s, want 121.00", got)
	}
	// The join is what the ledger view relies on.
	if e.Lines[0].AccountCode == "" || e.Lines[0].AccountType == "" {
		t.Error("lines came back without their account display fields")
	}
}

// The invariant this module exists to protect.
func TestPostRejectsUnbalancedEntry(t *testing.T) {
	svc, _ := newTestService(t)
	bank := mustAccount(t, svc, SysBank)
	sales := mustAccount(t, svc, SysSales)

	_, err := svc.PostFrom("2026-08-12", "wrong", SourceManual, 0, []Posting{
		Debit(bank.ID, 100_00, ""),
		Credit(sales.ID, 99_00, ""),
	})
	if err == nil {
		t.Fatal("an unbalanced entry was accepted")
	}
	if !IsValidation(err) {
		t.Errorf("want a validation error, got %T", err)
	}
	// The message must name the difference; "invalid entry" sends the operator
	// looking through every line by hand.
	if !strings.Contains(err.Error(), "1.00") {
		t.Errorf("error should state the 1.00 difference, got: %v", err)
	}
}

func TestPostRejectsMalformedLines(t *testing.T) {
	svc, _ := newTestService(t)
	bank := mustAccount(t, svc, SysBank)
	sales := mustAccount(t, svc, SysSales)

	cases := []struct {
		name  string
		lines []JournalLine
		want  string
	}{
		{
			name:  "single line",
			lines: []JournalLine{{AccountID: bank.ID, Debit: 100}},
			want:  "at least two lines",
		},
		{
			name: "both sides on one line",
			lines: []JournalLine{
				{AccountID: bank.ID, Debit: 100, Credit: 100},
				{AccountID: sales.ID, Credit: 100},
			},
			want: "exactly one of debit and credit",
		},
		{
			name: "neither side",
			lines: []JournalLine{
				{AccountID: bank.ID},
				{AccountID: sales.ID, Credit: 100},
			},
			want: "exactly one of debit and credit",
		},
		{
			name: "negative amount",
			lines: []JournalLine{
				{AccountID: bank.ID, Debit: -100},
				{AccountID: sales.ID, Credit: -100},
			},
			want: "cannot be negative",
		},
		{
			name: "unknown account",
			lines: []JournalLine{
				{AccountID: 999999, Debit: 100},
				{AccountID: sales.ID, Credit: 100},
			},
			want: "no account with id",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Post(JournalEntry{EntryDate: "2026-08-12", Lines: c.lines})
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("want error containing %q, got: %v", c.want, err)
			}
		})
	}
}

func TestPostRejectsArchivedAccount(t *testing.T) {
	svc, _ := newTestService(t)
	sales := mustAccount(t, svc, SysSales)

	extra, err := svc.CreateAccount(Account{Code: "6999", Name: "Temporary", Type: AccountExpense})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ArchiveAccount(extra.ID, true); err != nil {
		t.Fatal(err)
	}

	_, err = svc.PostFrom("2026-08-12", "", SourceManual, 0, []Posting{
		Debit(extra.ID, 100, ""),
		Credit(sales.ID, 100, ""),
	})
	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("want an archived-account rejection, got: %v", err)
	}
}

func TestPostRejectsLockedPeriod(t *testing.T) {
	svc, _ := newTestService(t)
	bank := mustAccount(t, svc, SysBank)
	sales := mustAccount(t, svc, SysSales)

	if _, err := svc.LockPeriod(Period{
		StartsOn: "2026-01-01", EndsOn: "2026-03-31", Locked: true, Note: "Q1 filed",
	}); err != nil {
		t.Fatal(err)
	}

	// Backdating into a closed quarter is the case the lock exists for.
	_, err := svc.PostFrom("2026-02-15", "backdated", SourceManual, 0, []Posting{
		Debit(bank.ID, 100, ""),
		Credit(sales.ID, 100, ""),
	})
	if err == nil {
		t.Fatal("a posting into a locked period was accepted")
	}
	if !IsPeriodLocked(err) {
		t.Errorf("want PeriodLockedError, got %T: %v", err, err)
	}

	// Outside the lock still works.
	if _, err := svc.PostFrom("2026-04-01", "after", SourceManual, 0, []Posting{
		Debit(bank.ID, 100, ""),
		Credit(sales.ID, 100, ""),
	}); err != nil {
		t.Errorf("posting outside the locked period failed: %v", err)
	}
}

func TestReverseMirrorsTheOriginal(t *testing.T) {
	svc, _ := newTestService(t)
	bank := mustAccount(t, svc, SysBank)
	sales := mustAccount(t, svc, SysSales)

	orig, err := svc.PostFrom("2026-08-12", "fee", SourceManual, 0, []Posting{
		Debit(bank.ID, 250_00, ""),
		Credit(sales.ID, 250_00, ""),
	})
	if err != nil {
		t.Fatal(err)
	}

	rev, err := svc.Reverse(orig.ID, "2026-08-13", "")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if rev.ReversesID != orig.ID {
		t.Errorf("reversal does not point at the original: %d", rev.ReversesID)
	}
	if !rev.Balanced() {
		t.Error("reversal does not balance")
	}
	// Each side must have swapped.
	for i, l := range rev.Lines {
		o := orig.Lines[i]
		if l.Debit != o.Credit || l.Credit != o.Debit {
			t.Errorf("line %d not mirrored: orig %s/%s, rev %s/%s",
				i, o.Debit, o.Credit, l.Debit, l.Credit)
		}
	}
	// And the pair must net to nothing on every account touched.
	net := map[int64]Money{}
	for _, e := range []JournalEntry{orig, rev} {
		for _, l := range e.Lines {
			net[l.AccountID] += l.Debit - l.Credit
		}
	}
	for acct, n := range net {
		if n != 0 {
			t.Errorf("account %d left with %s after reversal", acct, n)
		}
	}
}

// A failure partway through writing lines must leave nothing behind. This goes
// through the repository deliberately, bypassing the service's validation, to
// exercise the transaction rather than the guard in front of it.
func TestPostEntryIsAtomic(t *testing.T) {
	svc, repo := newTestService(t)
	bank := mustAccount(t, svc, SysBank)

	before := countEntries(t, repo)

	_, err := repo.PostEntry(JournalEntry{
		EntryDate:  "2026-08-12",
		SourceType: SourceManual,
		Lines: []JournalLine{
			{AccountID: bank.ID, Debit: 100},
			{AccountID: 999999, Credit: 100}, // violates the foreign key
		},
	})
	if err == nil {
		t.Fatal("expected the foreign key to reject the second line")
	}

	if after := countEntries(t, repo); after != before {
		t.Errorf("entry header survived a failed line insert: %d entries before, %d after",
			before, after)
	}
}

func countEntries(t *testing.T, repo *SQLiteRepository) int {
	t.Helper()
	var n int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM acct_journal_entries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAccountTypeCannotChangeOncePosted(t *testing.T) {
	svc, _ := newTestService(t)
	bank := mustAccount(t, svc, SysBank)
	sales := mustAccount(t, svc, SysSales)

	if _, err := svc.PostFrom("2026-08-12", "", SourceManual, 0, []Posting{
		Debit(bank.ID, 100, ""),
		Credit(sales.ID, 100, ""),
	}); err != nil {
		t.Fatal(err)
	}

	sales.Type = AccountExpense
	_, err := svc.UpdateAccount(sales.ID, sales)
	if err == nil {
		t.Fatal("retyping a posted account was allowed")
	}
	if !strings.Contains(err.Error(), "has postings") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestVATRateKindsMustAgreeWithRate(t *testing.T) {
	svc, _ := newTestService(t)

	if _, err := svc.SaveVATRate(VATRate{
		Code: "BAD", Name: "Nonsense", RateBP: 2100, Kind: VATExempt,
	}); err == nil {
		t.Error("an exempt rate with a non-zero percentage was accepted")
	}

	if _, err := svc.SaveVATRate(VATRate{
		Code: "OK", Name: "Second reduced", RateBP: 600, Kind: VATReduced,
	}); err != nil {
		t.Errorf("a valid reduced rate was rejected: %v", err)
	}
}
