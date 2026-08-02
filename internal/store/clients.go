package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Client kinds. The kind is informational — it drives nothing but the UI and
// the audit trail — but makes "which device did that rename?" answerable.
const (
	KindHarness = "harness"
	KindBrowser = "browser"
)

// Client is a device authorised to use the API.
type Client struct {
	ID         int64
	Name       string
	Kind       string
	CreatedAt  time.Time
	LastSeenAt *time.Time
}

// CreateClient registers a client and returns it along with the plaintext
// token, which is shown to the operator exactly once and never stored.
func (s *Store) CreateClient(ctx context.Context, name, kind string) (*Client, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, "", errors.New("client name is required")
	}
	token, err := newToken()
	if err != nil {
		return nil, "", err
	}

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO clients (name, token_hash, kind, created_at) VALUES (?, ?, ?, ?)`,
		name, hashToken(token), kind, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, "", fmt.Errorf("client %q: %w", name, ErrDuplicate)
		}
		return nil, "", fmt.Errorf("insert client: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, "", fmt.Errorf("insert client: %w", err)
	}
	return &Client{ID: id, Name: name, Kind: kind, CreatedAt: now}, token, nil
}

// ClientByToken resolves a plaintext bearer token to its client. The token is
// looked up by hash, so the database never holds a usable credential.
func (s *Store) ClientByToken(ctx context.Context, token string) (*Client, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	hash := hashToken(token)

	var c Client
	var storedHash string
	var lastSeen sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, kind, token_hash, created_at, last_seen_at FROM clients WHERE token_hash = ?`,
		hash).Scan(&c.ID, &c.Name, &c.Kind, &storedHash, &c.CreatedAt, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup client: %w", err)
	}
	// Redundant given the indexed lookup already matched, but keeps the
	// comparison constant-time if the query is ever loosened.
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(hash)) != 1 {
		return nil, ErrNotFound
	}
	if lastSeen.Valid {
		c.LastSeenAt = &lastSeen.Time
	}
	return &c, nil
}

// TouchClient records that a client just made a request.
func (s *Store) TouchClient(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE clients SET last_seen_at = ? WHERE id = ?`, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("touch client: %w", err)
	}
	return nil
}

// ListClients returns every registered client, oldest first.
func (s *Store) ListClients(ctx context.Context) ([]Client, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, kind, created_at, last_seen_at FROM clients ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list clients: %w", err)
	}
	defer rows.Close()

	var out []Client
	for rows.Next() {
		var c Client
		var lastSeen sql.NullTime
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.CreatedAt, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan client: %w", err)
		}
		if lastSeen.Valid {
			c.LastSeenAt = &lastSeen.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteClient revokes a client's token by removing it.
func (s *Store) DeleteClient(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM clients WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete client: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete client: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return "wm_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
