package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"wintermute/internal/tool"
)

// maxToolTasks bounds how many tasks one tool call may create. A model asked
// for "a list for the audit" occasionally decides that means forty items; the
// cap turns that into a clear error it can read and retry from, rather than
// forty rows somebody has to delete by hand.
const maxToolTasks = 40

// registration pairs a definition with the handler that serves it.
type registration struct {
	def     tool.Definition
	handler tool.Handler
}

// Register exposes the task module to the assistant.
//
// This is what the RCSA application's separate "Assistant" page did, arriving
// here as tools on the agent that already exists rather than as a second one.
// That app needed its own Anthropic client, conversation store and tool
// registry because it had no agent; wintermute has all three, and porting them
// would have meant two transcripts, two loops and two places to look when the
// model does something surprising.
//
// The set covers lists, tasks, notes and the calendar — everything this module
// stores, because it is all the same kind of thing: a person's own record of
// what they mean to do, reversible and audited by nobody. The rest of what
// this server can reach — the media library, the model backends — is a
// different decision, and should be made deliberately rather than by extending
// a slice.
//
// The handlers are thin adapters over the same Service methods the HTTP layer
// calls, so a list the model creates goes through the same validation and
// timestamps as one typed into the UI.
func Register(reg *tool.Registry, svc *Service) error {
	tools := []registration{
		{
			def: tool.Definition{
				Name: "list_todo_lists",
				Description: "Read the existing to-do lists, with a task count and a done count for each. " +
					"Call this before create_todo_list whenever the request might already have a list — " +
					"add to the existing one, or say it is already there, rather than creating a duplicate.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"include_archived": {"type": "boolean", "description": "Include archived lists. Defaults to false."}
					}
				}`),
				Risk: tool.RiskRead,
			},
			handler: listListsHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "create_todo_list",
				Description: "Create a to-do list, optionally with its first tasks. " +
					"Use this when asked to make a list, plan something out, or capture a set of actions. " +
					"Check list_todo_lists first so an existing list is added to rather than duplicated.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"title": {"type": "string", "description": "The list's name."},
						"description": {"type": "string", "description": "Optional one-line description."},
						"tasks": {
							"type": "array",
							"description": "Optional initial tasks, in the order they should appear.",
							"items": {
								"type": "object",
								"properties": {
									"title": {"type": "string"},
									"notes": {"type": "string"},
									"priority": {"type": "string", "enum": ["low", "normal", "high"]},
									"due_date": {"type": "string", "description": "YYYY-MM-DD."}
								},
								"required": ["title"]
							}
						}
					},
					"required": ["title"]
				}`),
				// Write rather than read: it creates rows. The client harness's
				// approval policy decides whether that needs confirming.
				Risk: tool.RiskWrite,
			},
			handler: createListHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "add_todo_task",
				Description: "Add one task to an existing list. Use this to extend a list rather than " +
					"creating a second list with a similar name.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"list_id": {"type": "integer", "description": "The list to add to, from list_todo_lists."},
						"title": {"type": "string"},
						"notes": {"type": "string"},
						"priority": {"type": "string", "enum": ["low", "normal", "high"]},
						"due_date": {"type": "string", "description": "YYYY-MM-DD."}
					},
					"required": ["list_id", "title"]
				}`),
				Risk: tool.RiskWrite,
			},
			handler: addTaskHandler(svc),
		},
	}
	// Notes and the calendar keep the tool names they had in morpheus. The
	// storage folded into the task module, but the model's vocabulary did not
	// need to: "note" and "calendar event" are what a person asks for, and a
	// tool called add_todo_task would not be found by a request to jot
	// something down.
	tools = append(tools, noteTools(svc)...)
	tools = append(tools, calendarTools(svc)...)

	for _, t := range tools {
		if err := reg.Register(t.def, t.handler); err != nil {
			return err
		}
	}
	return nil
}

func listListsHandler(svc *Service) tool.Handler {
	type input struct {
		IncludeArchived bool `json:"include_archived"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		// An argument-free call can arrive as an empty body rather than "{}",
		// which is not valid JSON to unmarshal into.
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
		}
		lists, err := svc.ListLists(in.IncludeArchived)
		if err != nil {
			return "", err
		}
		if len(lists) == 0 {
			return "There are no to-do lists yet.", nil
		}

		var b strings.Builder
		for _, l := range lists {
			fmt.Fprintf(&b, "#%d %s — %d task(s), %d done", l.ID, l.Title, l.TaskCount, l.DoneCount)
			if l.Archived {
				b.WriteString(" (archived)")
			}
			if l.Description != "" {
				fmt.Fprintf(&b, "\n    %s", l.Description)
			}
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}

type taskInput struct {
	Title    string `json:"title"`
	Notes    string `json:"notes"`
	Priority string `json:"priority"`
	DueDate  string `json:"due_date"`
}

