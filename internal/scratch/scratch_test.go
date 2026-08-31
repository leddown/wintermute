package scratch

import (
	"errors"
	"strings"
	"testing"
	"time"

	"wintermute/internal/store/storetest"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	st := storetest.New(t)
	return NewService(st.DB())
}

func mustCreate(t *testing.T, svc *Service, title, body string) Doc {
	t.Helper()
	d, err := svc.Create(Doc{Title: title, Body: body})
	if err != nil {
		t.Fatalf("Create(%q): %v", title, err)
	}
	return d
}

func TestCreateAndGetRoundTrips(t *testing.T) {
	svc := newTestService(t)
	body := "first line\n\nsecond paragraph with a tab\there"
	created := mustCreate(t, svc, "  Notes on the fold  ", body)

	if created.Title != "Notes on the fold" {
		t.Errorf("title = %q, want it trimmed", created.Title)
	}
	got, err := svc.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Body != body {
		t.Errorf("body = %q, want %q", got.Body, body)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Errorf("timestamps not set: %+v", got)
	}
}

// A blank title is named rather than refused. Being made to name a document
// before it can be saved is the friction a scratch pad exists to avoid, and a
// save that fails while someone types is a save that loses what they wrote.
func TestBlankTitleIsNamed(t *testing.T) {
	svc := newTestService(t)
	d := mustCreate(t, svc, "   ", "something")
	if d.Title != UntitledName {
		t.Errorf("title = %q, want %q", d.Title, UntitledName)
	}
}

func TestUpdateReplacesBodyAndMovesTimestamp(t *testing.T) {
	svc := newTestService(t)
	created := mustCreate(t, svc, "Draft", "one")

	updated, err := svc.Update(created.ID, Doc{Title: "Draft", Body: "one\ntwo"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Body != "one\ntwo" {
		t.Errorf("body = %q", updated.Body)
	}
	if updated.CreatedAt != created.CreatedAt {
		t.Errorf("CreatedAt moved: %q -> %q", created.CreatedAt, updated.CreatedAt)
	}
}

// The list feeds a sidebar, so it carries a preview instead of every body —
// otherwise opening the view drags every document in the pad across the wire.
func TestListCarriesPreviewNotBody(t *testing.T) {
	svc := newTestService(t)
	body := "a heading\nand then a long stretch of text " + strings.Repeat("x", 500)
	mustCreate(t, svc, "Long one", body)

	docs, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
	if docs[0].Body != "" {
		t.Errorf("List carried a body of %d bytes; it should carry only a preview", len(docs[0].Body))
	}
	if docs[0].Chars != len([]rune(body)) {
		t.Errorf("chars = %d, want %d", docs[0].Chars, len([]rune(body)))
	}
	if strings.ContainsAny(docs[0].Preview, "\n\r") {
		t.Errorf("preview has a line break in it: %q", docs[0].Preview)
	}
	if !strings.HasPrefix(docs[0].Preview, "a heading and then") {
		t.Errorf("preview = %q", docs[0].Preview)
	}
}

// Most recently changed first: the pad is worked in, not filed in, and the
// document being edited has to stay at the top of the list.
func TestListIsNewestChangeFirst(t *testing.T) {
	svc := newTestService(t)
	tick := 0
	svc.now = func() time.Time {
		tick++
		return time.Date(2026, 8, 27, 12, 0, tick, 0, time.UTC)
	}

	first := mustCreate(t, svc, "First", "")
	second := mustCreate(t, svc, "Second", "")
	if _, err := svc.Update(first.ID, Doc{Title: "First", Body: "edited"}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	docs, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) != 2 || docs[0].ID != first.ID || docs[1].ID != second.ID {
		t.Fatalf("order = %+v, want the edited document first", docs)
	}
}

func TestMissingDocumentIsNotFound(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.Get(404); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.Update(404, Doc{Title: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update: err = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(404); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete: err = %v, want ErrNotFound", err)
	}
}

// The cap is there so a runaway paste cannot grow the database every other
// module shares. It has to be refused rather than truncated: silently keeping
// half of what someone pasted is worse than saying it did not fit.
func TestOversizedBodyIsRefused(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.Create(Doc{Title: "Huge", Body: strings.Repeat("x", MaxBodyLen+1)})
	if err == nil {
		t.Fatal("Create accepted a body over the cap")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a validation failure", err)
	}
}
