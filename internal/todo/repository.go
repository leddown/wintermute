package todo

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is returned when a list or task does not exist.
var ErrNotFound = errors.New("not found")

// Repository is the persistence boundary.
//
// In the application this came from, every method took an owner string and
// every query carried an `owner = ?` clause, because that app had signed-in
// users who must not see each other's tasks. Wintermute has no user accounts —
// the boundary is the client token, checked once at the API edge — so the
// parameter would be a constant empty string threaded through twenty
// signatures. It is gone rather than stubbed: a scoping argument nothing scopes
// on reads as a guarantee that is not being made.
type Repository interface {
	ListLists(includeArchived bool) ([]List, error)
	GetList(id int64) (List, error)
	ListBySlug(slug string) (List, error)
	CreateList(l List) (List, error)
	UpdateList(id int64, l List) (List, error)
	DeleteList(id int64) error

	ListTasks(f Filter) ([]Task, error)
	GetTask(id int64) (Task, error)
	CreateTask(t Task) (Task, error)
	UpdateTask(id int64, t Task) (Task, error)
	DeleteTask(id int64) error
	NextOrdinal(listID int64) (int, error)

	ListEvents(from, to string) ([]Event, error)
	GetEvent(id int64) (Event, error)
	CreateEvent(e Event) (Event, error)
	DeleteEvent(id int64) error
}

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

