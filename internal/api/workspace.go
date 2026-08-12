package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"wintermute/internal/accounting"
	"wintermute/internal/company"
	"wintermute/internal/crm"
	"wintermute/internal/todo"
)

// This file exposes the workspace modules — company profile, CRM, tasks — that
// moved here from the RCSA application.
//
// They sit behind the same bearer-token middleware as everything else. That app
// gated its write routes behind a separate admin token on top of a login;
// wintermute has one credential and one operator, so a second tier would be a
// second copy of the same token with a different name.

// Workspace groups the ported services. It is a struct rather than three more
// arguments to New because these arrive and depart together, and a constructor
// with eight positional dependencies is one that gets called wrongly.
type Workspace struct {
	Company    *company.Service
	CRM        *crm.Service
	Todo       *todo.Service
	Accounting *accounting.Service
}

func (s *Server) registerWorkspaceRoutes(authed func(string, http.HandlerFunc)) {
	if s.workspace.Company != nil {
		authed("GET /api/v1/company", s.handleGetCompany)
		authed("PUT /api/v1/company", s.handleSaveCompany)
		authed("DELETE /api/v1/company", s.handleClearCompany)
	}
	if s.workspace.CRM != nil {
		authed("GET /api/v1/crm/dashboard", s.handleCRMDashboard)
		authed("GET /api/v1/crm/billing", s.handleCRMBilling)

		authed("GET /api/v1/crm/clients", s.handleListClients)
		authed("POST /api/v1/crm/clients", s.handleCreateClient)
		authed("GET /api/v1/crm/clients/{id}", s.handleGetClient)
		authed("PUT /api/v1/crm/clients/{id}", s.handleUpdateClient)
		authed("DELETE /api/v1/crm/clients/{id}", s.handleDeleteClient)

		authed("GET /api/v1/crm/engagements", s.handleListEngagements)
		authed("POST /api/v1/crm/engagements", s.handleCreateEngagement)
		authed("PUT /api/v1/crm/engagements/{id}", s.handleUpdateEngagement)
		authed("DELETE /api/v1/crm/engagements/{id}", s.handleDeleteEngagement)
		authed("POST /api/v1/crm/engagements/{id}/invoice", s.handleInvoiceEngagement)

		authed("GET /api/v1/crm/time", s.handleListTimeEntries)
		authed("POST /api/v1/crm/time", s.handleCreateTimeEntry)
		authed("PUT /api/v1/crm/time/{id}", s.handleUpdateTimeEntry)
		authed("DELETE /api/v1/crm/time/{id}", s.handleDeleteTimeEntry)
		authed("POST /api/v1/crm/time/{id}/invoiced", s.handleSetTimeEntryInvoiced)
	}
	if s.workspace.Todo != nil {
		authed("GET /api/v1/todo/lists", s.handleListTodoLists)
		authed("POST /api/v1/todo/lists", s.handleCreateTodoList)
		authed("PUT /api/v1/todo/lists/{id}", s.handleUpdateTodoList)
		authed("DELETE /api/v1/todo/lists/{id}", s.handleDeleteTodoList)

		authed("GET /api/v1/todo/tasks", s.handleListTodoTasks)
		authed("POST /api/v1/todo/tasks", s.handleCreateTodoTask)
		authed("PUT /api/v1/todo/tasks/{id}", s.handleUpdateTodoTask)
		authed("DELETE /api/v1/todo/tasks/{id}", s.handleDeleteTodoTask)
		authed("POST /api/v1/todo/tasks/{id}/status", s.handleSetTodoTaskStatus)

		authed("GET /api/v1/todo/agenda", s.handleTodoAgenda)
		authed("GET /api/v1/todo/calendar", s.handleTodoCalendar)
	}
	s.registerAccountingRoutes(authed)
}

// ---- company ----

