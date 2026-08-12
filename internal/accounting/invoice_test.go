package accounting

import (
	"strings"
	"testing"
)

// newClient inserts a CRM client directly. The accounting module treats the CRM
// as the customer list rather than keeping its own, so a test invoice needs a
// real row to point at.
func newClient(t *testing.T, repo *SQLiteRepository, name string) int64 {
	t.Helper()
	res, err := repo.db.Exec(
		`INSERT INTO crm_clients (name, status, hourly_rate, created_at) VALUES (?,?,?,?)`,
		name, "Active", 90.0, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func draftFor(clientID int64, lines ...InvoiceLine) Invoice {
	return Invoice{
		ClientID:  clientID,
		IssueDate: "2026-08-12",
		Lines:     lines,
	}
}

func line(desc string, hours Milli, rate Money) InvoiceLine {
	return InvoiceLine{Description: desc, Quantity: hours, UnitPrice: rate}
}

func TestCreateDraftComputesTotals(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	inv, err := svc.CreateDraft(draftFor(client,
		line("Discovery workshop", 7500, 12000), // 7.5h at 120.00 = 900.00
		line("Report", 2000, 12000),             // 2h at 120.00 = 240.00
	))
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	if inv.Status != StatusDraft {
		t.Errorf("status = %s, want draft", inv.Status)
	}
	// A draft has no number: it has had no accounting consequence yet.
	if inv.Number != "" {
		t.Errorf("draft was given number %q", inv.Number)
	}
	if inv.Subtotal != 114000 {
		t.Errorf("subtotal = %s, want 1140.00", inv.Subtotal)
	}
	// 21% seeded default.
	if inv.VAT != 23940 {
		t.Errorf("VAT = %s, want 239.40", inv.VAT)
	}
	if inv.Total != 137940 {
		t.Errorf("total = %s, want 1379.40", inv.Total)
	}
	// Due date follows the seeded 14-day terms.
	if inv.DueDate != "2026-08-26" {
		t.Errorf("due date = %s, want 2026-08-26", inv.DueDate)
	}
}

// Totals are recomputed from lines, never trusted from input.
func TestCreateDraftIgnoresSuppliedTotals(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	in := draftFor(client, line("Work", 1000, 10000))
	in.Subtotal, in.VAT, in.Total = 999999, 999999, 999999

	inv, err := svc.CreateDraft(in)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Subtotal != 10000 || inv.Total != 12100 {
		t.Errorf("supplied totals were trusted: subtotal %s, total %s", inv.Subtotal, inv.Total)
	}
}

func TestIssueAllocatesNumbersWithoutGaps(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	var numbers []string
	for i := 0; i < 3; i++ {
		d, err := svc.CreateDraft(draftFor(client, line("Work", 1000, 10000)))
		if err != nil {
			t.Fatal(err)
		}
		issued, err := svc.Issue(d.ID)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		numbers = append(numbers, issued.Number)
	}

	want := []string{"INV-0001", "INV-0002", "INV-0003"}
	for i := range want {
		if numbers[i] != want[i] {
			t.Errorf("number %d = %q, want %q", i, numbers[i], want[i])
		}
	}
}

// A draft that fails to issue must not consume a number, or the sequence gains
// a gap that no later document can fill.
func TestFailedIssueDoesNotConsumeANumber(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	good, err := svc.CreateDraft(draftFor(client, line("Work", 1000, 10000)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Issue(good.ID); err != nil {
		t.Fatal(err)
	}

	// Issuing something that is not a draft fails after the sequence would have
	// been read had the check not come first.
	if _, err := svc.Issue(good.ID); err == nil {
		t.Fatal("re-issuing an issued invoice was allowed")
	}

	next, err := svc.CreateDraft(draftFor(client, line("More", 1000, 10000)))
	if err != nil {
		t.Fatal(err)
	}
	issued, err := svc.Issue(next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Number != "INV-0002" {
		t.Errorf("number = %q, want INV-0002 — the sequence gained a gap", issued.Number)
	}
}

func TestIssuePostsABalancedEntry(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	d, err := svc.CreateDraft(draftFor(client, line("Work", 1000, 100000))) // 1000.00
	if err != nil {
		t.Fatal(err)
	}
	inv, err := svc.Issue(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inv.JournalEntry == 0 {
		t.Fatal("issued invoice has no journal entry")
	}

	entry, err := svc.GetEntry(inv.JournalEntry)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Balanced() {
		t.Error("invoice entry does not balance")
	}
	if entry.SourceType != SourceInvoice || entry.SourceID != inv.ID {
		t.Errorf("entry not traceable to the invoice: %s/%d", entry.SourceType, entry.SourceID)
	}

	ar := mustAccount(t, svc, SysAR)
	sales := mustAccount(t, svc, SysSales)
	vat := mustAccount(t, svc, SysVATOutput)

	got := map[int64]Money{}
	for _, l := range entry.Lines {
		got[l.AccountID] += l.Debit - l.Credit
	}
	// Dr AR 1210.00, Cr Sales 1000.00, Cr VAT 210.00
	if got[ar.ID] != 121000 {
		t.Errorf("AR = %s, want 1210.00", got[ar.ID])
	}
	if got[sales.ID] != -100000 {
		t.Errorf("sales = %s, want credit 1000.00", got[sales.ID])
	}
	if got[vat.ID] != -21000 {
		t.Errorf("VAT = %s, want credit 210.00", got[vat.ID])
	}
}

// Lines sharing an income account are grouped, so a twelve-line invoice does
// not produce twelve identical credits in the ledger view.
func TestIssueGroupsLinesByIncomeAccount(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	d, err := svc.CreateDraft(draftFor(client,
		line("Day one", 1000, 10000),
		line("Day two", 1000, 10000),
		line("Day three", 1000, 10000),
	))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := svc.Issue(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := svc.GetEntry(inv.JournalEntry)
	if err != nil {
		t.Fatal(err)
	}
	// AR + one grouped income credit + VAT.
	if len(entry.Lines) != 3 {
		t.Errorf("want 3 ledger lines, got %d", len(entry.Lines))
	}
}

func TestIssuedInvoiceIsImmutable(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	d, err := svc.CreateDraft(draftFor(client, line("Work", 1000, 10000)))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := svc.Issue(d.ID)
	if err != nil {
		t.Fatal(err)
	}

	changed := inv
	changed.Lines = []InvoiceLine{line("Rewritten", 1000, 500)}
	if _, err := svc.UpdateDraft(inv.ID, changed); err == nil {
		t.Error("an issued invoice was edited")
	} else if !strings.Contains(err.Error(), "credit note") {
		t.Errorf("error should point at the credit-note route, got: %v", err)
	}

	if err := svc.DeleteDraft(inv.ID); err == nil {
		t.Error("an issued invoice was deleted")
	}
}

func TestVoidReversesTheLedgerEntry(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	d, _ := svc.CreateDraft(draftFor(client, line("Work", 1000, 10000)))
	inv, err := svc.Issue(d.ID)
	if err != nil {
		t.Fatal(err)
	}

	voided, err := svc.Void(inv.ID, "raised against the wrong client")
	if err != nil {
		t.Fatalf("void: %v", err)
	}
	if voided.Status != StatusVoid {
		t.Errorf("status = %s, want void", voided.Status)
	}
	// The number stays: a void invoice is a numbered zero-value record, not a
	// hole in the sequence.
	if voided.Number != "INV-0001" {
		t.Errorf("void invoice lost its number: %q", voided.Number)
	}

	entries, err := svc.ListEntries(EntryFilter{SourceID: inv.ID})
	if err != nil {
		t.Fatal(err)
	}
	net := map[int64]Money{}
	for _, e := range entries {
		for _, l := range e.Lines {
			net[l.AccountID] += l.Debit - l.Credit
		}
	}
	for acct, n := range net {
		if n != 0 {
			t.Errorf("account %d still holds %s after the void", acct, n)
		}
	}
}

func TestCreditNoteMirrorsTheInvoice(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	d, _ := svc.CreateDraft(draftFor(client, line("Work", 1000, 100000)))
	inv, err := svc.Issue(d.ID)
	if err != nil {
		t.Fatal(err)
	}

	note, err := svc.CreditNote(inv.ID, nil, "overbilled")
	if err != nil {
		t.Fatalf("credit note: %v", err)
	}
	if note.Kind != KindCreditNote || note.CorrectsID != inv.ID {
		t.Fatalf("credit note not linked: kind %s, corrects %d", note.Kind, note.CorrectsID)
	}

	issued, err := svc.Issue(note.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Its own series, not the invoice one.
	if issued.Number != "CN-0001" {
		t.Errorf("credit note number = %q, want CN-0001", issued.Number)
	}

	invEntry, _ := svc.GetEntry(inv.JournalEntry)
	noteEntry, _ := svc.GetEntry(issued.JournalEntry)
	net := map[int64]Money{}
	for _, e := range []JournalEntry{invEntry, noteEntry} {
		for _, l := range e.Lines {
			net[l.AccountID] += l.Debit - l.Credit
		}
	}
	for acct, n := range net {
		if n != 0 {
			t.Errorf("account %d nets %s; the pair should cancel", acct, n)
		}
	}
}

func TestReverseChargeChargesNoVAT(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Bruxelles SA")

	in := draftFor(client, line("Advisory", 1000, 100000))
	in.ReverseCharge = true

	// Without the customer's VAT number the document would be unusable.
	if _, err := svc.CreateDraft(in); err == nil {
		t.Fatal("reverse charge was accepted without a customer VAT number")
	}

	in.CustomerVAT = "BE0123456789"
	inv, err := svc.CreateDraft(in)
	if err != nil {
		t.Fatal(err)
	}
	if inv.VAT != 0 {
		t.Errorf("reverse-charge invoice charged %s of VAT", inv.VAT)
	}
	if inv.Total != inv.Subtotal {
		t.Errorf("total %s should equal subtotal %s", inv.Total, inv.Subtotal)
	}

	issued, err := svc.Issue(inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := svc.GetEntry(issued.JournalEntry)
	vat := mustAccount(t, svc, SysVATOutput)
	for _, l := range entry.Lines {
		if l.AccountID == vat.ID {
			t.Error("reverse-charge invoice touched the output VAT account")
		}
	}
}

func TestInvoiceLineMustUseAnIncomeAccount(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")
	rent := accountByCode(t, repo, "6100") // an expense account

	l := line("Work", 1000, 10000)
	l.IncomeAccountID = rent
	_, err := svc.CreateDraft(draftFor(client, l))
	if err == nil {
		t.Fatal("an expense account was accepted on an invoice line")
	}
	if !strings.Contains(err.Error(), "not income") {
		t.Errorf("unexpected error: %v", err)
	}
}

func accountByCode(t *testing.T, repo *SQLiteRepository, code string) int64 {
	t.Helper()
	var id int64
	if err := repo.db.QueryRow(`SELECT id FROM acct_accounts WHERE code = ?`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestIssueRefusedInLockedPeriod(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")

	d, err := svc.CreateDraft(draftFor(client, line("Work", 1000, 10000)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LockPeriod(Period{
		StartsOn: "2026-08-01", EndsOn: "2026-08-31", Locked: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Issue(d.ID); !IsPeriodLocked(err) {
		t.Fatalf("want a locked-period refusal, got: %v", err)
	}
}
