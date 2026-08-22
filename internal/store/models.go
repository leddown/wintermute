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

/* ---------- backends declared through the UI ---------- */

// BackendConfig is a backend declared in the UI rather than in backends.json.
//
// APIKeyEnv names an environment variable; the key itself is never stored.
// See 0011_backend_config.sql for why that is not a limitation to be worked
// around later.
type BackendConfig struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	BaseURL   string    `json:"base_url,omitempty"`
	Model     string    `json:"model,omitempty"`
	APIKeyEnv string    `json:"api_key_env,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BackendConfigs lists the declared backends, by name.
func (s *Store) BackendConfigs(ctx context.Context) ([]BackendConfig, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, kind, base_url, model, api_key_env, created_at, updated_at
		 FROM backend_config ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list backend config: %w", err)
	}
	defer rows.Close()

	var out []BackendConfig
	for rows.Next() {
		var b BackendConfig
		if err := rows.Scan(&b.Name, &b.Kind, &b.BaseURL, &b.Model, &b.APIKeyEnv,
			&b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan backend config: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SaveBackendConfig declares a backend, or updates one already declared under
// the same name.
func (s *Store) SaveBackendConfig(ctx context.Context, b BackendConfig) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO backend_config (name, kind, base_url, model, api_key_env, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   kind = excluded.kind,
		   base_url = excluded.base_url,
		   model = excluded.model,
		   api_key_env = excluded.api_key_env,
		   updated_at = excluded.updated_at`,
		b.Name, b.Kind, b.BaseURL, b.Model, b.APIKeyEnv, now, now)
	if err != nil {
		return fmt.Errorf("save backend config: %w", err)
	}
	return nil
}

// DeleteBackendConfig undeclares a backend, reporting ErrNotFound when there
// was nothing to remove. The probe cache row in `backends` is left alone: it
// is rewritten by the next sweep, which will simply stop including this name.
func (s *Store) DeleteBackendConfig(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM backend_config WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete backend config: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete backend config: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- operator judgements ---------------------------------------------------
//
// The catalog above is a cache of what backends report and is rewritten on
// every probe. These are the operator's own annotations, which nothing
// refreshes. See 0016_model_notes.sql for why they are separate tables.

// ModelNote is what the operator wrote about a model.
type ModelNote struct {
	ModelID   string    `json:"model_id"`
	Note      string    `json:"note"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Champion is the model the operator reaches for at one task.
type Champion struct {
	Task      string    `json:"task"`
	ModelID   string    `json:"model_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NoteKey normalises a model id into the key these tables use.
//
// Lowercasing only, deliberately. Merging engine-specific names — Ollama's
// "qwen3:8b" against vLLM's "Qwen3-8B-Instruct" — needs heuristics, and a
// heuristic that merges wrongly attaches a judgement to a model it was never
// about. Two notes is a far better failure than one misattributed one. The
// common case still collapses: the same model on four Ollama hosts reports the
// same id, so it shares one note.
func NoteKey(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

// SetModelNote writes the operator's note for a model. An empty note deletes
// the row rather than storing a blank one, so "no note" is one state.
func (s *Store) SetModelNote(ctx context.Context, modelID, note string) error {
	key := NoteKey(modelID)
	if key == "" {
		return fmt.Errorf("set model note: a model id is required")
	}
	note = strings.TrimSpace(note)
	if note == "" {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM model_notes WHERE model_id = ?`, key); err != nil {
			return fmt.Errorf("clear model note: %w", err)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_notes (model_id, note, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(model_id) DO UPDATE SET note = excluded.note, updated_at = excluded.updated_at`,
		key, note, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("set model note: %w", err)
	}
	return nil
}

// ModelNotes returns every note, keyed by model id, for attaching to a catalog
// listing in one read rather than one query per model.
func (s *Store) ModelNotes(ctx context.Context) (map[string]ModelNote, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model_id, note, updated_at FROM model_notes`)
	if err != nil {
		return nil, fmt.Errorf("list model notes: %w", err)
	}
	defer rows.Close()

	out := map[string]ModelNote{}
	for rows.Next() {
		var n ModelNote
		if err := rows.Scan(&n.ModelID, &n.Note, &n.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan model note: %w", err)
		}
		out[n.ModelID] = n
	}
	return out, rows.Err()
}

// SetChampion names the model to reach for at a task, replacing whatever held
// the title before. An empty model id clears the task.
//
// One statement, so there is never an instant with two champions for a task and
// never a stale pointer left behind on a superseded model.
func (s *Store) SetChampion(ctx context.Context, task, modelID string) error {
	task = strings.TrimSpace(task)
	if task == "" {
		return fmt.Errorf("set champion: a task is required")
	}
	key := NoteKey(modelID)
	if key == "" {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM model_champions WHERE task = ?`, task); err != nil {
			return fmt.Errorf("clear champion: %w", err)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_champions (task, model_id, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(task) DO UPDATE SET model_id = excluded.model_id, updated_at = excluded.updated_at`,
		task, key, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("set champion: %w", err)
	}
	return nil
}

// Champions returns every task's champion, newest assignment first.
func (s *Store) Champions(ctx context.Context) ([]Champion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT task, model_id, updated_at FROM model_champions ORDER BY task`)
	if err != nil {
		return nil, fmt.Errorf("list champions: %w", err)
	}
	defer rows.Close()

	out := []Champion{}
	for rows.Next() {
		var c Champion
		if err := rows.Scan(&c.Task, &c.ModelID, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan champion: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
