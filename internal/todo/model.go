// Package todo is the task module: lists, the tasks inside them, notes,
// scheduled events, an agenda, and a calendar over the lot.
//
// Moved here from the RCSA application, where it was owner-scoped because that
// app had signed-in users. Wintermute has none — the boundary is the client
// token, checked once at the API edge — so the scoping is gone rather than
// stubbed out. See internal/todo/repository.go.
//
// Notes and the calendar arrived later, from morpheus, where they were two
// separate modules with a table each and an interface joining them into a
// merged feed. They are folded in here rather than ported as they stood.
// A morpheus note was a line of text that was either outstanding or dealt
// with and optionally landed on a date, which is a task without a list; and
// the merged feed — events plus notes pinned to a day — is what this module's
// calendar already was for due dates. What survives as its own thing is the
// Event: something that happens at a time, rather than something to be done by
// one, which no task field expresses.
package todo

import (
	"fmt"
	"strings"
	"time"
)

// Status values. Three, not two: "doing" is what makes a list usable as a
// working view rather than a checklist, and it is the distinction people
// otherwise invent by prefixing titles with asterisks.
const (
	StatusTodo  = "todo"
	StatusDoing = "doing"
	StatusDone  = "done"
)

var Statuses = []string{StatusTodo, StatusDoing, StatusDone}

const (
	PriorityLow    = "low"
	PriorityNormal = "normal"
	PriorityHigh   = "high"
)

var Priorities = []string{PriorityLow, PriorityNormal, PriorityHigh}

// DateLayout is the wire and storage format for a due date. Dates are stored as
// text rather than as a timestamp because a due date is a calendar day, not an
// instant — "due Friday" does not move when the reader is in another timezone.
const DateLayout = "2006-01-02"

// The notes inbox. Notes are tasks on one reserved list, found by slug rather
// than by title: a list matched by name would be handed to whoever first typed
// "Notes" into the new-list box, along with everything already in it.
const (
	NotesListSlug  = "notes"
	NotesListTitle = "Notes"
	notesListDesc  = "Quick notes. A note with a date shows up on the calendar."
)

// The default task list, on the same footing as the notes inbox and for the
// same reason: something has to catch a task nobody said where to put.
//
// A fresh install has no lists at all, so "add a task to buy milk" had nowhere
// to go — the tool refused, and a model that does not read its own tool
// results then reports the task as created. Found by slug rather than by
// title, so a list somebody named "Tasks" is theirs and not quietly annexed,
// and made on demand so an install that never adds a task never grows it.
const (
	InboxListSlug  = "inbox"
	InboxListTitle = "Tasks"
	inboxListDesc  = "Tasks added without a list of their own."
)

