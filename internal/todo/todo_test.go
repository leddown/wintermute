package todo

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"wintermute/internal/store"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewService(NewSQLiteRepository(st.DB()))
}

func mustNote(t *testing.T, svc *Service, body, eventDate string) Task {
	t.Helper()
	note, err := svc.CreateNote(body, eventDate)
	if err != nil {
		t.Fatalf("CreateNote(%q, %q): %v", body, eventDate, err)
	}
	return note
}

func mustEvent(t *testing.T, svc *Service, e Event) Event {
	t.Helper()
	created, err := svc.CreateEvent(e)
	if err != nil {
		t.Fatalf("CreateEvent(%+v): %v", e, err)
	}
	return created
}

// ---- notes ----

// The inbox is made on demand and then reused. Two notes landing on two lists
// would be the failure that matters: notes would still save, and half of them
// would stop appearing.
func TestNotesListIsCreatedOnceAndReused(t *testing.T) {
	svc := newTestService(t)

	lists, err := svc.ListLists(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 0 {
		t.Fatalf("a fresh install has %d list(s), want none until something asks", len(lists))
	}

	first := mustNote(t, svc, "one", "")
	second := mustNote(t, svc, "two", "")
	if first.ListID != second.ListID {
		t.Fatalf("notes filed on lists %d and %d, want one inbox", first.ListID, second.ListID)
	}

	lists, err = svc.ListLists(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 1 || lists[0].Slug != NotesListSlug {
		t.Fatalf("lists = %+v, want exactly the %q inbox", lists, NotesListSlug)
	}
}

// The slug is what makes the inbox findable, so a caller must not be able to
// claim it and inherit everything filed there.
func TestCreateListCannotClaimTheNotesSlug(t *testing.T) {
	svc := newTestService(t)

	imposter, err := svc.CreateList(List{Title: "Notes", Slug: NotesListSlug})
	if err != nil {
		t.Fatal(err)
	}
	if imposter.Slug != "" {
		t.Fatalf("created list kept slug %q, want it cleared", imposter.Slug)
	}

	note := mustNote(t, svc, "mine", "")
	if note.ListID == imposter.ID {
		t.Fatal("note was filed on the caller's list, not the server's inbox")
	}
}

func TestCreateNoteValidation(t *testing.T) {
	svc := newTestService(t)

	if _, err := svc.CreateNote("   ", ""); err == nil {
		t.Fatal("a blank note was accepted")
	}
	if _, err := svc.CreateNote("dentist", "14/07/2026"); err == nil {
		t.Fatal("a non-ISO event date was accepted")
	}

	note := mustNote(t, svc, "dentist", "2026-07-14")
	if note.DueDate != "2026-07-14" {
		t.Fatalf("event date = %q, want it stored as the due date", note.DueDate)
	}
	if note.Status != StatusTodo {
		t.Fatalf("status = %q, want a new note to be outstanding", note.Status)
	}
}

// Morpheus stored a note body as unbounded text. A task title is capped, so a
// long note keeps its full text rather than being cut down to fit.
func TestLongNoteKeepsItsWholeBody(t *testing.T) {
	svc := newTestService(t)

	body := strings.Repeat("word ", 100) + "end"
	note := mustNote(t, svc, body, "")

	if got := NoteBody(note); got != strings.TrimSpace(body) {
		t.Fatalf("NoteBody lost text: %d chars stored, %d written", len(got), len(strings.TrimSpace(body)))
	}
	if len([]rune(note.Title)) > maxTitleLen {
		t.Fatalf("title is %d runes, over the %d cap", len([]rune(note.Title)), maxTitleLen)
	}
	if !strings.HasSuffix(note.Title, "…") {
		t.Fatalf("title %q does not show it was shortened", note.Title)
	}
}

func TestListNotesIsNewestFirstAndKeepsDoneOnes(t *testing.T) {
	svc := newTestService(t)

	first := mustNote(t, svc, "first", "")
	second := mustNote(t, svc, "second", "")
	if _, err := svc.SetNoteStatus(first.ID, StatusDone); err != nil {
		t.Fatal(err)
	}

	notes, err := svc.ListNotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 {
		t.Fatalf("got %d notes, want both including the done one", len(notes))
	}
	if notes[0].ID != second.ID {
		t.Fatalf("first entry is #%d, want the newest note #%d", notes[0].ID, second.ID)
	}
	if notes[1].Status != StatusDone {
		t.Fatalf("done note came back as %q", notes[1].Status)
	}
}

// The note routes and tools are named for notes; acting on an arbitrary task
// through them would make the name a lie, which matters most for the model.
func TestNoteOperationsRefuseAnOrdinaryTask(t *testing.T) {
	svc := newTestService(t)

	list, err := svc.CreateList(List{Title: "Audit"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(Task{ListID: list.ID, Title: "collect evidence"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetNoteStatus(task.ID, StatusDone); err == nil {
		t.Fatal("SetNoteStatus changed a task that is not a note")
	}
	if err := svc.DeleteNote(task.ID); err == nil {
		t.Fatal("DeleteNote removed a task that is not a note")
	}
	if _, err := svc.GetTask(task.ID); err != nil {
		t.Fatalf("the task should be untouched: %v", err)
	}

	note := mustNote(t, svc, "keep", "")
	if err := svc.DeleteNote(note.ID); err != nil {
		t.Fatalf("DeleteNote on a real note: %v", err)
	}
	if _, err := svc.GetTask(note.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted note still readable: %v", err)
	}
}

// ---- events ----

func TestEventValidateSettlesTheStoredForm(t *testing.T) {
	tests := []struct {
		name       string
		event      Event
		wantStart  string
		wantEnd    string
		wantAllDay bool
		wantErr    string
	}{
		{
			name:       "a date-only start is all-day whatever the flag said",
			event:      Event{Title: "Offsite", StartAt: "2026-07-20"},
			wantStart:  "2026-07-20",
			wantAllDay: true,
		},
		{
			name:      "a timestamp is normalised to UTC so it sorts against dates",
			event:     Event{Title: "Standup", StartAt: "2026-07-21T11:00:00+02:00"},
			wantStart: "2026-07-21T09:00:00Z",
		},
		{
			name:      "an end is kept",
			event:     Event{Title: "Standup", StartAt: "2026-07-21T09:00:00Z", EndAt: "2026-07-21T09:15:00Z"},
			wantStart: "2026-07-21T09:00:00Z",
			wantEnd:   "2026-07-21T09:15:00Z",
		},
		{
			name:    "an end before the start is refused",
			event:   Event{Title: "Backwards", StartAt: "2026-07-21T09:00:00Z", EndAt: "2026-07-20T09:00:00Z"},
			wantErr: "must not be before",
		},
		{
			name:    "a title is required",
			event:   Event{StartAt: "2026-07-20"},
			wantErr: "needs a title",
		},
		{
			name:    "a start is required",
			event:   Event{Title: "When?"},
			wantErr: "start:",
		},
		{
			name:    "an unparseable start is refused",
			event:   Event{Title: "Offsite", StartAt: "20th July"},
			wantErr: "start:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.event
			err := e.Validate()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if e.StartAt != tc.wantStart {
				t.Errorf("start = %q, want %q", e.StartAt, tc.wantStart)
			}
			if e.EndAt != tc.wantEnd {
				t.Errorf("end = %q, want %q", e.EndAt, tc.wantEnd)
			}
			if e.AllDay != tc.wantAllDay {
				t.Errorf("all_day = %v, want %v", e.AllDay, tc.wantAllDay)
			}
			if e.Date != tc.wantStart[:len("2006-01-02")] {
				t.Errorf("date = %q, want the start's day", e.Date)
			}
		})
	}
}

// ---- calendar ----

// This is the merge morpheus did across two tables at read time: events, plus
// whatever is pinned to a day. Half of it is now free, because a dated note is
// a task with a due date.
func TestCalendarBetweenMergesEventsWithDatedNotesAndTasks(t *testing.T) {
	svc := newTestService(t)

	note := mustNote(t, svc, "dentist", "2026-07-14")
	mustNote(t, svc, "undated", "")

	list, err := svc.CreateList(List{Title: "Audit"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(Task{ListID: list.ID, Title: "evidence", DueDate: "2026-07-14"})
	if err != nil {
		t.Fatal(err)
	}
	allDay := mustEvent(t, svc, Event{Title: "Offsite", StartAt: "2026-07-20"})
	timed := mustEvent(t, svc, Event{Title: "Standup", StartAt: "2026-07-21T09:00:00Z"})
	mustEvent(t, svc, Event{Title: "Next month", StartAt: "2026-08-03"})

	cal, err := svc.CalendarBetween("2026-07-01", "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}

	if got := cal.Days["2026-07-14"]; len(got) != 2 {
		t.Fatalf("2026-07-14 holds %d due item(s), want the note and the task", len(got))
	}
	ids := map[int64]bool{}
	for _, d := range cal.Days["2026-07-14"] {
		ids[d.ID] = true
	}
	if !ids[note.ID] || !ids[task.ID] {
		t.Fatalf("the day is missing the note or the task: %+v", cal.Days["2026-07-14"])
	}

	// An all-day event and a timed one land on their own days, which is what
	// storing both as leading-date text is for.
	if got := cal.Events["2026-07-20"]; len(got) != 1 || got[0].ID != allDay.ID {
		t.Fatalf("all-day event missing from its day: %+v", cal.Events)
	}
	if got := cal.Events["2026-07-21"]; len(got) != 1 || got[0].ID != timed.ID {
		t.Fatalf("timed event missing from its day: %+v", cal.Events)
	}
	if _, ok := cal.Events["2026-08-03"]; ok {
		t.Fatal("an event past the exclusive end of the window was included")
	}
}

// The window is half-open. An off-by-one here shows a day twice when the UI
// pages from one month to the next.
func TestCalendarBetweenIsHalfOpen(t *testing.T) {
	svc := newTestService(t)

	mustNote(t, svc, "last day of july", "2026-07-31")
	mustEvent(t, svc, Event{Title: "First of august", StartAt: "2026-08-01"})

	july, err := svc.CalendarBetween("2026-07-01", "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(july.Days["2026-07-31"]) != 1 {
		t.Fatal("the last day of the window was excluded")
	}
	if len(july.Events["2026-08-01"]) != 0 {
		t.Fatal("the exclusive end day was included")
	}

	august, err := svc.CalendarBetween("2026-08-01", "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(august.Events["2026-08-01"]) != 1 {
		t.Fatal("the first day of the next window was excluded from it too")
	}
}

func TestCalendarMonthRejectsBadInput(t *testing.T) {
	svc := newTestService(t)

	if _, err := svc.Calendar("July 2026"); err == nil {
		t.Fatal("a non-ISO month was accepted")
	}
	if _, err := svc.CalendarBetween("2026-08-01", "2026-07-01"); err == nil {
		t.Fatal("a backwards window was accepted")
	}

	cal, err := svc.Calendar("2026-07")
	if err != nil {
		t.Fatal(err)
	}
	if cal.From != "2026-07-01" || cal.To != "2026-08-01" {
		t.Fatalf("month 2026-07 spans %s..%s, want 2026-07-01..2026-08-01", cal.From, cal.To)
	}
}

// ---- imports ----

func TestImportNotes(t *testing.T) {
	svc := newTestService(t)

	// Column order is irrelevant, unknown columns are ignored, blank lines do
	// not count as rows, and one bad date does not abort the file.
	result, err := svc.ImportNotes([][]string{
		{"Event_Date", "body", "colour"},
		{"", "buy milk", "green"},
		{"2026-07-14", "dentist", ""},
		{"", "", ""},
		{"14/07/2026", "bad date", ""},
		{"2026-07-15", "", ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalRows != 4 {
		t.Fatalf("total = %d, want the blank line skipped", result.TotalRows)
	}
	if result.Imported != 2 || result.Failed != 2 {
		t.Fatalf("imported %d / failed %d, want 2 / 2", result.Imported, result.Failed)
	}
	if len(result.Errors) != 2 || result.Errors[0].Row != 5 {
		t.Fatalf("errors = %+v, want the bad rows named by their file line", result.Errors)
	}

	notes, err := svc.ListNotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 {
		t.Fatalf("stored %d notes, want the 2 good rows", len(notes))
	}

	if _, err := svc.ImportNotes([][]string{{"note", "when"}, {"x", ""}}); err == nil {
		t.Fatal("a file with no body column was accepted")
	}
	if _, err := svc.ImportNotes(nil); err == nil {
		t.Fatal("an empty file was accepted")
	}
}

func TestImportEvents(t *testing.T) {
	svc := newTestService(t)

	result, err := svc.ImportEvents([][]string{
		{"title", "start", "end", "description", "all_day"},
		{"Team offsite", "2026-07-20", "", "Annual planning", "true"},
		{"Standup", "2026-07-21T09:00:00Z", "2026-07-21T09:15:00Z", "", "false"},
		{"No start", "", "", "", ""},
		{"Bad flag", "2026-07-22", "", "", "perhaps"},
		{"Backwards", "2026-07-23", "2026-07-22", "", ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 || result.Failed != 3 {
		t.Fatalf("imported %d / failed %d, want 2 / 3: %+v", result.Imported, result.Failed, result.Errors)
	}

	events, err := svc.ListEvents("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("stored %d events, want the 2 good rows", len(events))
	}
	if !events[0].AllDay {
		t.Errorf("the date-only row is not all-day: %+v", events[0])
	}
	if events[1].AllDay {
		t.Errorf("the timestamped row was marked all-day: %+v", events[1])
	}
	if events[1].EndAt != "2026-07-21T09:15:00Z" {
		t.Errorf("end = %q, want it kept", events[1].EndAt)
	}

	if _, err := svc.ImportEvents([][]string{{"title", "when"}, {"x", "2026-07-20"}}); err == nil {
		t.Fatal("a file with no start column was accepted")
	}
}

func TestParseBoolCell(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{in: "", want: false},
		{in: " TRUE ", want: true},
		{in: "yes", want: true},
		{in: "1", want: true},
		{in: "No", want: false},
		{in: "0", want: false},
		{in: "perhaps", wantErr: true},
	} {
		got, err := parseBoolCell(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseBoolCell(%q) accepted it", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBoolCell(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseBoolCell(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
