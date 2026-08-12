package todo

import (
	"fmt"
	"time"
)

// Service holds the task rules.
type Service struct {
	repo Repository
	now  func() time.Time
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo, now: time.Now}
}

func (s *Service) timestamp() string { return s.now().UTC().Format(time.RFC3339) }
func (s *Service) today() string     { return s.now().UTC().Format(DateLayout) }

// Today exposes the service's notion of the current day so the pages and the
// calendar agree with the overdue calculation.
func (s *Service) Today() string { return s.today() }

// ---- Lists ----

func (s *Service) ListLists(includeArchived bool) ([]List, error) {
	return s.repo.ListLists(includeArchived)
}

func (s *Service) GetList(id int64) (List, error) {
	return s.repo.GetList(id)
}

func (s *Service) CreateList(l List) (List, error) {
	if err := l.Validate(); err != nil {
		return List{}, err
	}
	l.CreatedAt = s.timestamp()
	l.UpdatedAt = l.CreatedAt
	return s.repo.CreateList(l)
}

func (s *Service) UpdateList(id int64, l List) (List, error) {
	if err := l.Validate(); err != nil {
		return List{}, err
	}
	l.UpdatedAt = s.timestamp()
	return s.repo.UpdateList(id, l)
}

func (s *Service) DeleteList(id int64) error {
	return s.repo.DeleteList(id)
}

// ---- Tasks ----

func (s *Service) ListTasks(f Filter) ([]Task, error) {
	return s.repo.ListTasks(f)
}

func (s *Service) GetTask(id int64) (Task, error) {
	return s.repo.GetTask(id)
}

func (s *Service) CreateTask(t Task) (Task, error) {
	if err := t.Validate(); err != nil {
		return Task{}, err
	}
	if t.ListID <= 0 {
		return Task{}, fmt.Errorf("a task needs a list")
	}
	ordinal, err := s.repo.NextOrdinal(t.ListID)
	if err != nil {
		return Task{}, err
	}
	t.Ordinal = ordinal
	t.CreatedAt = s.timestamp()
	t.UpdatedAt = t.CreatedAt
	if t.Status == StatusDone {
		t.CompletedAt = t.CreatedAt
	}
	return s.repo.CreateTask(t)
}

// UpdateTask applies an edit, maintaining completed_at as a side effect of the
// status rather than as a field the caller sets. A completion timestamp a
// client can supply is a completion timestamp that lies.
func (s *Service) UpdateTask(id int64, t Task) (Task, error) {
	existing, err := s.repo.GetTask(id)
	if err != nil {
		return Task{}, err
	}
	if err := t.Validate(); err != nil {
		return Task{}, err
	}
	// The list a task belongs to is not editable here: moving a task between
	// lists is a different operation with its own ordinal bookkeeping, and
	// silently accepting a new list_id on an edit would reorder two lists.
	t.ListID = existing.ListID
	t.Ordinal = existing.Ordinal
	t.CreatedAt = existing.CreatedAt
	t.UpdatedAt = s.timestamp()

	switch {
	case t.Status == StatusDone && existing.Status != StatusDone:
		t.CompletedAt = t.UpdatedAt
	case t.Status != StatusDone:
		t.CompletedAt = ""
	default:
		t.CompletedAt = existing.CompletedAt
	}
	return s.repo.UpdateTask(id, t)
}

// SetStatus is the one-click path behind the checkbox and the board columns.
func (s *Service) SetStatus(id int64, status string) (Task, error) {
	existing, err := s.repo.GetTask(id)
	if err != nil {
		return Task{}, err
	}
	existing.Status = status
	return s.UpdateTask(id, existing)
}

func (s *Service) DeleteTask(id int64) error {
	return s.repo.DeleteTask(id)
}

// ---- Views ----

// Agenda is the "what now" view: everything unfinished with a due date on or
// before the horizon, plus anything already overdue.
type Agenda struct {
	Today    string `json:"today"`
	Overdue  []Task `json:"overdue"`
	DueToday []Task `json:"due_today"`
	Upcoming []Task `json:"upcoming"`
	NoDate   []Task `json:"no_date"`
}

// AgendaHorizonDays bounds the "upcoming" bucket. Two weeks: far enough to plan
// around, near enough that the list stays a list rather than a backlog.
const AgendaHorizonDays = 14

func (s *Service) Agenda() (Agenda, error) {
	today := s.today()
	tasks, err := s.repo.ListTasks(Filter{})
	if err != nil {
		return Agenda{}, err
	}
	horizon := s.now().UTC().AddDate(0, 0, AgendaHorizonDays).Format(DateLayout)

	agenda := Agenda{
		Today:    today,
		Overdue:  []Task{},
		DueToday: []Task{},
		Upcoming: []Task{},
		NoDate:   []Task{},
	}
	for _, t := range tasks {
		switch {
		case t.DueDate == "":
			agenda.NoDate = append(agenda.NoDate, t)
		case t.DueDate < today:
			agenda.Overdue = append(agenda.Overdue, t)
		case t.DueDate == today:
			agenda.DueToday = append(agenda.DueToday, t)
		case t.DueDate <= horizon:
			agenda.Upcoming = append(agenda.Upcoming, t)
		}
	}
	return agenda, nil
}

// CalendarMonth is a month of task due dates, keyed by day.
type CalendarMonth struct {
	Month string            `json:"month"` // YYYY-MM
	Today string            `json:"today"`
	Days  map[string][]Task `json:"days"`
}

// Calendar returns the tasks due in the given month. month is "YYYY-MM"; empty
// means the current month.
//
// Done tasks are included here, unlike the agenda: a calendar is a record of
// when things happened as much as a plan, and a month showing only what slipped
// would misrepresent it.
func (s *Service) Calendar(month string) (CalendarMonth, error) {
	if month == "" {
		month = s.now().UTC().Format("2006-01")
	}
	start, err := time.Parse("2006-01", month)
	if err != nil {
		return CalendarMonth{}, fmt.Errorf("month must be YYYY-MM, got %q", month)
	}
	end := start.AddDate(0, 1, -1)

	tasks, err := s.repo.ListTasks(Filter{
		DueOnly:     true,
		IncludeDone: true,
		DueFrom:     start.Format(DateLayout),
		DueTo:       end.Format(DateLayout),
	})
	if err != nil {
		return CalendarMonth{}, err
	}

	days := make(map[string][]Task, len(tasks))
	for _, t := range tasks {
		days[t.DueDate] = append(days[t.DueDate], t)
	}
	return CalendarMonth{Month: month, Today: s.today(), Days: days}, nil
}
