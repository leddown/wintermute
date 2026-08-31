package twire

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"wintermute/internal/store/storetest"
)

// These exercise the SQLite rewrite of a repository that was written for
// PostgreSQL. Everything that had to change on the way across is something a
// test can catch and a compiler cannot: `$1` became `?`, `now()` became a Go
// timestamp, RETURNING became LastInsertId, BYTEA became BLOB, and a SQLSTATE
// check became a string match on the driver's error text. A throwaway SQLite
// file makes all of that verifiable on every run, with no server to configure
// and nothing to skip when there isn't one.

// newTestRepository opens a SQLite database in a temporary directory with the
// migrations applied.
//
// A file rather than the ":memory:" the fintech tests use, because twire writes
// from a goroutine the listener spawns. Every ":memory:" connection gets its own
// private database, so with the pool at four connections a hit recorded on one
// lands somewhere the reader on another cannot see it — the migrations having
// run on a third. Sequential tests never notice; the end-to-end one below fails
// on a missing table.
func newTestRepository(t *testing.T) *Repository {
	t.Helper()
	st := storetest.New(t)
	return NewRepository(st.DB())
}

// discardLogger keeps the service's warnings out of the test output; the tests
// assert on returned values, not on what was logged.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCanaryEnabledRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)

	// A canary nobody has touched is absent from the table, not false in it.
	set, err := repo.EnabledSet(ctx)
	if err != nil {
		t.Fatalf("EnabledSet error: %v", err)
	}
	if len(set) != 0 {
		t.Fatalf("EnabledSet on a fresh database = %v, want empty", set)
	}

	if err := repo.SetCanaryEnabled(ctx, "ssh", true); err != nil {
		t.Fatalf("SetCanaryEnabled error: %v", err)
	}
	// The second call takes the ON CONFLICT path, which is the half of the
	// upsert a single insert would never reach.
	if err := repo.SetCanaryEnabled(ctx, "ssh", false); err != nil {
		t.Fatalf("SetCanaryEnabled (update) error: %v", err)
	}
	if err := repo.SetCanaryEnabled(ctx, "redis", true); err != nil {
		t.Fatalf("SetCanaryEnabled error: %v", err)
	}

	set, err = repo.EnabledSet(ctx)
	if err != nil {
		t.Fatalf("EnabledSet error: %v", err)
	}
	if got, want := len(set), 2; got != want {
		t.Fatalf("EnabledSet size = %d, want %d", got, want)
	}
	if set["ssh"] {
		t.Error("ssh = enabled, want disabled after the update")
	}
	if !set["redis"] {
		t.Error("redis = disabled, want enabled")
	}
}

func TestCustomCanaryLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)

	profile := ServiceProfile{
		Key: customProfileKey(8080), Name: "Admin panel", Port: 8080,
		Description: "Internal", Banner: "HTTP/1.0 200 OK\r\n",
	}
	if err := repo.InsertCustomCanary(ctx, profile); err != nil {
		t.Fatalf("InsertCustomCanary error: %v", err)
	}

	// The UNIQUE port is what stops two canaries fighting over one socket, and
	// it has to surface as ErrPortTaken rather than as a raw driver error —
	// this is the check that changed shape when pgconn went away.
	if err := repo.InsertCustomCanary(ctx, profile); !errors.Is(err, ErrPortTaken) {
		t.Fatalf("duplicate InsertCustomCanary = %v, want ErrPortTaken", err)
	}

	got, err := repo.ListCustomCanaries(ctx)
	if err != nil {
		t.Fatalf("ListCustomCanaries error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListCustomCanaries = %d rows, want 1", len(got))
	}
	if got[0] != profile {
		t.Errorf("ListCustomCanaries[0] = %+v, want %+v", got[0], profile)
	}

	// Deleting takes the enabled-state row with it, so a re-created canary on
	// the same port does not inherit the old one's switch position.
	if err := repo.SetCanaryEnabled(ctx, profile.Key, true); err != nil {
		t.Fatalf("SetCanaryEnabled error: %v", err)
	}
	found, err := repo.DeleteCustomCanary(ctx, profile.Key)
	if err != nil {
		t.Fatalf("DeleteCustomCanary error: %v", err)
	}
	if !found {
		t.Error("DeleteCustomCanary found = false, want true")
	}
	set, err := repo.EnabledSet(ctx)
	if err != nil {
		t.Fatalf("EnabledSet error: %v", err)
	}
	if _, ok := set[profile.Key]; ok {
		t.Error("the enabled-state row outlived the custom canary")
	}

	found, err = repo.DeleteCustomCanary(ctx, profile.Key)
	if err != nil {
		t.Fatalf("second DeleteCustomCanary error: %v", err)
	}
	if found {
		t.Error("DeleteCustomCanary on a missing row found = true, want false")
	}
}

func TestEventsRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// Deliberately inserted out of order, so passing proves the ORDER BY works
	// rather than that rows come back the way they went in.
	for i, at := range []time.Time{base.Add(time.Minute), base, base.Add(2 * time.Minute)} {
		id, err := repo.InsertEvent(ctx, Event{
			ProfileKey: "ssh", ServiceName: "SSH", Port: 22,
			RemoteIP: "192.0.2.5", RemotePort: 40000 + i,
			DataPreview: "GET / HTTP/1.1", OccurredAt: at,
		})
		if err != nil {
			t.Fatalf("InsertEvent error: %v", err)
		}
		// LastInsertId replaced RETURNING id; a zero here means the swap lost
		// the identifier silently.
		if id == 0 {
			t.Error("InsertEvent id = 0, want the new row's id")
		}
	}
	if _, err := repo.InsertEvent(ctx, Event{
		ProfileKey: "redis", ServiceName: "Redis", Port: 6379,
		RemoteIP: "192.0.2.9", RemotePort: 51000, OccurredAt: base,
	}); err != nil {
		t.Fatalf("InsertEvent error: %v", err)
	}

	events, err := repo.ListEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ListEvents error: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("ListEvents = %d events, want 4", len(events))
	}
	// Newest first, and the timestamp has to survive the trip through TEXT.
	if want := base.Add(2 * time.Minute); !events[0].OccurredAt.Equal(want) {
		t.Errorf("newest event at %v, want %v", events[0].OccurredAt, want)
	}
	if events[0].DataPreview != "GET / HTTP/1.1" {
		t.Errorf("data preview = %q, want it preserved", events[0].DataPreview)
	}

	if got := len(mustEvents(t, repo, 2)); got != 2 {
		t.Errorf("ListEvents(limit 2) = %d events, want 2", got)
	}

	counts, err := repo.EventCountsByProfile(ctx)
	if err != nil {
		t.Fatalf("EventCountsByProfile error: %v", err)
	}
	if counts["ssh"] != 3 || counts["redis"] != 1 {
		t.Errorf("counts = %v, want ssh:3 redis:1", counts)
	}
}

func mustEvents(t *testing.T, repo *Repository, limit int) []Event {
	t.Helper()
	events, err := repo.ListEvents(context.Background(), limit)
	if err != nil {
		t.Fatalf("ListEvents error: %v", err)
	}
	return events
}

func TestAlertConfigRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	svc := NewService(repo, []byte("test-secret"), AlertConfig{From: "env@example.com"}, discardLogger())

	// With nothing saved, the environment defaults are what the caller gets.
	cfg, err := svc.GetAlertConfig(ctx)
	if err != nil {
		t.Fatalf("GetAlertConfig error: %v", err)
	}
	if cfg.From != "env@example.com" {
		t.Errorf("unsaved config From = %q, want the env default", cfg.From)
	}

	saved := AlertConfig{
		Enabled: true, SMTPUsername: "alerts@example.com", SMTPPassword: "app-password",
		From: "alerts@example.com", Recipients: []string{"me@example.com", "you@example.com"},
	}
	if err := svc.SetAlertConfig(ctx, saved, true); err != nil {
		t.Fatalf("SetAlertConfig error: %v", err)
	}

	got, err := svc.GetAlertConfig(ctx)
	if err != nil {
		t.Fatalf("GetAlertConfig error: %v", err)
	}
	// The password is the whole point of the BLOB columns and the AES round
	// trip; everything else here is ordinary text.
	if got.SMTPPassword != "app-password" {
		t.Errorf("password = %q, want it decrypted back", got.SMTPPassword)
	}
	if len(got.Recipients) != 2 || got.Recipients[0] != "me@example.com" {
		t.Errorf("recipients = %v, want both, in order", got.Recipients)
	}
	if !got.deliverable() {
		t.Error("deliverable = false, want true for a fully configured alert")
	}

	// Saving without a password keeps the stored one, which is what lets the
	// settings form edit a recipient list without re-typing a credential.
	edited := saved
	edited.SMTPPassword = ""
	edited.Recipients = []string{"me@example.com"}
	if err := svc.SetAlertConfig(ctx, edited, false); err != nil {
		t.Fatalf("SetAlertConfig (no password) error: %v", err)
	}
	got, err = svc.GetAlertConfig(ctx)
	if err != nil {
		t.Fatalf("GetAlertConfig error: %v", err)
	}
	if got.SMTPPassword != "app-password" {
		t.Errorf("password = %q after an edit that omitted it, want it retained", got.SMTPPassword)
	}
	if len(got.Recipients) != 1 {
		t.Errorf("recipients = %v, want the edit applied", got.Recipients)
	}
}

// Without WINTERMUTE_SECRET there is no key, and a password must be refused
// rather than stored in a form anyone with the database file can read.
func TestAlertConfigWithoutSecret(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	svc := NewService(repo, nil, AlertConfig{}, discardLogger())

	if svc.SecretConfigured() {
		t.Fatal("SecretConfigured = true with no secret")
	}

	withPassword := AlertConfig{
		Enabled: true, From: "alerts@example.com",
		Recipients: []string{"me@example.com"}, SMTPPassword: "app-password",
	}
	if err := svc.SetAlertConfig(ctx, withPassword, true); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("SetAlertConfig with a password and no secret = %v, want ErrNoSecret", err)
	}

	// The rest of the configuration still saves, so alerting can be set up to
	// the credential and the operator told exactly what is missing.
	noPassword := withPassword
	noPassword.SMTPPassword = ""
	if err := svc.SetAlertConfig(ctx, noPassword, true); err != nil {
		t.Fatalf("SetAlertConfig without a password error: %v", err)
	}
	got, err := svc.GetAlertConfig(ctx)
	if err != nil {
		t.Fatalf("GetAlertConfig error: %v", err)
	}
	if got.From != "alerts@example.com" || got.SMTPPassword != "" {
		t.Errorf("got %+v, want the config saved and the password empty", got)
	}
}

// A password stored under one secret is unreadable under another. Losing it is
// the accepted cost of rotation, but the rest of the configuration must survive
// and the call must not fail — the settings page's job is to say "re-enter it".
func TestAlertConfigSurvivesRotatedSecret(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)

	original := NewService(repo, []byte("first-secret"), AlertConfig{}, discardLogger())
	cfg := AlertConfig{
		Enabled: true, From: "alerts@example.com",
		Recipients: []string{"me@example.com"}, SMTPPassword: "app-password",
	}
	if err := original.SetAlertConfig(ctx, cfg, true); err != nil {
		t.Fatalf("SetAlertConfig error: %v", err)
	}

	rotated := NewService(repo, []byte("second-secret"), AlertConfig{}, discardLogger())
	got, err := rotated.GetAlertConfig(ctx)
	if err != nil {
		t.Fatalf("GetAlertConfig after rotation error: %v", err)
	}
	if got.SMTPPassword != "" {
		t.Errorf("password = %q, want it unreadable under the new secret", got.SMTPPassword)
	}
	if got.From != "alerts@example.com" {
		t.Errorf("From = %q, want the rest of the config intact", got.From)
	}
}

