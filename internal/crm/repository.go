package crm

import (
	"database/sql"
	"fmt"
	"strings"
)

// Repository is the CRM persistence boundary.
type Repository interface {
	ListClients(search, status string) ([]Client, error)
	GetClient(id int64) (Client, error)
	CreateClient(c Client) (Client, error)
	UpdateClient(id int64, c Client) (Client, error)
	DeleteClient(id int64) error

	ListEngagements(clientID int64, search, status string) ([]Engagement, error)
	GetEngagement(id int64) (Engagement, error)
	CreateEngagement(e Engagement) (Engagement, error)
	UpdateEngagement(id int64, e Engagement) (Engagement, error)
	DeleteEngagement(id int64) error

	ListTimeEntries(filter TimeEntryFilter) ([]TimeEntry, error)
	GetTimeEntry(id int64) (TimeEntry, error)
	CreateTimeEntry(t TimeEntry) (TimeEntry, error)
	UpdateTimeEntry(id int64, t TimeEntry) (TimeEntry, error)
	DeleteTimeEntry(id int64) error
	SetTimeEntryInvoiced(id int64, invoiced bool) (TimeEntry, error)
	MarkEngagementInvoiced(engagementID int64) (int64, error)

	Dashboard() (Dashboard, error)
	Billing() ([]BillingLine, error)
}

// TimeEntryFilter narrows a time-entry listing.
type TimeEntryFilter struct {
	EngagementID int64
	ClientID     int64
	Billable     string // "", "1", "0"
	Invoiced     string // "", "1", "0"
	Search       string
}

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

// insert runs an INSERT and returns the generated id. The app this came from
// had a dialect-aware helper for this because it also spoke Postgres; here the
// backend is only ever SQLite, so LastInsertId is the whole story.
func (r *SQLiteRepository) insert(query string, args ...any) (int64, error) {
	res, err := r.db.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("insert: %w", err)
	}
	return res.LastInsertId()
}

// amountExpr is the billable value of a time-entry row.
const amountExpr = `CASE WHEN te.billable = 1 THEN te.hours * te.rate ELSE 0 END`

// ---- Clients ----

func (r *SQLiteRepository) ListClients(search, status string) ([]Client, error) {
	query := `SELECT
		c.id, c.name, c.contact_name, c.email, c.phone, c.status, c.hourly_rate, c.notes, c.created_at,
		(SELECT COUNT(*) FROM crm_engagements e WHERE e.client_id = c.id) AS engagement_count,
		COALESCE((SELECT SUM(te.hours) FROM crm_time_entries te
			JOIN crm_engagements e ON e.id = te.engagement_id WHERE e.client_id = c.id), 0) AS logged_hours,
		COALESCE((SELECT SUM(` + amountExpr + `) FROM crm_time_entries te
			JOIN crm_engagements e ON e.id = te.engagement_id
			WHERE e.client_id = c.id AND te.billable = 1 AND te.invoiced = 0), 0) AS unbilled_amount
	FROM crm_clients c
	WHERE 1=1`

	args := make([]any, 0, 3)
	if search != "" {
		query += ` AND (UPPER(c.name) LIKE ? OR UPPER(c.contact_name) LIKE ? OR UPPER(c.email) LIKE ?)`
		like := "%" + strings.ToUpper(search) + "%"
		args = append(args, like, like, like)
	}
	if status != "" {
		query += ` AND UPPER(c.status) = ?`
		args = append(args, strings.ToUpper(status))
	}
	query += ` ORDER BY LOWER(c.name) ASC`

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Client, 0)
	for rows.Next() {
		item, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) GetClient(id int64) (Client, error) {
	row := r.db.QueryRow(`SELECT
		c.id, c.name, c.contact_name, c.email, c.phone, c.status, c.hourly_rate, c.notes, c.created_at,
		(SELECT COUNT(*) FROM crm_engagements e WHERE e.client_id = c.id) AS engagement_count,
		COALESCE((SELECT SUM(te.hours) FROM crm_time_entries te
			JOIN crm_engagements e ON e.id = te.engagement_id WHERE e.client_id = c.id), 0) AS logged_hours,
		COALESCE((SELECT SUM(`+amountExpr+`) FROM crm_time_entries te
			JOIN crm_engagements e ON e.id = te.engagement_id
			WHERE e.client_id = c.id AND te.billable = 1 AND te.invoiced = 0), 0) AS unbilled_amount
	FROM crm_clients c WHERE c.id = ?`, id)
	return scanClient(row)
}

