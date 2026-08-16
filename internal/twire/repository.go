package twire

// twire's persistence, on SQLite.
//
// The same three differences from the PostgreSQL original that the fintech
// package documents apply here, for the same reasons:
//
//   - Timestamps are RFC 3339 text written by this layer rather than by the
//     database. Postgres filled them with now(); SQLite's CURRENT_TIMESTAMP
//     writes a format Go does not parse back as RFC 3339, so the times are
//     produced in Go, once, in timestamp().
//   - Duplicate detection reads the driver's error text rather than a SQLSTATE.
//     modernc's driver reports a violated UNIQUE as a message containing
//     "UNIQUE constraint failed"; there is no pgconn.PgError to type-assert.
//   - Booleans are stored as INTEGER 0/1. database/sql scans those into a Go
//     bool without help, so only the writes need to say so.
//
// One more, particular to this module: InsertEvent used RETURNING id, which
// SQLite supports but the driver reports more simply through LastInsertId.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repository persists canary enabled-state, recorded events, and the singleton
// email-alert configuration to SQLite.
type Repository struct {
	db *sql.DB
}

// NewRepository builds a Repository backed by db.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// timestamp is how every time value in this package reaches the database.
func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// parseTime reads a stored timestamp back. An unparseable or empty value
// becomes the zero time rather than an error: a missing timestamp should cost
// an event its date, not its existence — and an event is a security record
// worth keeping even when its clock field is malformed.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// isUniqueViolation reports whether err is SQLite refusing a duplicate.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// EnabledSet returns the enabled flag for every canary that has ever been
// toggled, keyed by profile key. Keys absent from the map default to disabled.
func (r *Repository) EnabledSet(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT profile_key, enabled FROM twire_canaries`)
	if err != nil {
		return nil, fmt.Errorf("twire: list canaries: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var key string
		var enabled bool
		if err := rows.Scan(&key, &enabled); err != nil {
			return nil, fmt.Errorf("twire: scan canary: %w", err)
		}
		out[key] = enabled
	}
	return out, rows.Err()
}

// SetCanaryEnabled upserts a canary's enabled flag.
func (r *Repository) SetCanaryEnabled(ctx context.Context, key string, enabled bool) error {
	now := timestamp(time.Now())
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO twire_canaries (profile_key, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (profile_key) DO UPDATE SET
			enabled = excluded.enabled, updated_at = excluded.updated_at`,
		key, enabled, now, now,
	)
	if err != nil {
		return fmt.Errorf("twire: set canary enabled: %w", err)
	}
	return nil
}

