package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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
	tools = append(tools, taskTools(svc)...)
	tools = append(tools, noteTools(svc)...)
	tools = append(tools, calendarTools(svc)...)

	for _, t := range tools {
		if err := reg.Register(t.def, t.handler); err != nil {
			return err
		}
	}
	return nil
}

// ---- Dates the model actually sends ----
//
// The HTTP API takes YYYY-MM-DD and nothing else, because the browser sends it
// from a date input and anything else is a bug. A tool argument is not that:
// it is whatever a language model made of "due 18/8/2026", and a small local
// model will hand over the human form unchanged.
//
// Rejecting it is worse than it looks. The model reads the error, tries again,
// gets the same error, and burns the whole tool-call budget on one date — the
// turn dies with "exceeded its tool-call budget", which names neither the tool
// nor the date. So the tool layer normalises what it can, and when it truly
// cannot it says exactly what to send instead, so the retry is the last one.
var toolDateLayouts = []string{
	DateLayout, "2006/01/02", "2006.01.02",
	"2 January 2006", "2 Jan 2006", "January 2, 2006", "Jan 2, 2006",
	"January 2 2006", "Jan 2 2006",
}

// numericDate matches d/m/y and d-m-y in either order, which is the family
// that needs a rule rather than a layout.
var numericDate = regexp.MustCompile(`^(\d{1,2})[/.-](\d{1,2})[/.-](\d{4})$`)

// normaliseToolDate converts a date as written into YYYY-MM-DD.
//
// The d/m vs m/d ambiguity is resolved only when the numbers decide it: 18/8
// can only be day-first, 8/18 can only be month-first. When both are 12 or
// under it guesses nothing — silently picking one would put the task on the
// wrong day and nobody would find out until it was late. It returns both
// readings instead, which is a thing the model can act on.
func normaliseToolDate(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	for _, layout := range toolDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(DateLayout), nil
		}
	}
	if m := numericDate.FindStringSubmatch(s); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		year, _ := strconv.Atoi(m[3])
		switch {
		case a > 12 && b <= 12:
			return buildDate(year, b, a, s) // day first
		case b > 12 && a <= 12:
			return buildDate(year, a, b, s) // month first
		case a <= 12 && b <= 12:
			dayFirst, err1 := buildDate(year, b, a, s)
			monthFirst, err2 := buildDate(year, a, b, s)
			if err1 != nil || err2 != nil {
				return "", fmt.Errorf("due date must be YYYY-MM-DD, got %q", s)
			}
			return "", fmt.Errorf(
				"%q is ambiguous: it could be %s or %s. Send the date as YYYY-MM-DD",
				s, dayFirst, monthFirst)
		}
	}
	return "", fmt.Errorf(
		"could not read %q as a date. Send it as YYYY-MM-DD, for example 2026-08-18", s)
}

// buildDate rejects a date that does not exist rather than letting Go roll it
// over into the next month, which would turn 31/02 into the 3rd of March.
func buildDate(year, month, day int, raw string) (string, error) {
	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day {
		return "", fmt.Errorf("%q is not a real date", raw)
	}
	return t.Format(DateLayout), nil
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

// task builds the Task to store, normalising the two fields a model most often
// gets wrong: the date, and a priority written with different capitalisation
// or as a word the enum does not use.
func (in taskInput) task(listID int64) (Task, error) {
	due, err := normaliseToolDate(in.DueDate)
	if err != nil {
		return Task{}, err
	}
	return Task{
		ListID:   listID,
		Title:    in.Title,
		Notes:    in.Notes,
		Priority: normalisePriority(in.Priority),
		DueDate:  due,
	}, nil
}

// normalisePriority maps the words a model reaches for onto the three the
// module stores. Anything unrecognised is passed through for Validate to
// reject by name, which is a better error than a silent "normal".
func normalisePriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "":
		return ""
	case "low", "minor", "someday":
		return PriorityLow
	case "normal", "medium", "med", "standard", "default":
		return PriorityNormal
	case "high", "urgent", "important", "critical", "asap":
		return PriorityHigh
	default:
		return strings.ToLower(strings.TrimSpace(p))
	}
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
			task, err := t.task(list.ID)
			if err != nil {
				return "", fmt.Errorf("created list #%d %q with %d task(s), then failed on %q: %w",
					list.ID, list.Title, created, t.Title, err)
			}
			if _, err := svc.CreateTask(task); err != nil {
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
		wanted, err := in.task(in.ListID)
		if err != nil {
			return "", err
		}
		task, err := svc.CreateTask(wanted)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Added task #%d %q to list #%d%s.",
			task.ID, task.Title, task.ListID, dueSuffix(task.DueDate)), nil
	}
}

// dueSuffix echoes the stored date back in the tool's reply. A model that sent
// "18/8/2026" should see what that became, both so it can tell the user and so
// a wrong reading is visible in the transcript rather than only in the UI.
func dueSuffix(due string) string {
	if due == "" {
		return ""
	}
	return fmt.Sprintf(", due %s", due)
}