func (r *SQLiteRepository) CreateClient(c Client) (Client, error) {
	id, err := r.insert(`INSERT INTO crm_clients
		(name, contact_name, email, phone, status, hourly_rate, notes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.ContactName, c.Email, c.Phone, c.Status, c.HourlyRate, c.Notes, c.CreatedAt)
	if err != nil {
		return Client{}, err
	}
	return r.GetClient(id)
}

func (r *SQLiteRepository) UpdateClient(id int64, c Client) (Client, error) {
	res, err := r.db.Exec(`UPDATE crm_clients SET
		name = ?, contact_name = ?, email = ?, phone = ?, status = ?, hourly_rate = ?, notes = ?
		WHERE id = ?`,
		c.Name, c.ContactName, c.Email, c.Phone, c.Status, c.HourlyRate, c.Notes, id)
	if err != nil {
		return Client{}, err
	}
	if err := requireAffected(res); err != nil {
		return Client{}, err
	}
	return r.GetClient(id)
}

func (r *SQLiteRepository) DeleteClient(id int64) error {
	res, err := r.db.Exec(`DELETE FROM crm_clients WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

// ---- Engagements ----

func (r *SQLiteRepository) ListEngagements(clientID int64, search, status string) ([]Engagement, error) {
	query := `SELECT
		e.id, e.client_id, e.name, e.status, e.hourly_rate, e.budget_hours,
		e.start_date, e.end_date, e.notes, e.created_at,
		c.name AS client_name,
		COALESCE((SELECT SUM(te.hours) FROM crm_time_entries te WHERE te.engagement_id = e.id), 0) AS logged_hours
	FROM crm_engagements e
	JOIN crm_clients c ON c.id = e.client_id
	WHERE 1=1`

	args := make([]any, 0, 3)
	if clientID > 0 {
		query += ` AND e.client_id = ?`
		args = append(args, clientID)
	}
	if search != "" {
		query += ` AND (UPPER(e.name) LIKE ? OR UPPER(c.name) LIKE ?)`
		like := "%" + strings.ToUpper(search) + "%"
		args = append(args, like, like)
	}
	if status != "" {
		query += ` AND UPPER(e.status) = ?`
		args = append(args, strings.ToUpper(status))
	}
	query += ` ORDER BY LOWER(c.name) ASC, LOWER(e.name) ASC`

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Engagement, 0)
	for rows.Next() {
		item, err := scanEngagement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) GetEngagement(id int64) (Engagement, error) {
	row := r.db.QueryRow(`SELECT
		e.id, e.client_id, e.name, e.status, e.hourly_rate, e.budget_hours,
		e.start_date, e.end_date, e.notes, e.created_at,
		c.name AS client_name,
		COALESCE((SELECT SUM(te.hours) FROM crm_time_entries te WHERE te.engagement_id = e.id), 0) AS logged_hours
	FROM crm_engagements e
	JOIN crm_clients c ON c.id = e.client_id
	WHERE e.id = ?`, id)
	return scanEngagement(row)
}

func (r *SQLiteRepository) CreateEngagement(e Engagement) (Engagement, error) {
	id, err := r.insert(`INSERT INTO crm_engagements
		(client_id, name, status, hourly_rate, budget_hours, start_date, end_date, notes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ClientID, e.Name, e.Status, e.HourlyRate, e.BudgetHours, e.StartDate, e.EndDate, e.Notes, e.CreatedAt)
	if err != nil {
		return Engagement{}, err
	}
	return r.GetEngagement(id)
}

func (r *SQLiteRepository) UpdateEngagement(id int64, e Engagement) (Engagement, error) {
	res, err := r.db.Exec(`UPDATE crm_engagements SET
		client_id = ?, name = ?, status = ?, hourly_rate = ?, budget_hours = ?,
		start_date = ?, end_date = ?, notes = ?
		WHERE id = ?`,
		e.ClientID, e.Name, e.Status, e.HourlyRate, e.BudgetHours, e.StartDate, e.EndDate, e.Notes, id)
	if err != nil {
		return Engagement{}, err
	}
	if err := requireAffected(res); err != nil {
		return Engagement{}, err
	}
	return r.GetEngagement(id)
}

func (r *SQLiteRepository) DeleteEngagement(id int64) error {
	res, err := r.db.Exec(`DELETE FROM crm_engagements WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

// ---- Time entries ----

const timeEntrySelect = `SELECT
	te.id, te.engagement_id, te.entry_date, te.hours, te.description, te.billable,
	te.rate, te.invoiced, te.created_at,
	e.name AS engagement_name, e.client_id, c.name AS client_name,
	` + amountExpr + ` AS amount
FROM crm_time_entries te
JOIN crm_engagements e ON e.id = te.engagement_id
JOIN crm_clients c ON c.id = e.client_id`

func (r *SQLiteRepository) ListTimeEntries(filter TimeEntryFilter) ([]TimeEntry, error) {
	query := timeEntrySelect + ` WHERE 1=1`
	args := make([]any, 0, 5)
	if filter.EngagementID > 0 {
		query += ` AND te.engagement_id = ?`
		args = append(args, filter.EngagementID)
	}
	if filter.ClientID > 0 {
		query += ` AND e.client_id = ?`
		args = append(args, filter.ClientID)
	}
	if filter.Billable == "1" || filter.Billable == "0" {
		query += ` AND te.billable = ?`
		args = append(args, filter.Billable)
	}
	if filter.Invoiced == "1" || filter.Invoiced == "0" {
		query += ` AND te.invoiced = ?`
		args = append(args, filter.Invoiced)
	}
	if filter.Search != "" {
		query += ` AND (UPPER(te.description) LIKE ? OR UPPER(e.name) LIKE ? OR UPPER(c.name) LIKE ?)`
		like := "%" + strings.ToUpper(filter.Search) + "%"
		args = append(args, like, like, like)
	}
	query += ` ORDER BY te.entry_date DESC, te.id DESC`

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TimeEntry, 0)
	for rows.Next() {
		item, err := scanTimeEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) GetTimeEntry(id int64) (TimeEntry, error) {
	row := r.db.QueryRow(timeEntrySelect+` WHERE te.id = ?`, id)
	return scanTimeEntry(row)
}

func (r *SQLiteRepository) CreateTimeEntry(t TimeEntry) (TimeEntry, error) {
	id, err := r.insert(`INSERT INTO crm_time_entries
		(engagement_id, entry_date, hours, description, billable, rate, invoiced, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.EngagementID, t.EntryDate, t.Hours, t.Description, boolToInt(t.Billable), t.Rate, boolToInt(t.Invoiced), t.CreatedAt)
	if err != nil {
		return TimeEntry{}, err
	}
	return r.GetTimeEntry(id)
}

func (r *SQLiteRepository) UpdateTimeEntry(id int64, t TimeEntry) (TimeEntry, error) {
	res, err := r.db.Exec(`UPDATE crm_time_entries SET
		engagement_id = ?, entry_date = ?, hours = ?, description = ?, billable = ?, rate = ?, invoiced = ?
		WHERE id = ?`,
		t.EngagementID, t.EntryDate, t.Hours, t.Description, boolToInt(t.Billable), t.Rate, boolToInt(t.Invoiced), id)
	if err != nil {
		return TimeEntry{}, err
	}
	if err := requireAffected(res); err != nil {
		return TimeEntry{}, err
	}
	return r.GetTimeEntry(id)
}

func (r *SQLiteRepository) DeleteTimeEntry(id int64) error {
	res, err := r.db.Exec(`DELETE FROM crm_time_entries WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

func (r *SQLiteRepository) SetTimeEntryInvoiced(id int64, invoiced bool) (TimeEntry, error) {
	res, err := r.db.Exec(`UPDATE crm_time_entries SET invoiced = ? WHERE id = ?`, boolToInt(invoiced), id)
	if err != nil {
		return TimeEntry{}, err
	}
	if err := requireAffected(res); err != nil {
		return TimeEntry{}, err
	}
	return r.GetTimeEntry(id)
}

// MarkEngagementInvoiced flags every billable, not-yet-invoiced entry on an
// engagement as invoiced and returns how many rows were updated.
func (r *SQLiteRepository) MarkEngagementInvoiced(engagementID int64) (int64, error) {
	res, err := r.db.Exec(`UPDATE crm_time_entries SET invoiced = 1
		WHERE engagement_id = ? AND billable = 1 AND invoiced = 0`, engagementID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- Aggregations ----

func (r *SQLiteRepository) Dashboard() (Dashboard, error) {
	var d Dashboard
	if err := r.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM crm_clients),
		(SELECT COUNT(*) FROM crm_clients WHERE UPPER(status) = 'ACTIVE'),
		(SELECT COUNT(*) FROM crm_engagements WHERE UPPER(status) = 'ACTIVE'),
		COALESCE((SELECT SUM(hours) FROM crm_time_entries WHERE entry_date >= date('now','-30 days')), 0),
		COALESCE((SELECT SUM(hours) FROM crm_time_entries WHERE billable = 1), 0)
	`).Scan(&d.TotalClients, &d.ActiveClients, &d.ActiveEngagements, &d.HoursLast30, &d.BillableHoursAllTime); err != nil {
		return Dashboard{}, err
	}

	if err := r.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN te.billable = 1 AND te.invoiced = 0 THEN te.hours * te.rate ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN te.billable = 1 AND te.invoiced = 1 THEN te.hours * te.rate ELSE 0 END), 0)
	FROM crm_time_entries te`).Scan(&d.UnbilledAmount, &d.InvoicedAmount); err != nil {
		return Dashboard{}, err
	}

	topRows, err := r.db.Query(`SELECT c.id, c.name,
		COALESCE(SUM(te.hours), 0) AS hours,
		COALESCE(SUM(` + amountExpr + `), 0) AS amount
	FROM crm_clients c
	JOIN crm_engagements e ON e.client_id = c.id
	JOIN crm_time_entries te ON te.engagement_id = e.id
	GROUP BY c.id, c.name
	HAVING COALESCE(SUM(te.hours), 0) > 0
	ORDER BY hours DESC, LOWER(c.name) ASC
	LIMIT 5`)
	if err != nil {
		return Dashboard{}, err
	}
	defer topRows.Close()
	for topRows.Next() {
		var ch ClientHours
		if err := topRows.Scan(&ch.ClientID, &ch.ClientName, &ch.Hours, &ch.Amount); err != nil {
			return Dashboard{}, err
		}
		d.TopClients = append(d.TopClients, ch)
	}
	if err := topRows.Err(); err != nil {
		return Dashboard{}, err
	}

	recent, err := r.db.Query(timeEntrySelect + ` ORDER BY te.entry_date DESC, te.id DESC LIMIT 8`)
	if err != nil {
		return Dashboard{}, err
	}
	defer recent.Close()
	for recent.Next() {
		item, err := scanTimeEntry(recent)
		if err != nil {
			return Dashboard{}, err
		}
		d.RecentEntries = append(d.RecentEntries, item)
	}
	return d, recent.Err()
}

func (r *SQLiteRepository) Billing() ([]BillingLine, error) {
	rows, err := r.db.Query(`SELECT
		c.id, c.name, e.id, e.name,
		COALESCE(SUM(CASE WHEN te.billable = 1 THEN te.hours ELSE 0 END), 0) AS billable_hours,
		COALESCE(SUM(CASE WHEN te.billable = 1 AND te.invoiced = 0 THEN te.hours ELSE 0 END), 0) AS unbilled_hours,
		COALESCE(SUM(CASE WHEN te.billable = 1 AND te.invoiced = 0 THEN te.hours * te.rate ELSE 0 END), 0) AS unbilled_amount,
		COALESCE(SUM(CASE WHEN te.billable = 1 AND te.invoiced = 1 THEN te.hours * te.rate ELSE 0 END), 0) AS invoiced_amount
	FROM crm_engagements e
	JOIN crm_clients c ON c.id = e.client_id
	LEFT JOIN crm_time_entries te ON te.engagement_id = e.id
	GROUP BY c.id, c.name, e.id, e.name
	HAVING COALESCE(SUM(CASE WHEN te.billable = 1 THEN te.hours ELSE 0 END), 0) > 0
	ORDER BY unbilled_amount DESC, LOWER(c.name) ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]BillingLine, 0)
	for rows.Next() {
		var b BillingLine
		if err := rows.Scan(&b.ClientID, &b.ClientName, &b.EngagementID, &b.EngagementName,
			&b.BillableHours, &b.UnbilledHours, &b.UnbilledAmount, &b.InvoicedAmount); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---- scanners / helpers ----

type scanner interface{ Scan(dest ...any) error }

func scanClient(s scanner) (Client, error) {
	var c Client
	if err := s.Scan(&c.ID, &c.Name, &c.ContactName, &c.Email, &c.Phone, &c.Status,
		&c.HourlyRate, &c.Notes, &c.CreatedAt, &c.EngagementCount, &c.LoggedHours, &c.UnbilledAmount); err != nil {
		return Client{}, err
	}
	return c, nil
}

func scanEngagement(s scanner) (Engagement, error) {
	var e Engagement
	if err := s.Scan(&e.ID, &e.ClientID, &e.Name, &e.Status, &e.HourlyRate, &e.BudgetHours,
		&e.StartDate, &e.EndDate, &e.Notes, &e.CreatedAt, &e.ClientName, &e.LoggedHours); err != nil {
		return Engagement{}, err
	}
	return e, nil
}

func scanTimeEntry(s scanner) (TimeEntry, error) {
	var t TimeEntry
	var billableInt, invoicedInt int
	if err := s.Scan(&t.ID, &t.EngagementID, &t.EntryDate, &t.Hours, &t.Description, &billableInt,
		&t.Rate, &invoicedInt, &t.CreatedAt, &t.EngagementName, &t.ClientID, &t.ClientName, &t.Amount); err != nil {
		return TimeEntry{}, err
	}
	t.Billable = billableInt == 1
	t.Invoiced = invoicedInt == 1
	return t, nil
}

func requireAffected(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
