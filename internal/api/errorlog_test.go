package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The ring must not grow, and must keep the newest rather than the first.
func TestErrorLogRingDropsOldest(t *testing.T) {
	l := newErrorLog()
	for i := 0; i < errorRingSize+10; i++ {
		l.record(ServerError{Op: "op", Err: string(rune('a' + i%26))})
	}
	got := l.list(0)
	if len(got) != errorRingSize {
		t.Fatalf("kept %d entries, want the ring size %d", len(got), errorRingSize)
	}
	// Newest first, and the newest is the last one recorded.
	if got[0].ID != int64(errorRingSize+10) {
		t.Errorf("newest id = %d, want %d", got[0].ID, errorRingSize+10)
	}
	if got[len(got)-1].ID != 11 {
		t.Errorf("oldest kept id = %d, want the ring to have dropped everything before 11",
			got[len(got)-1].ID)
	}
}

func TestErrorLogLimit(t *testing.T) {
	l := newErrorLog()
	for i := 0; i < 5; i++ {
		l.record(ServerError{Op: "op"})
	}
	if got := l.list(2); len(got) != 2 {
		t.Fatalf("limit 2 returned %d", len(got))
	}
	if got := l.list(99); len(got) != 5 {
		t.Fatalf("a limit beyond what is held returned %d, want 5", len(got))
	}
}

// fail() must keep answering opaquely while recording the detail — the whole
// point is that the response does not change.
func TestFailStaysOpaqueButIsRecorded(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.fail(rec, "do the thing", errors.New("the real reason: permission denied"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "internal error") ||
		strings.Contains(body, "permission denied") {
		t.Fatalf("the response must stay generic, got %s", body)
	}

	got := srv.errors.list(1)
	if len(got) != 1 {
		t.Fatalf("the failure should have been recorded, got %d entries", len(got))
	}
	if got[0].Op != "do the thing" || !strings.Contains(got[0].Err, "permission denied") {
		t.Errorf("recorded = %+v", got[0])
	}
	if got[0].At.IsZero() {
		t.Error("a recorded failure needs a timestamp to be worth reading")
	}
}