// ---- Tasks ----
//
// The three tools above cover creating work. These cover the rest of what the
// Tasks view does — reading it, finishing it, changing it, removing it — which
// the assistant previously could not do at all: it could add a task and then
// had no way to tick it off.
//
// They refuse notes. A note is a task on a reserved list, so an id from
// list_notes will load here; without the guard, set_task_status would quietly
// mark a note done through a path that does not keep a note's shape.
func taskTools(svc *Service) []registration {
	return []registration{
		{
			def: tool.Definition{
				Name: "list_tasks",
				Description: "Read tasks. With list_id, the tasks on that list; without it, tasks " +
					"across every list. Use this before changing or completing a task, to get its id. " +
					"Notes are not included — use list_notes for those.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"list_id": {"type": "integer", "description": "Restrict to one list, from list_todo_lists."},
						"status": {"type": "string", "enum": ["todo", "doing", "done"], "description": "Only tasks in this state."},
						"search": {"type": "string", "description": "Match text in the title or notes."},
						"include_done": {"type": "boolean", "description": "Include finished tasks. Defaults to false."}
					}
				}`),
				Risk: tool.RiskRead,
			},
			handler: listTasksHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "get_agenda",
				Description: "What is outstanding right now, in four groups: overdue, due today, " +
					"due in the next two weeks, and undated. Use this for \"what should I be doing\", " +
					"\"what is late\" or \"what is due this week\" rather than listing every list.",
				Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
				Risk:       tool.RiskRead,
			},
			handler: agendaHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "set_task_status",
				Description: "Mark a task done, in progress, or back to not started. This is the " +
					"checkbox in the UI. Get the id from list_tasks or get_agenda first.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"task_id": {"type": "integer"},
						"status": {"type": "string", "enum": ["todo", "doing", "done"]}
					},
					"required": ["task_id", "status"]
				}`),
				Risk: tool.RiskWrite,
			},
			handler: setTaskStatusHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "update_todo_task",
				Description: "Change a task's title, notes, priority or due date. Only the fields " +
					"you send are changed; the rest are left alone. To clear a due date send an " +
					"empty string. Dates may be written as YYYY-MM-DD.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"task_id": {"type": "integer"},
						"title": {"type": "string"},
						"notes": {"type": "string"},
						"priority": {"type": "string", "enum": ["low", "normal", "high"]},
						"due_date": {"type": "string", "description": "YYYY-MM-DD, or empty to clear."}
					},
					"required": ["task_id"]
				}`),
				Risk: tool.RiskWrite,
			},
			handler: updateTaskHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "delete_todo_task",
				Description: "Delete a task outright. This cannot be undone — to record that " +
					"something is finished while keeping it, use set_task_status with \"done\".",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {"task_id": {"type": "integer"}},
					"required": ["task_id"]
				}`),
				Risk: tool.RiskDestructive,
			},
			handler: deleteTaskHandler(svc),
		},
		{
			def: tool.Definition{
				Name: "update_todo_list",
				Description: "Rename a list, change its description, or archive it. Archiving hides " +
					"a list without deleting anything on it. Only the fields you send are changed.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"list_id": {"type": "integer"},
						"title": {"type": "string"},
						"description": {"type": "string"},
						"archived": {"type": "boolean"}
					},
					"required": ["list_id"]
				}`),
				Risk: tool.RiskWrite,
			},
			handler: updateListHandler(svc),
		},
	}
}

// requireTask loads a task and refuses one that is really a note.
func requireTask(svc *Service, id int64) (Task, error) {
	task, err := svc.GetTask(id)
	if err != nil {
		return Task{}, err
	}
	notes, err := svc.NotesList()
	if err != nil {
		return Task{}, err
	}
	if task.ListID == notes.ID {
		return Task{}, fmt.Errorf(
			"#%d is a note, not a task on a list; use set_note_status or delete_note", id)
	}
	return task, nil
}

func describeTask(t Task, listTitle string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d [%s] %s", t.ID, t.Status, t.Title)
	if t.Priority != "" && t.Priority != PriorityNormal {
		fmt.Fprintf(&b, " (%s)", t.Priority)
	}
	if t.DueDate != "" {
		fmt.Fprintf(&b, " due %s", t.DueDate)
	}
	if listTitle != "" {
		fmt.Fprintf(&b, " — %s", listTitle)
	}
	return b.String()
}

func listTasksHandler(svc *Service) tool.Handler {
	type input struct {
		ListID      int64  `json:"list_id"`
		Status      string `json:"status"`
		Search      string `json:"search"`
		IncludeDone bool   `json:"include_done"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
		}
		// Titles come from the list index rather than a join, and the same
		// call identifies the notes inbox so its rows can be left out.
		lists, err := svc.ListLists(true)
		if err != nil {
			return "", err
		}
		titles := make(map[int64]string, len(lists))
		var notesID int64
		for _, l := range lists {
			titles[l.ID] = l.Title
			if l.Slug != "" {
				notesID = l.ID
			}
		}

		tasks, err := svc.ListTasks(Filter{
			ListID:      in.ListID,
			Status:      strings.ToLower(strings.TrimSpace(in.Status)),
			Search:      in.Search,
			IncludeDone: in.IncludeDone || in.Status == StatusDone,
		})
		if err != nil {
			return "", err
		}

		var b strings.Builder
		shown := 0
		for _, t := range tasks {
			if t.ListID == notesID && in.ListID != notesID {
				continue
			}
			label := ""
			if in.ListID == 0 {
				label = titles[t.ListID]
			}
			b.WriteString(describeTask(t, label))
			b.WriteString("\n")
			shown++
		}
		if shown == 0 {
			return "No tasks match.", nil
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}

