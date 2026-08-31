package models

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"wintermute/internal/store"
	"wintermute/internal/store/storetest"
)

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	st := storetest.New(t)
	return NewCatalog(nil, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A recorded "ok" describes the moment it was written. Once the prober has
// stopped confirming it, the health endpoint must stop repeating it — that is
// the difference between the UI admitting it has lost touch with a machine and
// it reporting a powered-off host as healthy.
func TestBackendHealthStaleOKReadsUnknown(t *testing.T) {
	c := testCatalog(t)
	ctx := context.Background()
	c.SetBackends([]Backend{{Name: "workshop", Kind: KindOllama, BaseURL: "http://workshop:11434"}})

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
	c.SetBackends([]Backend{{Name: "workshop", Kind: KindOllama}})

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

// The health table is a probe cache that nothing prunes, so a backend that has
// been undeclared leaves its last verdict behind. Reporting that row would show
// a removed backend as healthy indefinitely, which is the exact failure the
// staleness horizon exists to prevent — so it must be dropped, not aged.
func TestBackendHealthDropsUnconfiguredBackends(t *testing.T) {
	c := testCatalog(t)
	ctx := context.Background()
	c.SetBackends([]Backend{{Name: "kept", Kind: KindOllama}, {Name: "removed", Kind: KindOllama}})

	now := time.Now().UTC()
	for _, name := range []string{"kept", "removed"} {
		if err := c.store.UpsertBackend(ctx, store.BackendRow{
			Name: name, Kind: "ollama", Status: store.BackendOK, ProbedAt: &now,
		}); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
	}

	// Undeclare one, exactly as deleting it in the UI does.
	c.SetBackends([]Backend{{Name: "kept", Kind: KindOllama}})

	rows, err := c.BackendHealth(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "kept" {
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = r.Name
		}
		t.Fatalf("health reports %v, want only [kept]", names)
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