// ListCustomCanaries returns every operator-defined canary profile, ordered by
// port. Banner is included so custom canaries can greet on connect just like
// built-in ones.
func (r *Repository) ListCustomCanaries(ctx context.Context) ([]ServiceProfile, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT profile_key, name, port, description, banner
		FROM twire_custom_canaries
		ORDER BY port`)
	if err != nil {
		return nil, fmt.Errorf("twire: list custom canaries: %w", err)
	}
	defer rows.Close()
	var out []ServiceProfile
	for rows.Next() {
		var p ServiceProfile
		if err := rows.Scan(&p.Key, &p.Name, &p.Port, &p.Description, &p.Banner); err != nil {
			return nil, fmt.Errorf("twire: scan custom canary: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// InsertCustomCanary persists a new custom canary. A duplicate port (the UNIQUE
// constraint, or the generated profile_key primary key) is reported as
// ErrPortTaken.
func (r *Repository) InsertCustomCanary(ctx context.Context, p ServiceProfile) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO twire_custom_canaries (profile_key, name, port, description, banner, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		p.Key, p.Name, p.Port, p.Description, p.Banner, timestamp(time.Now()),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrPortTaken
		}
		return fmt.Errorf("twire: insert custom canary: %w", err)
	}
	return nil
}

// DeleteCustomCanary removes a custom canary definition and its enabled-state
// row (in twire_canaries). Recorded events are intentionally left in place as a
// historical log. found is false when no such custom canary existed.
func (r *Repository) DeleteCustomCanary(ctx context.Context, key string) (found bool, err error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM twire_custom_canaries WHERE profile_key = ?`, key)
	if err != nil {
		return false, fmt.Errorf("twire: delete custom canary: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("twire: delete custom canary rows: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM twire_canaries WHERE profile_key = ?`, key); err != nil {
		return true, fmt.Errorf("twire: clear custom canary enabled state: %w", err)
	}
	return true, nil
}

// InsertEvent records one connection attempt and returns its new ID.
func (r *Repository) InsertEvent(ctx context.Context, e Event) (int64, error) {
	at := e.OccurredAt
	if at.IsZero() {
		at = time.Now()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO twire_events (profile_key, service_name, port, remote_ip, remote_port, data_preview, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ProfileKey, e.ServiceName, e.Port, e.RemoteIP, e.RemotePort, e.DataPreview, timestamp(at),
	)
	if err != nil {
		return 0, fmt.Errorf("twire: insert event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("twire: insert event id: %w", err)
	}
	return id, nil
}

// ListEvents returns the most recent events, newest first, up to limit.
//
// Ordering by the stored text is correct rather than merely convenient: every
// timestamp is written in UTC by timestamp(), and RFC 3339 leads with the
// date, so lexical order is chronological order — the same property the todo
// package relies on.
func (r *Repository) ListEvents(ctx context.Context, limit int) ([]Event, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, profile_key, service_name, port, remote_ip, remote_port, data_preview, occurred_at
		FROM twire_events
		ORDER BY occurred_at DESC, id DESC
		LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("twire: list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var occurredAt string
		if err := rows.Scan(&e.ID, &e.ProfileKey, &e.ServiceName, &e.Port,
			&e.RemoteIP, &e.RemotePort, &e.DataPreview, &occurredAt); err != nil {
			return nil, fmt.Errorf("twire: scan event: %w", err)
		}
		e.OccurredAt = parseTime(occurredAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventCountsByProfile returns the total recorded hit count per profile key.
func (r *Repository) EventCountsByProfile(ctx context.Context) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT profile_key, count(*) FROM twire_events GROUP BY profile_key`)
	if err != nil {
		return nil, fmt.Errorf("twire: count events: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var key string
		var n int64
		if err := rows.Scan(&key, &n); err != nil {
			return nil, fmt.Errorf("twire: scan event count: %w", err)
		}
		out[key] = n
	}
	return out, rows.Err()
}

// GetAlertConfig returns the saved alert configuration. found is false when no
// row has been saved yet (the caller then falls back to env defaults). The
// returned AlertConfig has no password populated; the encrypted bytes are
// returned separately for the service to decrypt.
func (r *Repository) GetAlertConfig(ctx context.Context) (cfg AlertConfig, enc, nonce []byte, found bool, err error) {
	var recipients string
	row := r.db.QueryRowContext(ctx, `
		SELECT enabled, smtp_username, smtp_password_enc, smtp_password_nonce, smtp_from, recipients
		FROM twire_alert_config WHERE id = 1`)
	err = row.Scan(&cfg.Enabled, &cfg.SMTPUsername, &enc, &nonce, &cfg.From, &recipients)
	if errors.Is(err, sql.ErrNoRows) {
		return AlertConfig{}, nil, nil, false, nil
	}
	if err != nil {
		return AlertConfig{}, nil, nil, false, fmt.Errorf("twire: get alert config: %w", err)
	}
	cfg.Recipients = splitRecipients(recipients)
	return cfg, enc, nonce, true, nil
}

// SetAlertConfig upserts the singleton alert configuration. The password must
// already be encrypted by the caller.
func (r *Repository) SetAlertConfig(ctx context.Context, cfg AlertConfig, enc, nonce []byte) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO twire_alert_config
			(id, enabled, smtp_username, smtp_password_enc, smtp_password_nonce, smtp_from, recipients, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			enabled = excluded.enabled,
			smtp_username = excluded.smtp_username,
			smtp_password_enc = excluded.smtp_password_enc,
			smtp_password_nonce = excluded.smtp_password_nonce,
			smtp_from = excluded.smtp_from,
			recipients = excluded.recipients,
			updated_at = excluded.updated_at`,
		cfg.Enabled, cfg.SMTPUsername, enc, nonce, cfg.From,
		strings.Join(cfg.Recipients, ","), timestamp(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("twire: set alert config: %w", err)
	}
	return nil
}

// splitRecipients parses a comma-separated recipient list, trimming blanks.
func splitRecipients(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}
