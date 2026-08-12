package accounting

import (
	"fmt"
	"strings"
)

// This file is the seam between the two workspace modules. The CRM knows who
// the work was for and how long it took; accounting turns that into a document
// and a ledger entry. Nothing else crosses the boundary.
//
// It reads the CRM tables directly rather than taking a crm.Service. The two
// modules share one SQLite file, the queries here are reads plus a single
// flag update that has to happen inside the invoice's own transaction, and
// routing that through another service would mean either a second transaction
// or handing the CRM a *sql.Tx — both worse than a join.

// BillableTime is an unbilled CRM time entry, converted into the units the
// ledger works in. Hours arrive as REAL from the CRM and are pinned to exact
// thousandths here, at the boundary, before anything multiplies them.
type BillableTime struct {
	TimeEntryID    int64  `json:"time_entry_id"`
	EntryDate      string `json:"entry_date"`
	Description    string `json:"description"`
	Hours          Milli  `json:"hours"`
	Rate           Money  `json:"rate"`
	Amount         Money  `json:"amount"`
	EngagementID   int64  `json:"engagement_id"`
	EngagementName string `json:"engagement_name"`
	ClientID       int64  `json:"client_id"`
	ClientName     string `json:"client_name"`
}

// UnbilledFilter narrows what to bill. A zero engagement means every engagement
// for the client; a zero client means everything.
type UnbilledFilter struct {
	ClientID     int64
	EngagementID int64
	UpTo         string // inclusive; empty means no upper bound
}

// UnbilledTime lists billable time that has not reached an invoice.
//
// Two conditions, not one. `invoiced` is the CRM's own flag, set when an
// invoice is issued. The NOT EXISTS covers the window between drafting an
// invoice and issuing it: those hours are spoken for, and offering them to a
// second draft is how the same work gets billed twice. Lines on a void invoice
// are excluded from that guard, because voiding is what releases them again.
func (r *SQLiteRepository) UnbilledTime(f UnbilledFilter) ([]BillableTime, error) {
	where := []string{"t.billable = 1", "t.invoiced = 0"}
	var args []any
	if f.EngagementID != 0 {
		where = append(where, "t.engagement_id = ?")
		args = append(args, f.EngagementID)
	}
	if f.ClientID != 0 {
		where = append(where, "e.client_id = ?")
		args = append(args, f.ClientID)
	}
	if f.UpTo != "" {
		where = append(where, "t.entry_date <= ?")
		args = append(args, f.UpTo)
	}

	q := `SELECT t.id, t.entry_date, t.description, t.hours, t.rate,
		t.engagement_id, e.name, e.client_id, c.name
		FROM crm_time_entries t
		JOIN crm_engagements e ON e.id = t.engagement_id
		JOIN crm_clients c ON c.id = e.client_id
		WHERE ` + strings.Join(where, " AND ") + `
		  AND NOT EXISTS (
		      SELECT 1 FROM acct_invoice_lines il
		      JOIN acct_invoices i ON i.id = il.invoice_id
		      WHERE il.time_entry_id = t.id AND i.status <> 'void')
		ORDER BY e.client_id, t.engagement_id, t.entry_date, t.id`

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("unbilled time: %w", err)
	}
	defer rows.Close()

	out := []BillableTime{}
	for rows.Next() {
		var b BillableTime
		var hours, rate float64
		if err := rows.Scan(&b.TimeEntryID, &b.EntryDate, &b.Description, &hours, &rate,
			&b.EngagementID, &b.EngagementName, &b.ClientID, &b.ClientName); err != nil {
			return nil, fmt.Errorf("unbilled time: %w", err)
		}
		b.Hours = MilliFromFloat(hours)
		b.Rate = FromFloat(rate)
		b.Amount = b.Hours.Extend(b.Rate)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---- service ----

// UnbilledTime lists work that is ready to bill.
func (s *Service) UnbilledTime(f UnbilledFilter) ([]BillableTime, error) {
	return s.repo.UnbilledTime(f)
}

// DraftFromUnbilledTime builds a draft invoice from a client's unbilled hours.
//
// One invoice line per time entry, carrying its id. That keeps the document
// readable against the timesheet — a client querying a line can be answered
// with the date and description it came from — and it is what lets issuing the
// invoice flag exactly the right entries. Rolling the hours into a single
// "Consulting services" line would lose both.
//
// The draft is only a draft: nothing is billed, nothing is posted, and no
// number is consumed until it is issued.
func (s *Service) DraftFromUnbilledTime(f UnbilledFilter) (Invoice, error) {
	if f.ClientID == 0 && f.EngagementID == 0 {
		return Invoice{}, invalid("billing needs a client or an engagement")
	}
	entries, err := s.repo.UnbilledTime(f)
	if err != nil {
		return Invoice{}, err
	}
	if len(entries) == 0 {
		return Invoice{}, invalid("there is no unbilled billable time matching that")
	}

	// One invoice, one client. Billing an engagement implies its client; asking
	// for a client's whole backlog can span engagements but never clients.
	clientID := entries[0].ClientID
	for _, e := range entries {
		if e.ClientID != clientID {
			return Invoice{}, invalid(
				"that selection spans more than one client (%s and %s); bill them separately",
				entries[0].ClientName, e.ClientName)
		}
	}

	// A rate of zero means nobody set one on the engagement or the client. That
	// would silently produce a zero-value invoice, which is worse than stopping.
	var zeroRate []string
	for _, e := range entries {
		if e.Rate == 0 {
			zeroRate = append(zeroRate, e.EntryDate)
		}
	}
	if len(zeroRate) > 0 {
		return Invoice{}, invalid(
			"%d of those time entries have no rate (%s); set a rate on the engagement "+
				"or the client, or bill them by hand",
			len(zeroRate), strings.Join(firstN(zeroRate, 3), ", "))
	}

	settings, err := s.repo.Settings()
	if err != nil {
		return Invoice{}, err
	}
	income, err := s.repo.AccountBySystemKey(SysSales)
	if err != nil {
		return Invoice{}, err
	}

	lines := make([]InvoiceLine, 0, len(entries))
	engagements := map[int64]bool{}
	for _, e := range entries {
		engagements[e.EngagementID] = true
		desc := e.Description
		if desc == "" {
			desc = e.EngagementName
		}
		lines = append(lines, InvoiceLine{
			Description:     fmt.Sprintf("%s — %s", e.EntryDate, desc),
			Quantity:        e.Hours,
			UnitPrice:       e.Rate,
			VATRateID:       settings.DefaultVATRateID,
			IncomeAccountID: income.ID,
			TimeEntryID:     e.TimeEntryID,
		})
	}

	// Only pin the invoice to an engagement when all the work belongs to one.
	var engagementID int64
	if len(engagements) == 1 {
		engagementID = entries[0].EngagementID
	}

	return s.CreateDraft(Invoice{
		ClientID:     clientID,
		EngagementID: engagementID,
		BillTo:       entries[0].ClientName,
		IssueDate:    today(),
		Lines:        lines,
	})
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], "…")
}