func createListHandler(svc *Service) tool.Handler {
	type input struct {
		Title       string      `json:"title"`
		Description string      `json:"description"`
		Tasks       []taskInput `json:"tasks"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if len(in.Tasks) > maxToolTasks {
			return "", fmt.Errorf("that is %d tasks; create at most %d in one call", len(in.Tasks), maxToolTasks)
		}

		list, err := svc.CreateList(List{Title: in.Title, Description: in.Description})
		if err != nil {
			return "", err
		}

		created := 0
		for _, t := range in.Tasks {
			if _, err := svc.CreateTask(Task{
				ListID:   list.ID,
				Title:    t.Title,
				Notes:    t.Notes,
				Priority: t.Priority,
				DueDate:  t.DueDate,
			}); err != nil {
				// The list already exists, so the partial result is reported
				// rather than hidden behind a bare error: the model needs to
				// know what to add rather than start again.
				return "", fmt.Errorf("created list #%d %q with %d task(s), then failed on %q: %w",
					list.ID, list.Title, created, t.Title, err)
			}
			created++
		}
		return fmt.Sprintf("Created list #%d %q with %d task(s).", list.ID, list.Title, created), nil
	}
}

func addTaskHandler(svc *Service) tool.Handler {
	type input struct {
		ListID int64 `json:"list_id"`
		taskInput
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		task, err := svc.CreateTask(Task{
			ListID:   in.ListID,
			Title:    in.Title,
			Notes:    in.Notes,
			Priority: in.Priority,
			DueDate:  in.DueDate,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Added task #%d %q to list #%d.", task.ID, task.Title, task.ListID), nil
	}
}

// ---- Notes ----

// noteTools are the note-shaped view of the task module, carried over from
// morpheus with their names and arguments intact.
//
// They read and write the notes list and nothing else. set_note_status and
// delete_note refuse a task that is not a note, so a model that mixes up an id
// from list_todo_lists with one from list_notes is told rather than obeyed.
func noteTools(svc *Service) []registration {
	return []registration{
		{
			def: tool.Definition{
				Name: "create_note",
				Description: "Write down a short note. Use this to jot something down, rather than " +
					"create_todo_list, when there is one thing to record and no list to plan. " +
					"The note starts outstanding; set_note_status marks it done. " +
					"Give it an event_date to pin it to a day, which puts it on the calendar.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"body": {"type": "string", "description": "The note's text."},
						"event_date": {"type": "string", "description": "Optional day for the note, YYYY-MM-DD. Omit to leave it off the calendar."}
					},
					"required": ["body"]
				}`),
				Risk: tool.RiskWrite,
			},
			handler: createNoteHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "list_notes",
				Description: "Read the notes, newest first. Each reports whether it is still " +
					"outstanding, and the day it is pinned to if it has one.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"limit": {"type": "integer", "description": "Return at most this many. Omit for all of them."}
					}
				}`),
				Risk: tool.RiskRead,
			},
			handler: listNotesHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "set_note_status",
				Description: "Mark a note done, or put it back to outstanding. The note's text is " +
					"unchanged — this only records whether it has been dealt with.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"id": {"type": "integer", "description": "The note's id, from list_notes."},
						"status": {"type": "string", "enum": ["todo", "doing", "done"]}
					},
					"required": ["id", "status"]
				}`),
				Risk: tool.RiskWrite,
			},
			handler: setNoteStatusHandler(svc),
		},
		{
			def: tool.Definition{
				Name:        "delete_note",
				Description: "Delete a note by id. To record that a note is dealt with while keeping it, use set_note_status instead.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {"id": {"type": "integer", "description": "The note's id, from list_notes."}},
					"required": ["id"]
				}`),
				// Destructive, not write: there is no undo, and the text is
				// gone. That is worth a confirmation even from an operator
				// running with writes auto-approved.
				Risk: tool.RiskDestructive,
			},
			handler: deleteNoteHandler(svc),
		},
	}
}

func createNoteHandler(svc *Service) tool.Handler {
	type input struct {
		Body      string `json:"body"`
		EventDate string `json:"event_date"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		note, err := svc.CreateNote(in.Body, in.EventDate)
		if err != nil {
			return "", err
		}
		if note.DueDate != "" {
			return fmt.Sprintf("Noted #%d, on the calendar for %s.", note.ID, note.DueDate), nil
		}
		return fmt.Sprintf("Noted #%d.", note.ID), nil
	}
}