func agendaHandler(svc *Service) tool.Handler {
	return func(_ context.Context, _ json.RawMessage) (string, error) {
		ag, err := svc.Agenda()
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Today is %s.\n", ag.Today)
		groups := []struct {
			label string
			tasks []Task
		}{
			{"Overdue", ag.Overdue},
			{"Due today", ag.DueToday},
			{"Upcoming", ag.Upcoming},
			{"No due date", ag.NoDate},
		}
		empty := true
		for _, g := range groups {
			if len(g.tasks) == 0 {
				continue
			}
			empty = false
			fmt.Fprintf(&b, "\n%s:\n", g.label)
			for _, t := range g.tasks {
				fmt.Fprintf(&b, "  %s\n", describeTask(t, t.ListTitle))
			}
		}
		if empty {
			return fmt.Sprintf("Today is %s. Nothing is outstanding.", ag.Today), nil
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}

func setTaskStatusHandler(svc *Service) tool.Handler {
	type input struct {
		TaskID int64  `json:"task_id"`
		Status string `json:"status"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if _, err := requireTask(svc, in.TaskID); err != nil {
			return "", err
		}
		status := strings.ToLower(strings.TrimSpace(in.Status))
		if !contains(Statuses, status) {
			return "", fmt.Errorf("status must be one of %s, got %q",
				strings.Join(Statuses, ", "), in.Status)
		}
		task, err := svc.SetStatus(in.TaskID, status)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Task #%d %q is now %s.", task.ID, task.Title, task.Status), nil
	}
}

func updateTaskHandler(svc *Service) tool.Handler {
	// Pointers so an omitted field and a field set to empty are different
	// things: the first leaves the value alone, the second clears it.
	type input struct {
		TaskID   int64   `json:"task_id"`
		Title    *string `json:"title"`
		Notes    *string `json:"notes"`
		Priority *string `json:"priority"`
		DueDate  *string `json:"due_date"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		task, err := requireTask(svc, in.TaskID)
		if err != nil {
			return "", err
		}
		if in.Title != nil {
			task.Title = *in.Title
		}
		if in.Notes != nil {
			task.Notes = *in.Notes
		}
		if in.Priority != nil {
			task.Priority = normalisePriority(*in.Priority)
		}
		if in.DueDate != nil {
			due, err := normaliseToolDate(*in.DueDate)
			if err != nil {
				return "", err
			}
			task.DueDate = due
		}
		updated, err := svc.UpdateTask(in.TaskID, task)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Updated %s", describeTask(updated, "")), nil
	}
}

func deleteTaskHandler(svc *Service) tool.Handler {
	type input struct {
		TaskID int64 `json:"task_id"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		task, err := requireTask(svc, in.TaskID)
		if err != nil {
			return "", err
		}
		if err := svc.DeleteTask(in.TaskID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted task #%d %q.", task.ID, task.Title), nil
	}
}

func updateListHandler(svc *Service) tool.Handler {
	type input struct {
		ListID      int64   `json:"list_id"`
		Title       *string `json:"title"`
		Description *string `json:"description"`
		Archived    *bool   `json:"archived"`
	}
	return func(_ context.Context, raw json.RawMessage) (string, error) {
		var in input
		if err := json.Unmarshal(raw, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		list, err := svc.GetList(in.ListID)
		if err != nil {
			return "", err
		}
		// The notes inbox is the server's own list; renaming or archiving it
		// would take the notes tools' storage out from under them.
		if list.Slug != "" {
			return "", fmt.Errorf("#%d is the notes inbox and cannot be renamed or archived", in.ListID)
		}
		if in.Title != nil {
			list.Title = *in.Title
		}
		if in.Description != nil {
			list.Description = *in.Description
		}
		if in.Archived != nil {
			list.Archived = *in.Archived
		}
		updated, err := svc.UpdateList(in.ListID, list)
		if err != nil {
			return "", err
		}
		state := ""
		if updated.Archived {
			state = " (archived)"
		}
		return fmt.Sprintf("Updated list #%d %q%s.", updated.ID, updated.Title, state), nil
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
