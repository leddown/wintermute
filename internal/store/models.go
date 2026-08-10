package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// BackendRow is a configured model backend as last observed.
type BackendRow struct {
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	BaseURL    string     `json:"base_url,omitempty"`
	Model      string     `json:"model,omitempty"`
	Cloud      bool       `json:"cloud"`
	Status     string     `json:"status"`
	StatusNote string     `json:"status_note,omitempty"`
	ProbedAt   *time.Time `json:"probed_at,omitempty"`
}

// Backend health values.
const (
	BackendOK          = "ok"
	BackendUnreachable = "unreachable"
	BackendUnknown     = "unknown"
)

// CatalogRow is one model a backend reported.
type CatalogRow struct {
	Backend      string    `json:"backend"`
	ModelID      string    `json:"model_id"`
	Family       string    `json:"family,omitempty"`
	ParamsB      float64   `json:"params_b,omitempty"`
	Quant        string    `json:"quant,omitempty"`
	CtxLen       int       `json:"ctx_len,omitempty"`
	SizeBytes    int64     `json:"size_bytes,omitempty"`
	Capabilities []string  `json:"capabilities,omitempty"`
	Loaded       bool      `json:"loaded"`
	VRAMBytes    int64     `json:"vram_bytes,omitempty"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

// UpsertBackend records a backend and the outcome of its last probe.
func (s *Store) UpsertBackend(ctx context.Context, b BackendRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO backends (name, kind, base_url, model, cloud, status, status_note, probed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   kind = excluded.kind,
		   base_url = excluded.base_url,
		   model = excluded.model,
		   cloud = excluded.cloud,
		   status = excluded.status,
		   status_note = excluded.status_note,
		   probed_at = excluded.probed_at`,
		b.Name, b.Kind, b.BaseURL, b.Model, b.Cloud, b.Status, b.StatusNote, b.ProbedAt)
	if err != nil {
		return fmt.Errorf("upsert backend: %w", err)
	}
	return nil
}

// Backends lists every known backend, ordered by name.
func (s *Store) Backends(ctx context.Context) ([]BackendRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, kind, base_url, model, cloud, status, status_note, probed_at
		 FROM backends ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list backends: %w", err)
	}
	defer rows.Close()

	var out []BackendRow
	for rows.Next() {
		var b BackendRow
		if err := rows.Scan(&b.Name, &b.Kind, &b.BaseURL, &b.Model, &b.Cloud,
			&b.Status, &b.StatusNote, &b.ProbedAt); err != nil {
			return nil, fmt.Errorf("scan backend: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ReplaceCatalog swaps in the models a backend reported, in one transaction.
//
// A probe returns the backend's complete inventory, so stale rows are deleted
// rather than merged — otherwise a model removed from the server would linger
// in the catalog and be offered to the user forever.
func (s *Store) ReplaceCatalog(ctx context.Context, backend string, models []CatalogRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin catalog replace: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_models WHERE backend_name = ?`, backend); err != nil {
		return fmt.Errorf("clear catalog: %w", err)
	}

	now := time.Now().UTC()
	for _, m := range models {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO catalog_models
			   (backend_name, model_id, family, params_b, quant, ctx_len, size_bytes,
			    capabilities, loaded, vram_bytes, last_seen_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			backend, m.ModelID, m.Family, m.ParamsB, m.Quant, m.CtxLen, m.SizeBytes,
			strings.Join(m.Capabilities, ","), m.Loaded, m.VRAMBytes, now)
		if err != nil {
			return fmt.Errorf("insert catalog model: %w", err)
		}
	}
	return tx.Commit()
}

// Catalog returns every known model across backends, ordered for display.
func (s *Store) Catalog(ctx context.Context) ([]CatalogRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT backend_name, model_id, family, params_b, quant, ctx_len, size_bytes,
		        capabilities, loaded, vram_bytes, last_seen_at
		 FROM catalog_models ORDER BY backend_name, model_id`)
	if err != nil {
		return nil, fmt.Errorf("list catalog: %w", err)
	}
	defer rows.Close()

	var out []CatalogRow
	for rows.Next() {
		var m CatalogRow
		var caps string
		if err := rows.Scan(&m.Backend, &m.ModelID, &m.Family, &m.ParamsB, &m.Quant,
			&m.CtxLen, &m.SizeBytes, &caps, &m.Loaded, &m.VRAMBytes, &m.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan catalog model: %w", err)
		}
		if caps != "" {
			m.Capabilities = strings.Split(caps, ",")
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
