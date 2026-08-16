package todo

import (
	"errors"
	"fmt"
	"sort"
	"strings"
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
	// A slug names a list the server owns and reopens by name. Accepting one
	// from a caller would let it claim the notes inbox and everything filed
	// there, so it is cleared here and set only by NotesList.
	l.Slug = ""
	return s.createList(l)
}

func (s *Service) createList(l List) (List, error) {
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

// ---- Notes ----
//
// A note is a task on the reserved notes list. Morpheus kept notes in a table
// of their own and the calendar reached into it through an interface to show
// the dated ones; folding them in means a note lands in the agenda and on the
// calendar because it *is* a task, with nothing joining two models at read
// time. What is left note-shaped is the vocabulary — a body rather than a
// title, an event date rather than a due date — and the ordering.

// NotesList returns the notes inbox, creating it the first time anything asks.
// It is made on demand rather than seeded by the migration so an install that
// never takes a note never grows a list nobody asked for.
func (s *Service) NotesList() (List, error) {
	list, err := s.repo.ListBySlug(NotesListSlug)
	if err == nil {
		return list, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return List{}, err
	}

	created, err := s.createList(List{
		Title:       NotesListTitle,
		Description: notesListDesc,
		Slug:        NotesListSlug,
	})
	if err != nil {
		// Losing a race to a concurrent request is the expected failure here:
		// the slug is unique, so the loser re-reads rather than reporting a
		// conflict nobody caused.
		if list, lookupErr := s.repo.ListBySlug(NotesListSlug); lookupErr == nil {
			return list, nil
		}
		return List{}, err
	}
	return created, nil
}

// ListNotes returns the notes, newest first.
//
// The ordering is the one place a note is read differently from a task: a list
// of tasks is ordered by what is due, but notes are a stream, and the one just
// written belongs at the top. Done notes are included — morpheus showed them
// struck through rather than hiding them, and a note already dealt with is
// still a record that it was.
func (s *Service) ListNotes() ([]Task, error) {
	list, err := s.NotesList()
	if err != nil {
		return nil, err
	}
	notes, err := s.repo.ListTasks(Filter{ListID: list.ID, IncludeDone: true})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(notes, func(i, j int) bool {
		if notes[i].CreatedAt != notes[j].CreatedAt {
			return notes[i].CreatedAt > notes[j].CreatedAt
		}
		return notes[i].ID > notes[j].ID
	})
	return notes, nil
}

// CreateNote files a note. eventDate is optional and pins the note to a day,
// which is what puts it on the calendar.
func (s *Service) CreateNote(body, eventDate string) (Task, error) {
	if strings.TrimSpace(body) == "" {
		return Task{}, fmt.Errorf("a note needs some text")
	}
	eventDate = strings.TrimSpace(eventDate)
	if eventDate != "" {
		// Checked here rather than left to the task's own validation so the
		// message names the field the caller actually filled in.
		if _, err := time.Parse(DateLayout, eventDate); err != nil {
			return Task{}, fmt.Errorf("event date must be YYYY-MM-DD, got %q", eventDate)
		}
	}
	list, err := s.NotesList()
	if err != nil {
		return Task{}, err
	}
	title, overflow := noteFields(body)
	return s.CreateTask(Task{
		ListID:  list.ID,
		Title:   title,
		Notes:   overflow,
		DueDate: eventDate,
	})
}

// SetNoteStatus marks a note done, or puts it back.
func (s *Service) SetNoteStatus(id int64, status string) (Task, error) {
	if _, err := s.requireNote(id); err != nil {
		return Task{}, err
	}
	return s.SetStatus(id, status)
}

// DeleteNote removes a note.
func (s *Service) DeleteNote(id int64) error {
	if _, err := s.requireNote(id); err != nil {
		return err
	}
	return s.repo.DeleteTask(id)
}

// requireNote refuses a task that is not a note. The routes and the assistant
// tools that call it are named for notes, and letting one delete an arbitrary
// task by id would make the name a lie — which matters most for the model,
// which picks a tool by what it claims to touch.
func (s *Service) requireNote(id int64) (Task, error) {
	task, err := s.repo.GetTask(id)
	if err != nil {
		return Task{}, err
	}
	list, err := s.NotesList()
	if err != nil {
		return Task{}, err
	}
	if task.ListID != list.ID {
		return Task{}, fmt.Errorf("#%d is a task on %q, not a note", id, task.ListTitle)
	}
	return task, nil
}

// NoteBody returns a note's full text. It is the task's title unless the note
// was too long to fit in one, in which case noteFields put the whole thing in
// the notes field — see there for why it is not simply truncated.
func NoteBody(t Task) string {
	if t.Notes != "" {
		return t.Notes
	}
	return t.Title
}

// noteFields splits a note's text into a task title and, when it will not fit,
// the remainder. Morpheus stored a note body as unbounded text and its import
// accepted whatever a spreadsheet cell held, so truncating on the way in would
// silently lose what somebody pasted. The title keeps a readable opening and
// the notes field keeps the whole thing.
func noteFields(body string) (title, overflow string) {
	body = strings.TrimSpace(body)
	runes := []rune(body)
	if len(runes) <= maxTitleLen {
		return body, ""
	}
	head := string(runes[:maxTitleLen-1])
	// Cut back to a word boundary, but only if one is near enough that the
	// title still says something.
	if i := strings.LastIndexAny(head, " \t\n"); i > maxTitleLen/2 {
		head = head[:i]
	}
	return strings.TrimSpace(head) + "…", body
}

// ---- Events ----

// ListEvents returns the events starting in the half-open window [from, to),
// as "YYYY-MM-DD" bounds. An empty bound is unbounded on that side.
func (s *Service) ListEvents(from, to string) ([]Event, error) {
	return s.repo.ListEvents(from, to)
}

// CreateEvent schedules an event. Validate settles the stored form of the
// boundaries, so what reaches the repository is already normalised.
func (s *Service) CreateEvent(e Event) (Event, error) {
	if err := e.Validate(); err != nil {
		return Event{}, err
	}
	e.CreatedAt = s.timestamp()
	e.UpdatedAt = e.CreatedAt
	return s.repo.CreateEvent(e)
}

func (s *Service) GetEvent(id int64) (Event, error) {
	return s.repo.GetEvent(id)
}

func (s *Service) DeleteEvent(id int64) error {
	return s.repo.DeleteEvent(id)
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

// CalendarMonth is a window of the calendar: the tasks due in it and the
// events scheduled in it, each keyed by day.
//
// Tasks and events are kept apart rather than flattened into one list of
// entries. Morpheus merged them, into a feed whose every reader immediately
// switched on a kind field to find out which of two shapes it had — and the
// two are not alike enough to share a row: a task can be ticked off and can be
// overdue, an event can only arrive.
type CalendarMonth struct {
	// Month is "YYYY-MM", set only when the window was built from a month.
	Month string `json:"month,omitempty"`
	// From and To bound the window as "YYYY-MM-DD", half-open: To is the first
	// day *not* included.
	From   string             `json:"from"`
	To     string             `json:"to"`
	Today  string             `json:"today"`
	Days   map[string][]Task  `json:"days"`
	Events map[string][]Event `json:"events"`
}

// Calendar returns the given month's calendar. month is "YYYY-MM"; empty means
// the current month.
func (s *Service) Calendar(month string) (CalendarMonth, error) {
	if month == "" {
		month = s.now().UTC().Format("2006-01")
	}
	start, err := time.Parse("2006-01", month)
	if err != nil {
		return CalendarMonth{}, fmt.Errorf("month must be YYYY-MM, got %q", month)
	}

	cal, err := s.CalendarBetween(start.Format(DateLayout), start.AddDate(0, 1, 0).Format(DateLayout))
	if err != nil {
		return CalendarMonth{}, err
	}
	cal.Month = month
	return cal, nil
}

// CalendarBetween returns the calendar over the half-open window [from, to).
//
// This is what morpheus called the calendar feed, where it merged a table of
// events with the dated rows of a separate notes table. Half of that merge is
// gone: a dated note is a task with a due date, so it arrives with the tasks
// and needs nothing joining it in.
//
// Done tasks are included, unlike on the agenda: a calendar is a record of
// when things happened as much as a plan, and a month showing only what
// slipped would misrepresent it.
func (s *Service) CalendarBetween(from, to string) (CalendarMonth, error) {
	if _, err := time.Parse(DateLayout, from); err != nil {
		return CalendarMonth{}, fmt.Errorf("from must be YYYY-MM-DD, got %q", from)
	}
	toDay, err := time.Parse(DateLayout, to)
	if err != nil {
		return CalendarMonth{}, fmt.Errorf("to must be YYYY-MM-DD, got %q", to)
	}
	if to < from {
		return CalendarMonth{}, fmt.Errorf("to must not be before from")
	}

	tasks, err := s.repo.ListTasks(Filter{
		DueOnly:     true,
		IncludeDone: true,
		DueFrom:     from,
		// The task filter's upper bound is inclusive, so the exclusive end of
		// the window is the day after the last one wanted.
		DueTo: toDay.AddDate(0, 0, -1).Format(DateLayout),
	})
	if err != nil {
		return CalendarMonth{}, err
	}
	events, err := s.repo.ListEvents(from, to)
	if err != nil {
		return CalendarMonth{}, err
	}

	days := make(map[string][]Task, len(tasks))
	for _, t := range tasks {
		days[t.DueDate] = append(days[t.DueDate], t)
	}
	byDay := make(map[string][]Event, len(events))
	for _, e := range events {
		byDay[e.Date] = append(byDay[e.Date], e)
	}
	return CalendarMonth{
		From:   from,
		To:     to,
		Today:  s.today(),
		Days:   days,
		Events: byDay,
	}, nil
}