// insert runs an INSERT and returns the generated id.
func (r *SQLiteRepository) insert(query string, args ...any) (int64, error) {
	res, err := r.db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ---- Lists ----

// listSelect is shared by the three list reads. The counts are a correlated
// subquery rather than a second round trip: the index shows progress per list,
// and doing it per row turned one page load into N+1 queries.
const listSelect = `
	SELECT l.id, l.title, l.description, l.archived, l.slug, l.created_at, l.updated_at,
	       (SELECT COUNT(*) FROM todo_tasks t WHERE t.list_id = l.id),
	       (SELECT COUNT(*) FROM todo_tasks t WHERE t.list_id = l.id AND t.status = 'done')
	FROM todo_lists l`

// scanList reads one row of listSelect. rows is either *sql.Rows or *sql.Row.
func scanList(row interface{ Scan(...any) error }) (List, error) {
	var l List
	var archived int
	err := row.Scan(&l.ID, &l.Title, &l.Description, &archived, &l.Slug,
		&l.CreatedAt, &l.UpdatedAt, &l.TaskCount, &l.DoneCount)
	if err != nil {
		return List{}, err
	}
	l.Archived = archived != 0
	return l, nil
}

func (r *SQLiteRepository) ListLists(includeArchived bool) ([]List, error) {
	query := listSelect
	if !includeArchived {
		query += ` WHERE l.archived = 0`
	}
	query += ` ORDER BY l.archived, l.updated_at DESC, l.id DESC`

	rows, err := r.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("listing todo lists: %w", err)
	}
	defer rows.Close()

	out := make([]List, 0)
	for rows.Next() {
		l, err := scanList(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning todo list: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) GetList(id int64) (List, error) {
	l, err := scanList(r.db.QueryRow(listSelect+` WHERE l.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return List{}, ErrNotFound
	}
	if err != nil {
		return List{}, fmt.Errorf("loading todo list: %w", err)
	}
	return l, nil
}

// ListBySlug finds one of the lists the server keeps for itself — today only
// the notes inbox. An empty slug matches nothing on purpose: every list a
// person made carries one, and this must not return an arbitrary one of them.
func (r *SQLiteRepository) ListBySlug(slug string) (List, error) {
	if slug == "" {
		return List{}, ErrNotFound
	}
	l, err := scanList(r.db.QueryRow(listSelect+` WHERE l.slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return List{}, ErrNotFound
	}
	if err != nil {
		return List{}, fmt.Errorf("loading todo list by slug: %w", err)
	}
	return l, nil
}

func (r *SQLiteRepository) CreateList(l List) (List, error) {
	id, err := r.insert(`
		INSERT INTO todo_lists (title, description, archived, slug, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		l.Title, l.Description, boolToInt(l.Archived), l.Slug, l.CreatedAt, l.UpdatedAt)
	if err != nil {
		return List{}, fmt.Errorf("creating todo list: %w", err)
	}
	l.ID = id
	return l, nil
}

func (r *SQLiteRepository) UpdateList(id int64, l List) (List, error) {
	res, err := r.db.Exec(`
		UPDATE todo_lists SET title = ?, description = ?, archived = ?, updated_at = ?
		WHERE id = ?`,
		l.Title, l.Description, boolToInt(l.Archived), l.UpdatedAt, id)
	if err != nil {
		return List{}, fmt.Errorf("updating todo list: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return List{}, ErrNotFound
	}
	return r.GetList(id)
}

func (r *SQLiteRepository) DeleteList(id int64) error {
	// Tasks go with the list. The store opens SQLite with foreign_keys=ON so
	// the cascade would cover it, but the child delete is explicit rather than
	// trusting a pragma set in another package.
	if _, err := r.db.Exec(`DELETE FROM todo_tasks WHERE list_id = ?`, id); err != nil {
		return fmt.Errorf("deleting tasks for list: %w", err)
	}
	res, err := r.db.Exec(`DELETE FROM todo_lists WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting todo list: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- Tasks ----

func (r *SQLiteRepository) ListTasks(f Filter) ([]Task, error) {
	var (
		clauses []string
		args    []any
	)

	if f.ListID > 0 {
		clauses = append(clauses, "t.list_id = ?")
		args = append(args, f.ListID)
	}
	if f.Status != "" {
		clauses = append(clauses, "t.status = ?")
		args = append(args, f.Status)
	}
	if !f.IncludeDone && f.Status == "" {
		clauses = append(clauses, "t.status <> 'done'")
	}
	if f.DueOnly {
		clauses = append(clauses, "t.due_date <> ''")
	}
	if f.DueFrom != "" {
		clauses = append(clauses, "t.due_date >= ?")
		args = append(args, f.DueFrom)
	}
	if f.DueTo != "" {
		clauses = append(clauses, "t.due_date <= ?")
		args = append(args, f.DueTo)
	}
	if f.Search != "" {
		clauses = append(clauses, "(LOWER(t.title) LIKE ? OR LOWER(t.notes) LIKE ?)")
		pattern := "%" + strings.ToLower(f.Search) + "%"
		args = append(args, pattern, pattern)
	}

	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}

	// #nosec G202 -- every fragment in clauses is a constant string defined
	// above; user input reaches the query only through the ? placeholders.
	query := `
		SELECT t.id, t.list_id, t.title, t.notes, t.status, t.priority, t.due_date,
		       t.ordinal, t.created_at, t.updated_at, t.completed_at, l.title
		FROM todo_tasks t
		JOIN todo_lists l ON l.id = t.list_id` + where + `
		ORDER BY (t.due_date = '') , t.due_date, t.ordinal, t.id`

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	defer rows.Close()

	out := make([]Task, 0)
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.ListID, &t.Title, &t.Notes, &t.Status, &t.Priority,
			&t.DueDate, &t.Ordinal, &t.CreatedAt, &t.UpdatedAt, &t.CompletedAt, &t.ListTitle); err != nil {
			return nil, fmt.Errorf("scanning task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) GetTask(id int64) (Task, error) {
	var t Task
	err := r.db.QueryRow(`
		SELECT t.id, t.list_id, t.title, t.notes, t.status, t.priority, t.due_date,
		       t.ordinal, t.created_at, t.updated_at, t.completed_at, l.title
		FROM todo_tasks t JOIN todo_lists l ON l.id = t.list_id
		WHERE t.id = ?`, id).
		Scan(&t.ID, &t.ListID, &t.Title, &t.Notes, &t.Status, &t.Priority, &t.DueDate,
			&t.Ordinal, &t.CreatedAt, &t.UpdatedAt, &t.CompletedAt, &t.ListTitle)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("loading task: %w", err)
	}
	return t, nil
}

// CreateTask inserts a task after confirming the list exists. The check stays
// here rather than in the service because it is what makes the insert safe: a
// caller passing an unknown list_id would otherwise create a task no list query
// can ever return.
func (r *SQLiteRepository) CreateTask(t Task) (Task, error) {
	if _, err := r.GetList(t.ListID); err != nil {
		return Task{}, err
	}
	id, err := r.insert(`
		INSERT INTO todo_tasks (list_id, title, notes, status, priority, due_date, ordinal,
		                        created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ListID, t.Title, t.Notes, t.Status, t.Priority, t.DueDate, t.Ordinal,
		t.CreatedAt, t.UpdatedAt, t.CompletedAt)
	if err != nil {
		return Task{}, fmt.Errorf("creating task: %w", err)
	}
	return r.GetTask(id)
}

func (r *SQLiteRepository) UpdateTask(id int64, t Task) (Task, error) {
	res, err := r.db.Exec(`
		UPDATE todo_tasks SET title = ?, notes = ?, status = ?, priority = ?, due_date = ?,
		                      ordinal = ?, updated_at = ?, completed_at = ?
		WHERE id = ?`,
		t.Title, t.Notes, t.Status, t.Priority, t.DueDate,
		t.Ordinal, t.UpdatedAt, t.CompletedAt, id)
	if err != nil {
		return Task{}, fmt.Errorf("updating task: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return Task{}, ErrNotFound
	}
	return r.GetTask(id)
}

func (r *SQLiteRepository) DeleteTask(id int64) error {
	res, err := r.db.Exec(`DELETE FROM todo_tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting task: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *SQLiteRepository) NextOrdinal(listID int64) (int, error) {
	var next sql.NullInt64
	if err := r.db.QueryRow(
		`SELECT MAX(ordinal) FROM todo_tasks WHERE list_id = ?`, listID).Scan(&next); err != nil {
		return 0, fmt.Errorf("finding next ordinal: %w", err)
	}
	return int(next.Int64) + 1, nil
}

// ---- Events ----

const eventSelect = `
	SELECT id, title, description, start_at, end_at, all_day, created_at, updated_at
	FROM todo_events`

func scanEvent(row interface{ Scan(...any) error }) (Event, error) {
	var e Event
	var allDay int
	err := row.Scan(&e.ID, &e.Title, &e.Description, &e.StartAt, &e.EndAt, &allDay,
		&e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return Event{}, err
	}
	e.AllDay = allDay != 0
	e.Date = day(e.StartAt)
	return e, nil
}

// ListEvents returns the events starting in the half-open window [from, to),
// as "YYYY-MM-DD" bounds. Empty bounds are unbounded on that side.
//
// The comparison is against the stored text, which is why ParseMoment
// normalises timestamps to UTC: an all-day "2026-07-20" and a timed
// "2026-07-20T09:00:00Z" both fall inside a July window under the same string
// comparison, because RFC3339 leads with the date.
func (r *SQLiteRepository) ListEvents(from, to string) ([]Event, error) {
	var (
		clauses []string
		args    []any
	)
	if from != "" {
		clauses = append(clauses, "start_at >= ?")
		args = append(args, from)
	}
	if to != "" {
		clauses = append(clauses, "start_at < ?")
		args = append(args, to)
	}
	query := eventSelect
	if len(clauses) > 0 {
		// #nosec G202 -- the fragments are constants defined above; the bounds
		// reach the query only through the ? placeholders.
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += ` ORDER BY start_at, id`

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()

	out := make([]Event, 0)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) GetEvent(id int64) (Event, error) {
	e, err := scanEvent(r.db.QueryRow(eventSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("loading event: %w", err)
	}
	return e, nil
}

func (r *SQLiteRepository) CreateEvent(e Event) (Event, error) {
	id, err := r.insert(`
		INSERT INTO todo_events (title, description, start_at, end_at, all_day, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.Title, e.Description, e.StartAt, e.EndAt, boolToInt(e.AllDay), e.CreatedAt, e.UpdatedAt)
	if err != nil {
		return Event{}, fmt.Errorf("creating event: %w", err)
	}
	return r.GetEvent(id)
}

func (r *SQLiteRepository) DeleteEvent(id int64) error {
	res, err := r.db.Exec(`DELETE FROM todo_events WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting event: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// boolToInt stores flags as INTEGER, matching the rest of the schema.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
