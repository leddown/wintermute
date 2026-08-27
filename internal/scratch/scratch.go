// Package scratch is the scratch pad: named documents of free text.
//
// It is deliberately thin. A pad is a title and a body, and the only thing the
// server does with the body is store it and hand it back — no rendering, no
// format, no structure to keep valid. That is the point of it: somewhere to
// drop what a turn produced and work it over without deciding first what kind
// of thing it is.
//
// It sits next to the task module rather than inside it because the shapes do
// not meet. A note is a task on a reserved list — one line, with a status and
// possibly a date — and stretching it to hold pages of prose would give every
// note a status nobody sets and a length no list view can show.
//
// There is no interface over the storage here. The task and CRM modules have
// one because their tests substitute a fake; this is one table and five
// statements, and an interface would be a name for something with a single
// implementation.
package scratch

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrNotFound is returned when a document does not exist.
var ErrNotFound = errors.New("not found")

const (
	maxTitleLen = 200
	// A pad holds what a model produced, which can be long. The cap is well
	// under the API's 8 MiB body limit and exists so a runaway paste cannot
	// quietly grow the memory database that everything else shares.
	MaxBodyLen = 1 << 20
	// UntitledName is what a pad saved with a blank title is called. Naming it
	// here rather than refusing the save keeps the editor out of the way: the
	// text is the thing being kept, and being made to name it first is exactly
	// the friction a scratch pad exists to avoid.
	UntitledName = "Untitled"
	// previewLen is how much of the body the list carries so the sidebar can
	// show what a pad is without fetching every body.
	previewLen = 120
)

// Doc is one pad.
type Doc struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	// Body is empty on documents returned by List, which does not read the
	// bodies — see Preview.
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`

	// Preview and Chars are filled by List only, so the sidebar can say what a
	// pad holds and how much of it without carrying every body to the browser.
	Preview string `json:"preview,omitempty"`
	Chars   int    `json:"chars,omitempty"`
}

// Validate normalises a document and reports the first problem.
func (d *Doc) Validate() error {
	d.Title = strings.TrimSpace(d.Title)
	if d.Title == "" {
		d.Title = UntitledName
	}
	if utf8.RuneCountInString(d.Title) > maxTitleLen {
		return fmt.Errorf("title is longer than %d characters", maxTitleLen)
	}
	if len(d.Body) > MaxBodyLen {
		return fmt.Errorf("document is larger than %d bytes", MaxBodyLen)
	}
	return nil
}

// Service holds the pad rules and its storage.
type Service struct {
	db  *sql.DB
	now func() time.Time
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

func (s *Service) timestamp() string { return s.now().UTC().Format(time.RFC3339) }

// List returns every document, most recently changed first, without bodies.
func (s *Service) List() ([]Doc, error) {
	rows, err := s.db.Query(`
		SELECT id, title, created_at, updated_at,
		       substr(body, 1, ?), length(body)
		FROM scratch_docs
		ORDER BY updated_at DESC, id DESC`, previewLen)
	if err != nil {
		return nil, fmt.Errorf("listing scratch documents: %w", err)
	}
	defer rows.Close()

	docs := []Doc{}
	for rows.Next() {
		var d Doc
		if err := rows.Scan(&d.ID, &d.Title, &d.CreatedAt, &d.UpdatedAt, &d.Preview, &d.Chars); err != nil {
			return nil, fmt.Errorf("scanning scratch document: %w", err)
		}
		// Newlines in a preview become a one-line sidebar row of their own
		// otherwise, which is how a two-paragraph pad ends up three rows tall.
		d.Preview = strings.Join(strings.Fields(d.Preview), " ")
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// Get returns one document with its body.
func (s *Service) Get(id int64) (Doc, error) {
	var d Doc
	err := s.db.QueryRow(`
		SELECT id, title, body, created_at, updated_at
		FROM scratch_docs WHERE id = ?`, id).
		Scan(&d.ID, &d.Title, &d.Body, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Doc{}, ErrNotFound
	}
	if err != nil {
		return Doc{}, fmt.Errorf("reading scratch document: %w", err)
	}
	return d, nil
}

// Create stores a new document.
func (s *Service) Create(d Doc) (Doc, error) {
	if err := d.Validate(); err != nil {
		return Doc{}, err
	}
	d.CreatedAt = s.timestamp()
	d.UpdatedAt = d.CreatedAt
	res, err := s.db.Exec(`
		INSERT INTO scratch_docs (title, body, created_at, updated_at)
		VALUES (?, ?, ?, ?)`, d.Title, d.Body, d.CreatedAt, d.UpdatedAt)
	if err != nil {
		return Doc{}, fmt.Errorf("creating scratch document: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Doc{}, fmt.Errorf("creating scratch document: %w", err)
	}
	d.ID = id
	return d, nil
}

// Update replaces a document's title and body.
//
// The whole body is written every time rather than a diff. The editor saves on
// a timer while someone types, so the write is frequent, but it is one row of
// text and a patch protocol here would be a second thing to get wrong on the
// path that holds the only copy of what they wrote.
func (s *Service) Update(id int64, d Doc) (Doc, error) {
	if err := d.Validate(); err != nil {
		return Doc{}, err
	}
	d.UpdatedAt = s.timestamp()
	res, err := s.db.Exec(`
		UPDATE scratch_docs SET title = ?, body = ?, updated_at = ?
		WHERE id = ?`, d.Title, d.Body, d.UpdatedAt, id)
	if err != nil {
		return Doc{}, fmt.Errorf("updating scratch document: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return Doc{}, ErrNotFound
	}
	return s.Get(id)
}

// Delete removes a document. There is no undo, which is why the browser asks
// first.
func (s *Service) Delete(id int64) error {
	res, err := s.db.Exec(`DELETE FROM scratch_docs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting scratch document: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}