func TestServiceStatusIncludesCustomCanaries(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	svc := NewService(repo, nil, AlertConfig{}, discardLogger())

	if _, err := svc.CreateCustom(ctx, "Admin panel", 8080, "Internal", ""); err != nil {
		t.Fatalf("CreateCustom error: %v", err)
	}
	// A port the built-in catalog already claims belongs to that canary, not to
	// a second one shadowing it.
	if _, err := svc.CreateCustom(ctx, "Fake SSH", 22, "", ""); !errors.Is(err, ErrPortReserved) {
		t.Fatalf("CreateCustom on a catalog port = %v, want ErrPortReserved", err)
	}
	if _, err := svc.CreateCustom(ctx, "", 9000, "", ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("CreateCustom with no name = %v, want ErrValidation", err)
	}

	statuses, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if len(statuses) != len(catalog)+1 {
		t.Fatalf("Status = %d canaries, want the catalog plus one custom", len(statuses))
	}
	last := statuses[len(statuses)-1]
	if !last.Custom || last.Port != 8080 {
		t.Errorf("last status = %+v, want the custom canary on 8080", last)
	}
	// Nothing was enabled, so nothing should be listening — this is the promise
	// that installing twire opens no sockets by itself.
	for _, s := range statuses {
		if s.Enabled || s.Listening {
			t.Errorf("canary %q enabled=%v listening=%v, want both false by default",
				s.Key, s.Enabled, s.Listening)
		}
	}

	// A built-in canary cannot be deleted, only switched off.
	if err := svc.DeleteCustom(ctx, "ssh"); !errors.Is(err, ErrNotCustom) {
		t.Fatalf("DeleteCustom on a built-in = %v, want ErrNotCustom", err)
	}
	if err := svc.DeleteCustom(ctx, customProfileKey(8080)); err != nil {
		t.Fatalf("DeleteCustom error: %v", err)
	}
	if err := svc.Enable(ctx, customProfileKey(8080)); !errors.Is(err, ErrUnknownCanary) {
		t.Fatalf("Enable on a deleted canary = %v, want ErrUnknownCanary", err)
	}
}

// The end-to-end path: a canary that is enabled binds its port, greets a
// caller with its banner, and records the connection as an event. This is the
// one test that proves twire does the thing it exists for, rather than that its
// bookkeeping round-trips.
func TestEnabledCanaryRecordsAConnection(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	svc := NewService(repo, nil, AlertConfig{}, discardLogger())

	port := freePort(t)
	if _, err := svc.CreateCustom(ctx, "Test service", port, "", "HELLO\r\n"); err != nil {
		t.Fatalf("CreateCustom error: %v", err)
	}
	key := customProfileKey(port)

	// Start() wires the shutdown path, so cancelling the context is what closes
	// the listener — the same lifecycle the app gives it.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	svc.Start(runCtx)

	if err := svc.Enable(ctx, key); err != nil {
		t.Fatalf("Enable error: %v", err)
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("dialling the canary: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	banner := make([]byte, len("HELLO\r\n"))
	if _, err := io.ReadFull(conn, banner); err != nil {
		t.Fatalf("reading the banner: %v", err)
	}
	if string(banner) != "HELLO\r\n" {
		t.Errorf("banner = %q, want the profile's", banner)
	}
	// The canary records what the caller sends, so send something recognisable.
	if _, err := conn.Write([]byte("PROBE\x00\x01")); err != nil {
		t.Fatalf("writing to the canary: %v", err)
	}
	conn.Close()

	// The hit is recorded from the connection goroutine, so wait for it rather
	// than assuming it has landed.
	var events []Event
	for i := 0; i < 100; i++ {
		events = mustEvents(t, repo, 10)
		if len(events) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].Port != port || events[0].RemoteIP != "127.0.0.1" {
		t.Errorf("event = %+v, want the canary's port and a loopback source", events[0])
	}
	// Non-printable bytes are flattened before storage, so a probe cannot smuggle
	// control characters into anything that later displays the preview.
	if events[0].DataPreview != "PROBE.." {
		t.Errorf("data preview = %q, want the sanitised %q", events[0].DataPreview, "PROBE..")
	}

	// Disabling closes the socket, which is what makes the toggle mean anything.
	if err := svc.Disable(ctx, key); err != nil {
		t.Fatalf("Disable error: %v", err)
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second); err == nil {
		c.Close()
		t.Error("the canary still accepts connections after Disable")
	}
}

// freePort asks the kernel for an unused port and hands it back. There is a
// race between closing this listener and the canary binding it, but no way to
// avoid one while the canary opens its own socket — and a hard-coded port would
// collide with whatever the machine running the tests happens to be doing.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
