package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wintermute/internal/store"
)

// The throttle's rules, with an explicit clock so nothing here waits.
func TestTouchTrackerThrottlesPerClient(t *testing.T) {
	tr := newTouchTracker()
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	if !tr.due(1, base) {
		t.Fatal("first sighting of a client should write")
	}
	if tr.due(1, base.Add(time.Second)) {
		t.Error("a second request one second later should not write")
	}
	if tr.due(1, base.Add(touchInterval-time.Millisecond)) {
		t.Error("still inside the interval should not write")
	}
	if !tr.due(1, base.Add(touchInterval)) {
		t.Error("at the interval it should write again")
	}

	// Clients are throttled independently: a chatty browser must not suppress
	// the record of a harness that has just come back.
	if !tr.due(2, base.Add(time.Second)) {
		t.Error("a different client should write on its first sighting")
	}
}

// The throttle must claim the slot under its lock, or a burst of concurrent
// requests all decide they are the one to write and the flush comes back.
func TestTouchTrackerClaimsUnderLock(t *testing.T) {
	tr := newTouchTracker()
	now := time.Now()

	const goroutines = 32
	results := make(chan bool, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			<-start
			results <- tr.due(1, now)
		}()
	}
	close(start)

	wrote := 0
	for i := 0; i < goroutines; i++ {
		if <-results {
			wrote++
		}
	}
	if wrote != 1 {
		t.Errorf("%d of %d concurrent requests decided to write, want exactly 1", wrote, goroutines)
	}
}

// End to end through the real middleware: repeated requests must leave
// last_seen_at alone after the first. This is the behaviour that turned every
// authenticated request into a disk flush.
func TestAuthenticatedRequestsWriteLastSeenOnce(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := t.Context()
	client, token, err := st.CreateClient(ctx, "browser", store.KindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()

	call := func() {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}
	lastSeen := func() *time.Time {
		t.Helper()
		clients, err := st.ListClients(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range clients {
			if c.ID == client.ID {
				return c.LastSeenAt
			}
		}
		t.Fatal("client vanished")
		return nil
	}

	if seen := lastSeen(); seen != nil {
		t.Fatalf("last_seen_at set before any request: %v", seen)
	}

	call()
	first := lastSeen()
	if first == nil {
		t.Fatal("first authenticated request did not record last_seen_at")
	}

	for i := 0; i < 20; i++ {
		call()
	}
	after := lastSeen()
	if after == nil || !after.Equal(*first) {
		t.Errorf("last_seen_at moved during 20 further requests (%v -> %v); the throttle is not holding",
			first, after)
	}
}

// A rejected token must not reach the store at all — it is the fast path, and
// keeping it fast is what stops a bad token costing the same as a good one.
func TestInvalidTokenIsRejectedWithoutTouching(t *testing.T) {
	srv, st := newTestServer(t)
	if _, _, err := st.CreateClient(t.Context(), "browser", store.KindBrowser); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer wm_not_a_real_token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	clients, err := st.ListClients(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		if c.LastSeenAt != nil {
			t.Errorf("client %q was touched by a request that failed authentication", c.Name)
		}
	}
}
