// Package todo is the task module: lists, the tasks inside them, an agenda,
// and a due-date calendar over the lot.
//
// Moved here from the RCSA application, where it was owner-scoped because that
// app had signed-in users. Wintermute has none — the boundary is the client
// token, checked once at the API edge — so the scoping is gone rather than
// stubbed out. See internal/todo/repository.go.
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

// List is a named collection of tasks.
type List struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Archived    bool   `json:"archived"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`

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
