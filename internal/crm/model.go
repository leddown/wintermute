// Package crm is a lightweight client-relationship and practice-management
// module: clients, the engagements done for them, billable time entries, and a
// billing rollup.
//
// The data model follows the loop common to small-business consulting tooling
// (Dolibarr, Invoice Ninja, SolidInvoice, Solidtime): client -> engagement ->
// billable time entry -> billing, with time tracking feeding billing directly
// so hours do not leak unbilled.
//
// Moved here from the RCSA application. It carries no owner column: see
// migrations/0004_workspace.sql.
package crm

// Client is a customer of the consultancy.
type Client struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	ContactName string  `json:"contact_name"`
	Email       string  `json:"email"`
	Phone       string  `json:"phone"`
	Status      string  `json:"status"` // Prospect / Active / Inactive
	HourlyRate  float64 `json:"hourly_rate"`
	Notes       string  `json:"notes"`
	CreatedAt   string  `json:"created_at"`

	// Derived (read-only) fields populated by list/get queries.
	EngagementCount int     `json:"engagement_count"`
	LoggedHours     float64 `json:"logged_hours"`
	UnbilledAmount  float64 `json:"unbilled_amount"`
}

// Engagement is a project or retainer performed for a client.
type Engagement struct {
	ID          int64   `json:"id"`
	ClientID    int64   `json:"client_id"`
	Name        string  `json:"name"`
	Status      string  `json:"status"` // Proposed / Active / On Hold / Completed
	HourlyRate  float64 `json:"hourly_rate"`
	BudgetHours float64 `json:"budget_hours"`
	StartDate   string  `json:"start_date"`
	EndDate     string  `json:"end_date"`
	Notes       string  `json:"notes"`
	CreatedAt   string  `json:"created_at"`

	// Derived (read-only) fields.
	ClientName  string  `json:"client_name"`
	LoggedHours float64 `json:"logged_hours"`
}

// EffectiveRate returns the engagement rate, falling back to the client rate.
func (e Engagement) EffectiveRate(clientRate float64) float64 {
	if e.HourlyRate > 0 {
		return e.HourlyRate
	}
	return clientRate
}

// TimeEntry is a logged block of work against an engagement.
type TimeEntry struct {
	ID           int64   `json:"id"`
	EngagementID int64   `json:"engagement_id"`
	EntryDate    string  `json:"entry_date"`
	Hours        float64 `json:"hours"`
	Description  string  `json:"description"`
	Billable     bool    `json:"billable"`
	Rate         float64 `json:"rate"`
	Invoiced     bool    `json:"invoiced"`
	CreatedAt    string  `json:"created_at"`

	// Derived (read-only) fields.
	EngagementName string  `json:"engagement_name"`
	ClientID       int64   `json:"client_id"`
	ClientName     string  `json:"client_name"`
	Amount         float64 `json:"amount"` // hours*rate when billable, else 0
}

// Dashboard is the practice overview shown on the CRM landing page.
type Dashboard struct {
	TotalClients         int           `json:"total_clients"`
	ActiveClients        int           `json:"active_clients"`
	ActiveEngagements    int           `json:"active_engagements"`
	HoursLast30          float64       `json:"hours_last_30"`
	BillableHoursAllTime float64       `json:"billable_hours_all_time"`
	UnbilledAmount       float64       `json:"unbilled_amount"`
	InvoicedAmount       float64       `json:"invoiced_amount"`
	TopClients           []ClientHours `json:"top_clients"`
	RecentEntries        []TimeEntry   `json:"recent_entries"`
}

// ClientHours is a per-client time/value rollup for the dashboard.
type ClientHours struct {
	ClientID   int64   `json:"client_id"`
	ClientName string  `json:"client_name"`
	Hours      float64 `json:"hours"`
	Amount     float64 `json:"amount"`
}

// BillingLine is a per-engagement billing rollup.
type BillingLine struct {
	ClientID       int64   `json:"client_id"`
	ClientName     string  `json:"client_name"`
	EngagementID   int64   `json:"engagement_id"`
	EngagementName string  `json:"engagement_name"`
	BillableHours  float64 `json:"billable_hours"`
	UnbilledHours  float64 `json:"unbilled_hours"`
	UnbilledAmount float64 `json:"unbilled_amount"`
	InvoicedAmount float64 `json:"invoiced_amount"`
}
