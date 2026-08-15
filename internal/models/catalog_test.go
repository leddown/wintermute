package models

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"wintermute/internal/store"
)

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewCatalog(nil, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A recorded "ok" describes the moment it was written. Once the prober has
// stopped confirming it, the health endpoint must stop repeating it — that is
// the difference between the UI admitting it has lost touch with a machine and
// it reporting a powered-off host as healthy.
func TestBackendHealthStaleOKReadsUnknown(t *testing.T) {
	c := testCatalog(t)
	ctx := context.Background()

	old := time.Now().UTC().Add(-time.Hour)
	if err := c.store.UpsertBackend(ctx, store.BackendRow{
		Name:     "workshop",
		Kind:     "ollama",
		BaseURL:  "http://workshop:11434",
		Status:   store.BackendOK,
		ProbedAt: &old,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Without a staleness horizon (no background prober), the stored verdict
	// stands: manual probing means the row is explicitly a last-known value.
	rows, err := c.BackendHealth(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != store.BackendOK {
		t.Fatalf("want stored ok with no horizon, got %+v", rows)
	}

	c.mu.Lock()
	c.staleAfter = 3 * time.Minute
	c.mu.Unlock()

	rows, err = c.BackendHealth(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if rows[0].Status != store.BackendUnknown {
		t.Fatalf("stale probe: want %q, got %q", store.BackendUnknown, rows[0].Status)
	}
	if rows[0].StatusNote == "" {
		t.Fatal("stale probe: want a note saying why the status is unknown")
	}
}

// A probe from within the horizon is still current, and must be reported as
// taken rather than blanked.
func TestBackendHealthFreshProbeSurvives(t *testing.T) {
	c := testCatalog(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := c.store.UpsertBackend(ctx, store.BackendRow{
		Name:     "workshop",
		Kind:     "ollama",
		Status:   store.BackendUnreachable,
		ProbedAt: &now,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	c.mu.Lock()
	c.staleAfter = 3 * time.Minute
	c.mu.Unlock()

	rows, err := c.BackendHealth(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if rows[0].Status != store.BackendUnreachable {
		t.Fatalf("fresh probe: want %q, got %q", store.BackendUnreachable, rows[0].Status)
	}
}

// Watch must not spin, block or panic when probing is switched off.
func TestWatchDisabledReturns(t *testing.T) {
	c := testCatalog(t)
	done := make(chan struct{})
	go func() {
		c.Watch(context.Background(), 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch with a zero interval did not return")
	}
}
