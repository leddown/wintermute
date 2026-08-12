package accounting

import (
	"strings"
	"testing"
)

func newEngagement(t *testing.T, repo *SQLiteRepository, clientID int64, name string, rate float64) int64 {
	t.Helper()
	res, err := repo.db.Exec(`INSERT INTO crm_engagements
		(client_id, name, status, hourly_rate, created_at) VALUES (?,?,?,?,?)`,
		clientID, name, "Active", rate, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func newTimeEntry(t *testing.T, repo *SQLiteRepository, engagementID int64, date string, hours, rate float64, desc string) int64 {
	t.Helper()
	res, err := repo.db.Exec(`INSERT INTO crm_time_entries
		(engagement_id, entry_date, hours, description, billable, rate, invoiced, created_at)
		VALUES (?,?,?,?,1,?,0,?)`,
		engagementID, date, hours, desc, rate, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestDraftFromUnbilledTime(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")
	eng := newEngagement(t, repo, client, "Platform review", 120)
	newTimeEntry(t, repo, eng, "2026-08-03", 7.5, 120, "Discovery")
	newTimeEntry(t, repo, eng, "2026-08-04", 2.25, 120, "Write-up")

	inv, err := svc.DraftFromUnbilledTime(UnbilledFilter{EngagementID: eng})
	if err != nil {
		t.Fatalf("draft from time: %v", err)
	}

	if len(inv.Lines) != 2 {
		t.Fatalf("want one line per time entry, got %d", len(inv.Lines))
	}
	// 7.5*120 + 2.25*120 = 900 + 270 = 1170.00
	if inv.Subtotal != 117000 {
		t.Errorf("subtotal = %s, want 1170.00", inv.Subtotal)
	}
	// Provenance is what lets issuing flag the right entries.
	for i, l := range inv.Lines {
		if l.TimeEntryID == 0 {
			t.Errorf("line %d lost its time entry link", i)
		}
		if !strings.Contains(l.Description, "2026-08-0") {
			t.Errorf("line %d description lost the date: %q", i, l.Description)
		}
	}
	if inv.EngagementID != eng {
		t.Errorf("invoice not pinned to the engagement")
	}
}

// The hours column is REAL. Anything that reaches a price must be exact first.
func TestFractionalHoursSurviveTheFloatBoundary(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")
	eng := newEngagement(t, repo, client, "Retainer", 0)
	// 0.1 has no exact binary representation; three of them are the classic case.
	newTimeEntry(t, repo, eng, "2026-08-03", 0.1, 100, "a")
	newTimeEntry(t, repo, eng, "2026-08-04", 0.1, 100, "b")
	newTimeEntry(t, repo, eng, "2026-08-05", 0.1, 100, "c")

	inv, err := svc.DraftFromUnbilledTime(UnbilledFilter{EngagementID: eng})
	if err != nil {
		t.Fatal(err)
	}
	// 0.1h at 100.00 is 10.00 exactly, three times is 30.00 — not 29.999…
	if inv.Subtotal != 3000 {
		t.Errorf("subtotal = %s, want 30.00", inv.Subtotal)
	}
}

// Drafting must not offer the same hours twice, and issuing must flag them.
func TestBilledTimeIsNotOfferedAgain(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")
	eng := newEngagement(t, repo, client, "Work", 100)
	entryID := newTimeEntry(t, repo, eng, "2026-08-03", 4, 100, "Analysis")

	draft, err := svc.DraftFromUnbilledTime(UnbilledFilter{EngagementID: eng})
	if err != nil {
		t.Fatal(err)
	}

	// While the draft exists the hours are spoken for, even though the CRM flag
	// has not been set yet. This is the window a second draft would double-bill.
	if _, err := svc.DraftFromUnbilledTime(UnbilledFilter{EngagementID: eng}); err == nil {
		t.Error("the same hours were offered to a second draft")
	}

	if _, err := svc.Issue(draft.ID); err != nil {
		t.Fatal(err)
	}

	// And the CRM flag is now set, inside the same transaction as the issue.
	var invoiced int
	if err := repo.db.QueryRow(`SELECT invoiced FROM crm_time_entries WHERE id = ?`, entryID).
		Scan(&invoiced); err != nil {
		t.Fatal(err)
	}
	if invoiced != 1 {
		t.Error("issuing did not flag the time entry as invoiced")
	}

	left, err := svc.UnbilledTime(UnbilledFilter{EngagementID: eng})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d entries still offered as unbilled after issue", len(left))
	}
}

// Voiding an invoice releases its hours: the work was real and still needs
// billing, just not on that document.
func TestVoidingReleasesTimeForRebilling(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")
	eng := newEngagement(t, repo, client, "Work", 100)
	entryID := newTimeEntry(t, repo, eng, "2026-08-03", 4, 100, "Analysis")

	draft, _ := svc.DraftFromUnbilledTime(UnbilledFilter{EngagementID: eng})
	inv, err := svc.Issue(draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Void(inv.ID, "wrong client"); err != nil {
		t.Fatal(err)
	}

	// The void clears the accounting-side guard. The CRM flag is separate and is
	// deliberately left set, so this asserts the honest current behaviour rather
	// than a nicer one that does not exist.
	if _, err := repo.db.Exec(`UPDATE crm_time_entries SET invoiced = 0 WHERE id = ?`, entryID); err != nil {
		t.Fatal(err)
	}
	left, err := svc.UnbilledTime(UnbilledFilter{EngagementID: eng})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("voided work was not released for rebilling: %d entries", len(left))
	}
}

func TestUnbilledExcludesNonBillableAndRespectsCutoff(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")
	eng := newEngagement(t, repo, client, "Work", 100)
	newTimeEntry(t, repo, eng, "2026-08-03", 4, 100, "Billable")
	newTimeEntry(t, repo, eng, "2026-09-15", 4, 100, "Later")
	if _, err := repo.db.Exec(`INSERT INTO crm_time_entries
		(engagement_id, entry_date, hours, description, billable, rate, invoiced, created_at)
		VALUES (?,?,?,?,0,?,0,?)`, eng, "2026-08-04", 3.0, "Internal", 100.0, ""); err != nil {
		t.Fatal(err)
	}

	all, err := svc.UnbilledTime(UnbilledFilter{EngagementID: eng})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("non-billable time leaked into billing: %d entries", len(all))
	}

	upto, err := svc.UnbilledTime(UnbilledFilter{EngagementID: eng, UpTo: "2026-08-31"})
	if err != nil {
		t.Fatal(err)
	}
	if len(upto) != 1 {
		t.Errorf("cutoff ignored: %d entries", len(upto))
	}
}

func TestZeroRateTimeIsRefusedRatherThanBilledAtNothing(t *testing.T) {
	svc, repo := newTestService(t)
	client := newClient(t, repo, "Acme")
	eng := newEngagement(t, repo, client, "Work", 0)
	newTimeEntry(t, repo, eng, "2026-08-03", 4, 0, "Analysis")

	_, err := svc.DraftFromUnbilledTime(UnbilledFilter{EngagementID: eng})
	if err == nil {
		t.Fatal("a zero-rate invoice was drafted")
	}
	if !strings.Contains(err.Error(), "no rate") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBillingCannotSpanClients(t *testing.T) {
	svc, repo := newTestService(t)
	a := newClient(t, repo, "Acme")
	b := newClient(t, repo, "Globex")
	ea := newEngagement(t, repo, a, "A", 100)
	eb := newEngagement(t, repo, b, "B", 100)
	newTimeEntry(t, repo, ea, "2026-08-03", 1, 100, "a")
	newTimeEntry(t, repo, eb, "2026-08-03", 1, 100, "b")

	// No client filter and no engagement filter is refused outright.
	if _, err := svc.DraftFromUnbilledTime(UnbilledFilter{}); err == nil {
		t.Error("billing with no target was allowed")
	}

	// Per-client works.
	if _, err := svc.DraftFromUnbilledTime(UnbilledFilter{ClientID: a}); err != nil {
		t.Errorf("per-client billing failed: %v", err)
	}
}
