package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"wintermute/internal/tool"
)

// maxToolTasks bounds how many tasks one tool call may create. A model asked
// for "a list for the audit" occasionally decides that means forty items; the
// cap turns that into a clear error it can read and retry from, rather than
// forty rows somebody has to delete by hand.
const maxToolTasks = 40

// Register exposes the task module to the assistant.
//
// This is what the RCSA application's separate "Assistant" page did, arriving
// here as tools on the agent that already exists rather than as a second one.
// That app needed its own Anthropic client, conversation store and tool
// registry because it had no agent; wintermute has all three, and porting them
// would have meant two transcripts, two loops and two places to look when the
// model does something surprising.
//
// The tool set is short on purpose. Reading and creating a to-do list is
// reversible and touches nothing anyone audits. The rest of what this server
// can reach — the media library, the model backends — is a different decision,
// and should be made deliberately rather than by extending a slice.
//
// The handlers are thin adapters over the same Service methods the HTTP layer
// calls, so a list the model creates goes through the same validation and
// timestamps as one typed into the UI.
func Register(reg *tool.Registry, svc *Service) error {
	tools := []struct {
		def     tool.Definition
		handler tool.Handler
	}{
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
