package todo

import (
	"strings"
	"testing"
)

// Deleting an ordinary list takes its tasks with it.
func TestDeleteListRemovesItsTasks(t *testing.T) {
	svc := newTestService(t)
	list, err := svc.CreateList(List{Title: "Home"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateTask(Task{ListID: list.ID, Title: "Fix car"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteList(list.ID); err != nil {
		t.Fatalf("DeleteList: %v", err)
	}
	left, err := svc.ListTasks(Filter{ListID: list.ID, IncludeDone: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d task(s) survived the list", len(left))
	}
	lists, err := svc.ListLists(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lists {
		if l.ID == list.ID {
			t.Error("the list itself survived")
		}
	}
}

// The notes inbox is the server's own list: deleting it would destroy every
// note and leave the note paths quietly recreating an empty one, which reads
// as the notes having vanished rather than as a deletion.
func TestDeleteListRefusesTheNotesInbox(t *testing.T) {
	svc := newTestService(t)
	note := mustNote(t, svc, "ring the garage", "")
	inbox, err := svc.NotesList()
	if err != nil {
		t.Fatal(err)
	}

	err = svc.DeleteList(inbox.ID)
	if err == nil {
		t.Fatal("deleting the notes inbox was allowed")
	}
	if !strings.Contains(err.Error(), "notes inbox") {
		t.Errorf("error should say why, got: %v", err)
	}

	// The note is still there, which is the point of the guard.
	notes, err := svc.ListNotes()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].ID != note.ID {
		t.Errorf("the note did not survive the refused delete: %+v", notes)
	}
}

func TestDeleteListMissing(t *testing.T) {
	if err := newTestService(t).DeleteList(4242); err == nil {
		t.Error("deleting a list that does not exist should fail")
	}
}
