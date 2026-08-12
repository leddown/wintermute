package crm

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a referenced record does not exist.
var ErrNotFound = errors.New("crm: record not found")

// ValidationError marks a bad-input error so handlers can map it to HTTP 400.
type ValidationError struct{ Msg string }

func (e ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// IsValidation reports whether err is a ValidationError.
func IsValidation(err error) bool {
	var ve ValidationError
	return errors.As(err, &ve)
}

// nowStamp is overridable in tests for deterministic timestamps.
var nowStamp = func() string { return time.Now().UTC().Format(time.RFC3339) }

func today() string { return time.Now().UTC().Format("2006-01-02") }

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// ---- Clients ----

func (s *Service) ListClients(search, status string) ([]Client, error) {
	return s.repo.ListClients(strings.TrimSpace(search), strings.TrimSpace(status))
}

func (s *Service) GetClient(id int64) (Client, error) {
	c, err := s.repo.GetClient(id)
	return c, mapNotFound(err)
}

func (s *Service) CreateClient(c Client) (Client, error) {
	normalizeClient(&c)
	if err := validateClient(c); err != nil {
		return Client{}, err
	}
	c.CreatedAt = nowStamp()
	return s.repo.CreateClient(c)
}

func (s *Service) UpdateClient(id int64, c Client) (Client, error) {
	normalizeClient(&c)
	if err := validateClient(c); err != nil {
		return Client{}, err
	}
	updated, err := s.repo.UpdateClient(id, c)
	return updated, mapNotFound(err)
}

func (s *Service) DeleteClient(id int64) error {
	return mapNotFound(s.repo.DeleteClient(id))
}

// ---- Engagements ----

func (s *Service) ListEngagements(clientID int64, search, status string) ([]Engagement, error) {
	return s.repo.ListEngagements(clientID, strings.TrimSpace(search), strings.TrimSpace(status))
}

func (s *Service) GetEngagement(id int64) (Engagement, error) {
	e, err := s.repo.GetEngagement(id)
	return e, mapNotFound(err)
}

func (s *Service) CreateEngagement(e Engagement) (Engagement, error) {
	normalizeEngagement(&e)
	if err := validateEngagement(e); err != nil {
		return Engagement{}, err
	}
	if _, err := s.repo.GetClient(e.ClientID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Engagement{}, invalid("client_id %d does not exist", e.ClientID)
		}
		return Engagement{}, err
	}
	e.CreatedAt = nowStamp()
	return s.repo.CreateEngagement(e)
}

func (s *Service) UpdateEngagement(id int64, e Engagement) (Engagement, error) {
	normalizeEngagement(&e)
	if err := validateEngagement(e); err != nil {
		return Engagement{}, err
	}
	updated, err := s.repo.UpdateEngagement(id, e)
	return updated, mapNotFound(err)
}

func (s *Service) DeleteEngagement(id int64) error {
	return mapNotFound(s.repo.DeleteEngagement(id))
}

// ---- Time entries ----

func (s *Service) ListTimeEntries(filter TimeEntryFilter) ([]TimeEntry, error) {
	filter.Search = strings.TrimSpace(filter.Search)
	return s.repo.ListTimeEntries(filter)
}

func (s *Service) GetTimeEntry(id int64) (TimeEntry, error) {
	t, err := s.repo.GetTimeEntry(id)
	return t, mapNotFound(err)
}

func (s *Service) CreateTimeEntry(t TimeEntry) (TimeEntry, error) {
	normalizeTimeEntry(&t)
	if err := validateTimeEntry(t); err != nil {
		return TimeEntry{}, err
	}
	eng, err := s.repo.GetEngagement(t.EngagementID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TimeEntry{}, invalid("engagement_id %d does not exist", t.EngagementID)
		}
		return TimeEntry{}, err
	}
	// Snapshot the billing rate at log time so later rate changes don't
	// retroactively alter already-logged (and possibly invoiced) work.
	if t.Rate <= 0 {
		t.Rate = s.resolveRate(eng)
	}
	t.CreatedAt = nowStamp()
	return s.repo.CreateTimeEntry(t)
}