// List is a named collection of tasks.
type List struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Archived    bool   `json:"archived"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	// Slug is set only on lists the server itself keeps and has to reopen —
	// today just the notes inbox. It is not settable through the API: a caller
	// who could claim a slug could take over the inbox.
	Slug string `json:"slug,omitempty"`

	// Counts are filled by ListLists for the index page, which would otherwise
	// need one query per list to show progress.
	TaskCount int `json:"task_count"`
	DoneCount int `json:"done_count"`
}

// Task is one item on a list.
type Task struct {
	ID          int64  `json:"id"`
	ListID      int64  `json:"list_id"`
	Title       string `json:"title"`
	Notes       string `json:"notes"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	DueDate     string `json:"due_date"`
	Ordinal     int    `json:"ordinal"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	CompletedAt string `json:"completed_at"`

	// ListTitle is filled by the calendar and agenda queries, which span lists
	// and are unreadable without saying which list a task came from.
	ListTitle string `json:"list_title,omitempty"`
}

// Overdue reports whether an unfinished task's due date has passed.
func (t Task) Overdue(today string) bool {
	if t.Status == StatusDone || t.DueDate == "" {
		return false
	}
	return t.DueDate < today
}

const (
	maxTitleLen = 200
	maxNotesLen = 4000
)

// Validate normalises a list and reports the first problem.
func (l *List) Validate() error {
	l.Title = strings.TrimSpace(l.Title)
	l.Description = strings.TrimSpace(l.Description)
	if l.Title == "" {
		return fmt.Errorf("a list needs a title")
	}
	if len([]rune(l.Title)) > maxTitleLen {
		return fmt.Errorf("list title must be %d characters or fewer", maxTitleLen)
	}
	if len([]rune(l.Description)) > maxNotesLen {
		return fmt.Errorf("list description must be %d characters or fewer", maxNotesLen)
	}
	return nil
}

// Validate normalises a task and reports the first problem. Blank status and
// priority default rather than failing: the common creation path — typing a
// title into the quick-add box — supplies neither.
func (t *Task) Validate() error {
	t.Title = strings.TrimSpace(t.Title)
	t.Notes = strings.TrimSpace(t.Notes)
	t.Status = strings.TrimSpace(strings.ToLower(t.Status))
	t.Priority = strings.TrimSpace(strings.ToLower(t.Priority))
	t.DueDate = strings.TrimSpace(t.DueDate)

	if t.Title == "" {
		return fmt.Errorf("a task needs a title")
	}
	if len([]rune(t.Title)) > maxTitleLen {
		return fmt.Errorf("task title must be %d characters or fewer", maxTitleLen)
	}
	if len([]rune(t.Notes)) > maxNotesLen {
		return fmt.Errorf("task notes must be %d characters or fewer", maxNotesLen)
	}
	if t.Status == "" {
		t.Status = StatusTodo
	}
	if !contains(Statuses, t.Status) {
		return fmt.Errorf("status must be one of %s, got %q", strings.Join(Statuses, ", "), t.Status)
	}
	if t.Priority == "" {
		t.Priority = PriorityNormal
	}
	if !contains(Priorities, t.Priority) {
		return fmt.Errorf("priority must be one of %s, got %q", strings.Join(Priorities, ", "), t.Priority)
	}
	if t.DueDate != "" {
		if _, err := time.Parse(DateLayout, t.DueDate); err != nil {
			return fmt.Errorf("due date must be YYYY-MM-DD, got %q", t.DueDate)
		}
	}
	return nil
}

func contains(haystack []string, needle string) bool {
	for _, candidate := range haystack {
		if candidate == needle {
			return true
		}
	}
	return false
}

// Filter narrows a task query.
type Filter struct {
	ListID  int64
	Status  string
	Search  string
	DueFrom string
	DueTo   string
	// DueOnly restricts to tasks that carry a due date, which is what the
	// calendar and agenda views want.
	DueOnly bool
	// IncludeDone defaults false on the agenda: a list of things to do should
	// not be mostly things already done.
	IncludeDone bool
}

// Event is a scheduled calendar item — something that happens at a time,
// rather than something to be done by a date, which is what a task's due date
// already covers.
type Event struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// StartAt and EndAt hold "YYYY-MM-DD" for an all-day event and an RFC3339
	// instant in UTC for a timed one. EndAt is empty when no end was given.
	StartAt string `json:"start_at"`
	EndAt   string `json:"end_at"`
	AllDay  bool   `json:"all_day"`
	// Date is the calendar day StartAt falls on, derived rather than stored.
	// A caller grouping events by day should never have to know whether it is
	// looking at a date or a timestamp.
	Date      string `json:"date"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ParseMoment reads an event boundary. It accepts a calendar day
// ("YYYY-MM-DD", which makes the event all-day) or an RFC3339 timestamp, and
// returns the value in the form it is stored, plus whether it was date-only.
//
// A timestamp is normalised to UTC so that stored values sort against each
// other and against a plain date bound. That is what lets one range query
// serve a month view over a mix of all-day and timed events.
func ParseMoment(s string) (string, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false, fmt.Errorf("a date or timestamp is required")
	}
	if t, err := time.Parse(DateLayout, s); err == nil {
		return t.Format(DateLayout), true, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), false, nil
	}
	return "", false, fmt.Errorf("must be YYYY-MM-DD or an RFC3339 timestamp, got %q", s)
}

// day returns the calendar day of a stored moment. Both storage forms lead
// with the date, so this is a slice rather than a parse.
func day(moment string) string {
	if len(moment) >= len("2006-01-02") {
		return moment[:len("2006-01-02")]
	}
	return moment
}

// Validate normalises an event and reports the first problem. It also settles
// the stored form of the boundaries and the derived day, so a caller that has
// validated has an event ready to write.
func (e *Event) Validate() error {
	e.Title = strings.TrimSpace(e.Title)
	e.Description = strings.TrimSpace(e.Description)

	if e.Title == "" {
		return fmt.Errorf("an event needs a title")
	}
	if len([]rune(e.Title)) > maxTitleLen {
		return fmt.Errorf("event title must be %d characters or fewer", maxTitleLen)
	}
	if len([]rune(e.Description)) > maxNotesLen {
		return fmt.Errorf("event description must be %d characters or fewer", maxNotesLen)
	}

	start, startAllDay, err := ParseMoment(e.StartAt)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	e.StartAt = start
	// A date-only start makes the event all-day whatever the flag said: there
	// is no time of day to show, and an event rendered at midnight because a
	// checkbox was left clear is a lie about when it happens.
	e.AllDay = e.AllDay || startAllDay

	if strings.TrimSpace(e.EndAt) != "" {
		end, _, err := ParseMoment(e.EndAt)
		if err != nil {
			return fmt.Errorf("end: %w", err)
		}
		if end < e.StartAt {
			return fmt.Errorf("an event's end must not be before its start")
		}
		e.EndAt = end
	} else {
		e.EndAt = ""
	}

	e.Date = day(e.StartAt)
	return nil
}