func (s *Server) handleGetCompany(w http.ResponseWriter, r *http.Request) {
	profile, err := s.workspace.Company.Profile()
	if err != nil {
		s.fail(w, "load company profile", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": profile, "complete": profile.Complete()})
}

func (s *Server) handleSaveCompany(w http.ResponseWriter, r *http.Request) {
	var profile company.Profile
	if !decode(w, r, &profile) {
		return
	}
	saved, err := s.workspace.Company.Save(profile, clientFrom(r.Context()).Name)
	if err != nil {
		// Every error out of Save is a validation failure with a message
		// written for the person who typed the value.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": saved, "complete": saved.Complete()})
}

func (s *Server) handleClearCompany(w http.ResponseWriter, r *http.Request) {
	if err := s.workspace.Company.Clear(); err != nil {
		s.fail(w, "clear company profile", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- CRM ----

func (s *Server) handleCRMDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := s.workspace.CRM.Dashboard()
	if err != nil {
		s.fail(w, "crm dashboard", err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleCRMBilling(w http.ResponseWriter, r *http.Request) {
	lines, err := s.workspace.CRM.Billing()
	if err != nil {
		s.fail(w, "crm billing", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (s *Server) handleListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := s.workspace.CRM.ListClients(r.URL.Query().Get("search"), r.URL.Query().Get("status"))
	if err != nil {
		s.fail(w, "list clients", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": clients})
}

func (s *Server) handleGetClient(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	client, err := s.workspace.CRM.GetClient(id)
	if err != nil {
		s.crmError(w, "get client", err)
		return
	}
	writeJSON(w, http.StatusOK, client)
}

func (s *Server) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	var in crm.Client
	if !decode(w, r, &in) {
		return
	}
	created, err := s.workspace.CRM.CreateClient(in)
	if err != nil {
		s.crmError(w, "create client", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in crm.Client
	if !decode(w, r, &in) {
		return
	}
	updated, err := s.workspace.CRM.UpdateClient(id, in)
	if err != nil {
		s.crmError(w, "update client", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.workspace.CRM.DeleteClient(id); err != nil {
		s.crmError(w, "delete client", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListEngagements(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID, _ := strconv.ParseInt(q.Get("client_id"), 10, 64)
	list, err := s.workspace.CRM.ListEngagements(clientID, q.Get("search"), q.Get("status"))
	if err != nil {
		s.fail(w, "list engagements", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"engagements": list})
}

func (s *Server) handleCreateEngagement(w http.ResponseWriter, r *http.Request) {
	var in crm.Engagement
	if !decode(w, r, &in) {
		return
	}
	created, err := s.workspace.CRM.CreateEngagement(in)
	if err != nil {
		s.crmError(w, "create engagement", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateEngagement(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in crm.Engagement
	if !decode(w, r, &in) {
		return
	}
	updated, err := s.workspace.CRM.UpdateEngagement(id, in)
	if err != nil {
		s.crmError(w, "update engagement", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteEngagement(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.workspace.CRM.DeleteEngagement(id); err != nil {
		s.crmError(w, "delete engagement", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleInvoiceEngagement(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	n, err := s.workspace.CRM.MarkEngagementInvoiced(id)
	if err != nil {
		s.crmError(w, "invoice engagement", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invoiced": n})
}

func (s *Server) handleListTimeEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	engagementID, _ := strconv.ParseInt(q.Get("engagement_id"), 10, 64)
	clientID, _ := strconv.ParseInt(q.Get("client_id"), 10, 64)
	entries, err := s.workspace.CRM.ListTimeEntries(crm.TimeEntryFilter{
		EngagementID: engagementID,
		ClientID:     clientID,
		Billable:     q.Get("billable"),
		Invoiced:     q.Get("invoiced"),
		Search:       q.Get("search"),
	})
	if err != nil {
		s.fail(w, "list time entries", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleCreateTimeEntry(w http.ResponseWriter, r *http.Request) {
	var in crm.TimeEntry
	if !decode(w, r, &in) {
		return
	}
	created, err := s.workspace.CRM.CreateTimeEntry(in)
	if err != nil {
		s.crmError(w, "create time entry", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateTimeEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in crm.TimeEntry
	if !decode(w, r, &in) {
		return
	}
	updated, err := s.workspace.CRM.UpdateTimeEntry(id, in)
	if err != nil {
		s.crmError(w, "update time entry", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteTimeEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.workspace.CRM.DeleteTimeEntry(id); err != nil {
		s.crmError(w, "delete time entry", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSetTimeEntryInvoiced(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Invoiced bool `json:"invoiced"`
	}
	if !decode(w, r, &req) {
		return
	}
	updated, err := s.workspace.CRM.SetTimeEntryInvoiced(id, req.Invoiced)
	if err != nil {
		s.crmError(w, "set time entry invoiced", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ---- tasks ----

func (s *Server) handleListTodoLists(w http.ResponseWriter, r *http.Request) {
	lists, err := s.workspace.Todo.ListLists(r.URL.Query().Get("archived") == "1")
	if err != nil {
		s.fail(w, "list todo lists", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lists": lists})
}

func (s *Server) handleCreateTodoList(w http.ResponseWriter, r *http.Request) {
	var in todo.List
	if !decode(w, r, &in) {
		return
	}
	created, err := s.workspace.Todo.CreateList(in)
	if err != nil {
		s.todoError(w, "create todo list", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateTodoList(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in todo.List
	if !decode(w, r, &in) {
		return
	}
	updated, err := s.workspace.Todo.UpdateList(id, in)
	if err != nil {
		s.todoError(w, "update todo list", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteTodoList(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.workspace.Todo.DeleteList(id); err != nil {
		s.todoError(w, "delete todo list", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListTodoTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	listID, _ := strconv.ParseInt(q.Get("list_id"), 10, 64)
	tasks, err := s.workspace.Todo.ListTasks(todo.Filter{
		ListID:      listID,
		Status:      q.Get("status"),
		Search:      q.Get("search"),
		DueFrom:     q.Get("due_from"),
		DueTo:       q.Get("due_to"),
		DueOnly:     q.Get("due_only") == "1",
		IncludeDone: q.Get("include_done") == "1",
	})
	if err != nil {
		s.fail(w, "list tasks", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "today": s.workspace.Todo.Today()})
}

func (s *Server) handleCreateTodoTask(w http.ResponseWriter, r *http.Request) {
	var in todo.Task
	if !decode(w, r, &in) {
		return
	}
	created, err := s.workspace.Todo.CreateTask(in)
	if err != nil {
		s.todoError(w, "create task", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateTodoTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in todo.Task
	if !decode(w, r, &in) {
		return
	}
	updated, err := s.workspace.Todo.UpdateTask(id, in)
	if err != nil {
		s.todoError(w, "update task", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteTodoTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.workspace.Todo.DeleteTask(id); err != nil {
		s.todoError(w, "delete task", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSetTodoTaskStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}
	updated, err := s.workspace.Todo.SetStatus(id, strings.TrimSpace(req.Status))
	if err != nil {
		s.todoError(w, "set task status", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleTodoAgenda(w http.ResponseWriter, r *http.Request) {
	agenda, err := s.workspace.Todo.Agenda()
	if err != nil {
		s.fail(w, "todo agenda", err)
		return
	}
	writeJSON(w, http.StatusOK, agenda)
}

func (s *Server) handleTodoCalendar(w http.ResponseWriter, r *http.Request) {
	month, err := s.workspace.Todo.Calendar(r.URL.Query().Get("month"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, month)
}

// ---- shared ----

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

// crmError maps the CRM service's two error kinds. Anything else is a fault:
// reporting a storage failure as a bad request tells the caller to fix
// something they did not do wrong.
func (s *Server) crmError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, crm.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case crm.IsValidation(err):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.fail(w, op, err)
	}
}

// todoError maps the task module's errors. Its validation failures are plain
// errors rather than a typed kind, so a not-found is distinguished and
// everything else is treated as the caller's to fix — the service validates
// before it touches the database, which is where its other errors come from.
func (s *Server) todoError(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, todo.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}