func listNotesHandler(svc *Service) tool.Handler {
	type input struct {
		Limit int `json:"limit"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
		}
		notes, err := svc.ListNotes()
		if err != nil {
			return "", err
		}
		if len(notes) == 0 {
			return "There are no notes yet.", nil
		}
		if in.Limit > 0 && in.Limit < len(notes) {
			notes = notes[:in.Limit]
		}

		var b strings.Builder
		for _, n := range notes {
			fmt.Fprintf(&b, "#%d [%s] %s", n.ID, n.Status, NoteBody(n))
			if n.DueDate != "" {
				fmt.Fprintf(&b, " (%s)", n.DueDate)
			}
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}

func setNoteStatusHandler(svc *Service) tool.Handler {
	type input struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		note, err := svc.SetNoteStatus(in.ID, in.Status)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Note #%d is now %s.", note.ID, note.Status), nil
	}
}

func deleteNoteHandler(svc *Service) tool.Handler {
	type input struct {
		ID int64 `json:"id"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if err := svc.DeleteNote(in.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted note #%d.", in.ID), nil
	}
}

// ---- Calendar ----

// calendarTools expose scheduled events, and the reading tool covers the whole
// calendar rather than only events: morpheus's list_calendar returned a feed
// merging events with dated notes, and answering "what is on next week" with
// half of it would be worse than not answering.
func calendarTools(svc *Service) []registration {
	return []registration{
		{
			def: tool.Definition{
				Name: "create_calendar_event",
				Description: "Schedule an event: something that happens at a time. For something to " +
					"be done by a date, add a task or a dated note instead — those can be ticked off, " +
					"and an event cannot.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"title": {"type": "string", "description": "What the event is."},
						"description": {"type": "string", "description": "Optional longer description."},
						"start": {"type": "string", "description": "YYYY-MM-DD for an all-day event, or an RFC3339 timestamp for a timed one."},
						"end": {"type": "string", "description": "Optional end, same formats as start."},
						"all_day": {"type": "boolean", "description": "Whether this takes the whole day. A date-only start is all-day regardless."}
					},
					"required": ["title", "start"]
				}`),
				Risk: tool.RiskWrite,
			},
			handler: createEventHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "list_calendar",
				Description: "Read the calendar for a date window: the events scheduled in it and the " +
					"tasks and notes due in it, day by day. Defaults to the current month.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"from": {"type": "string", "description": "Window start, inclusive, YYYY-MM-DD."},
						"to": {"type": "string", "description": "Window end, exclusive, YYYY-MM-DD."},
						"month": {"type": "string", "description": "A whole month as YYYY-MM, instead of from/to."}
					}
				}`),
				Risk: tool.RiskRead,
			},
			handler: listCalendarHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "delete_calendar_event",
				Description: "Delete a calendar event by id. This does not touch tasks or notes that " +
					"fall on the same day — remove those with delete_note or from their list.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {"id": {"type": "integer", "description": "The event's id, from list_calendar."}},
					"required": ["id"]
				}`),
				Risk: tool.RiskDestructive,
			},
			handler: deleteEventHandler(svc),
		},
	}
}

func createEventHandler(svc *Service) tool.Handler {
	type input struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Start       string `json:"start"`
		End         string `json:"end"`
		AllDay      bool   `json:"all_day"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		event, err := svc.CreateEvent(Event{
			Title:       in.Title,
			Description: in.Description,
			StartAt:     in.Start,
			EndAt:       in.End,
			AllDay:      in.AllDay,
		})
		if err != nil {
			return "", err
		}
		when := event.StartAt
		if event.EndAt != "" {
			when += " to " + event.EndAt
		}
		return fmt.Sprintf("Scheduled #%d %q for %s.", event.ID, event.Title, when), nil
	}
}

func listCalendarHandler(svc *Service) tool.Handler {
	type input struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Month string `json:"month"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
		}

		var (
			cal CalendarMonth
			err error
		)
		switch {
		case in.From != "" || in.To != "":
			if in.From == "" || in.To == "" {
				return "", fmt.Errorf("give both from and to, or neither")
			}
			cal, err = svc.CalendarBetween(in.From, in.To)
		default:
			cal, err = svc.Calendar(in.Month)
		}
		if err != nil {
			return "", err
		}

		// Days come back keyed by date from two maps, so they are collected
		// and sorted here — a calendar read out of order is not a calendar.
		days := make([]string, 0, len(cal.Days)+len(cal.Events))
		for d := range cal.Days {
			days = append(days, d)
		}
		for d := range cal.Events {
			if _, dup := cal.Days[d]; !dup {
				days = append(days, d)
			}
		}
		sort.Strings(days)

		if len(days) == 0 {
			return fmt.Sprintf("Nothing on the calendar between %s and %s.", cal.From, cal.To), nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s to %s (today is %s):\n", cal.From, cal.To, cal.Today)
		for _, d := range days {
			fmt.Fprintf(&b, "%s\n", d)
			for _, e := range cal.Events[d] {
				fmt.Fprintf(&b, "    event #%d %s", e.ID, e.Title)
				if !e.AllDay {
					fmt.Fprintf(&b, " at %s", e.StartAt)
				}
				b.WriteString("\n")
			}
			for _, t := range cal.Days[d] {
				fmt.Fprintf(&b, "    %s #%d [%s] %s\n", dueKind(t), t.ID, t.Status, t.Title)
			}
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}

// dueKind says whether a due-dated item reads as a note or a task, so the
// model can tell which tool will act on it.
func dueKind(t Task) string {
	if t.ListTitle == NotesListTitle {
		return "note"
	}
	return "task on " + t.ListTitle + ":"
}

func deleteEventHandler(svc *Service) tool.Handler {
	type input struct {
		ID int64 `json:"id"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if err := svc.DeleteEvent(in.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted event #%d.", in.ID), nil
	}
}