func (s *Service) UpdateTimeEntry(id int64, t TimeEntry) (TimeEntry, error) {
	normalizeTimeEntry(&t)
	if err := validateTimeEntry(t); err != nil {
		return TimeEntry{}, err
	}
	if t.Rate <= 0 {
		eng, err := s.repo.GetEngagement(t.EngagementID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return TimeEntry{}, invalid("engagement_id %d does not exist", t.EngagementID)
			}
			return TimeEntry{}, err
		}
		t.Rate = s.resolveRate(eng)
	}
	updated, err := s.repo.UpdateTimeEntry(id, t)
	return updated, mapNotFound(err)
}

func (s *Service) DeleteTimeEntry(id int64) error {
	return mapNotFound(s.repo.DeleteTimeEntry(id))
}

func (s *Service) SetTimeEntryInvoiced(id int64, invoiced bool) (TimeEntry, error) {
	updated, err := s.repo.SetTimeEntryInvoiced(id, invoiced)
	return updated, mapNotFound(err)
}

// MarkEngagementInvoiced marks all billable, unbilled time on an engagement as
// invoiced and returns the number of entries updated.
func (s *Service) MarkEngagementInvoiced(engagementID int64) (int64, error) {
	return s.repo.MarkEngagementInvoiced(engagementID)
}

// resolveRate returns the engagement rate, falling back to the client rate.
func (s *Service) resolveRate(eng Engagement) float64 {
	if eng.HourlyRate > 0 {
		return eng.HourlyRate
	}
	if client, err := s.repo.GetClient(eng.ClientID); err == nil {
		return client.HourlyRate
	}
	return 0
}

// ---- Aggregations ----

func (s *Service) Dashboard() (Dashboard, error)   { return s.repo.Dashboard() }
func (s *Service) Billing() ([]BillingLine, error) { return s.repo.Billing() }

// ---- normalization & validation ----

func normalizeClient(c *Client) {
	c.Name = strings.TrimSpace(c.Name)
	c.ContactName = strings.TrimSpace(c.ContactName)
	c.Email = strings.TrimSpace(c.Email)
	c.Phone = strings.TrimSpace(c.Phone)
	c.Status = normalizeStatus(c.Status, clientStatuses, "Active")
	c.Notes = strings.TrimSpace(c.Notes)
	if c.HourlyRate < 0 {
		c.HourlyRate = 0
	}
}

func validateClient(c Client) error {
	if c.Name == "" {
		return invalid("name is required")
	}
	return nil
}

func normalizeEngagement(e *Engagement) {
	e.Name = strings.TrimSpace(e.Name)
	e.Status = normalizeStatus(e.Status, engagementStatuses, "Active")
	e.StartDate = strings.TrimSpace(e.StartDate)
	e.EndDate = strings.TrimSpace(e.EndDate)
	e.Notes = strings.TrimSpace(e.Notes)
	if e.HourlyRate < 0 {
		e.HourlyRate = 0
	}
	if e.BudgetHours < 0 {
		e.BudgetHours = 0
	}
}

func validateEngagement(e Engagement) error {
	if e.ClientID <= 0 {
		return invalid("client_id is required")
	}
	if e.Name == "" {
		return invalid("name is required")
	}
	return nil
}

func normalizeTimeEntry(t *TimeEntry) {
	t.Description = strings.TrimSpace(t.Description)
	t.EntryDate = strings.TrimSpace(t.EntryDate)
	if t.EntryDate == "" {
		t.EntryDate = today()
	}
	if t.Rate < 0 {
		t.Rate = 0
	}
}

func validateTimeEntry(t TimeEntry) error {
	if t.EngagementID <= 0 {
		return invalid("engagement_id is required")
	}
	if t.Hours <= 0 {
		return invalid("hours must be greater than 0")
	}
	if t.Hours > 24 {
		return invalid("hours must be 24 or less for a single entry")
	}
	return nil
}

var (
	clientStatuses     = []string{"Prospect", "Active", "Inactive"}
	engagementStatuses = []string{"Proposed", "Active", "On Hold", "Completed"}
)

func normalizeStatus(value string, allowed []string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	for _, a := range allowed {
		if strings.EqualFold(a, value) {
			return a
		}
	}
	return value
}

func mapNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
