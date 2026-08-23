package store

// The model repository index: provenance for weights kept on the operator's own
// disk, and the flat labels they are sorted by.
//
// The file on disk is the source of truth. Nothing here is authoritative about
// whether a model exists — a listing walks the repository and uses these rows
// only to say where a file came from and what was verified about it on arrival.
// See 0018_model_repo.sql.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RepoFile is one recorded file in the model repository.
type RepoFile struct {
	// RelPath is the path within the repository root, always with forward
	// slashes so a row written on one mount point matches on the next.
	RelPath   string    `json:"rel_path"`
	HubID     string    `json:"hub_id,omitempty"`
	SourceURL string    `json:"source_url,omitempty"`
	Quant     string    `json:"quant,omitempty"`
	ParamsB   float64   `json:"params_b,omitempty"`
	SizeBytes int64     `json:"size_bytes,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	AddedAt   time.Time `json:"added_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RecordRepoFile writes what is known about a file that has landed in the
// repository, replacing any earlier record of the same path.
//
// AddedAt is preserved across a re-download on purpose: re-fetching a file that
// was corrupted is the same file arriving again, not a new acquisition, and
// resetting the date would lose the only record of when the operator first
// chose it.
func (s *Store) RecordRepoFile(ctx context.Context, f RepoFile) error {
	key := RepoKey(f.RelPath)
	if key == "" {
		return fmt.Errorf("record repo file: a relative path is required")
	}
	now := time.Now().UTC()
	if f.AddedAt.IsZero() {
		f.AddedAt = now
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_repo_files
		   (rel_path, hub_id, source_url, quant, params_b, size_bytes, sha256, added_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(rel_path) DO UPDATE SET
		   hub_id = excluded.hub_id, source_url = excluded.source_url,
		   quant = excluded.quant, params_b = excluded.params_b,
		   size_bytes = excluded.size_bytes, sha256 = excluded.sha256,
		   updated_at = excluded.updated_at`,
		key, f.HubID, f.SourceURL, f.Quant, f.ParamsB, f.SizeBytes, f.SHA256, f.AddedAt, now)
	if err != nil {
		return fmt.Errorf("record repo file: %w", err)
	}
	return nil
}

// RepoFiles returns every recorded file, keyed by relative path, for merging
// into a directory walk in one read rather than one query per file.
func (s *Store) RepoFiles(ctx context.Context) (map[string]RepoFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rel_path, hub_id, source_url, quant, params_b, size_bytes, sha256, added_at, updated_at
		   FROM model_repo_files`)
	if err != nil {
		return nil, fmt.Errorf("list repo files: %w", err)
	}
	defer rows.Close()

	out := map[string]RepoFile{}
	for rows.Next() {
		var f RepoFile
		if err := rows.Scan(&f.RelPath, &f.HubID, &f.SourceURL, &f.Quant,
			&f.ParamsB, &f.SizeBytes, &f.SHA256, &f.AddedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan repo file: %w", err)
		}
		out[f.RelPath] = f
	}
	return out, rows.Err()
}

// ForgetRepoFile drops the record of a file. The file itself is the caller's
// business — this only removes what was remembered about it.
//
// Tags are left behind deliberately. Deleting a quantisation to free space and
// fetching a better one under the same name is the common case, and the labels
// the operator put on it were about the model, not about those particular
// bytes.
func (s *Store) ForgetRepoFile(ctx context.Context, relPath string) error {
	key := RepoKey(relPath)
	if key == "" {
		return fmt.Errorf("forget repo file: a relative path is required")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM model_repo_files WHERE rel_path = ?`, key); err != nil {
		return fmt.Errorf("forget repo file: %w", err)
	}
	return nil
}

// RepoKey normalises a repository-relative path into the key the tables use.
//
// Separators are normalised to forward slashes and any leading "./" or "/" is
// stripped, so the same file identified from a walk and from a download request
// lands on one row. Case is left alone, unlike NoteKey: this is a filename on a
// Linux filesystem, where two paths differing in case are two different files,
// and folding them would attach one file's provenance to another's.
func RepoKey(relPath string) string {
	p := strings.ReplaceAll(strings.TrimSpace(relPath), "\\", "/")
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	return strings.Trim(p, "/")
}

// ---- tags ------------------------------------------------------------------

// AddTag labels a subject. Adding a tag it already carries is not an error;
// the operator's intent is satisfied either way.
func (s *Store) AddTag(ctx context.Context, modelID, tag string) error {
	key, label := RepoKey(modelID), TagKey(tag)
	if key == "" {
		return fmt.Errorf("add tag: a subject is required")
	}
	if label == "" {
		return fmt.Errorf("add tag: a tag is required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_tags (model_id, tag, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(model_id, tag) DO NOTHING`,
		key, label, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("add tag: %w", err)
	}
	return nil
}

// RemoveTag takes a label off a subject.
func (s *Store) RemoveTag(ctx context.Context, modelID, tag string) error {
	key, label := RepoKey(modelID), TagKey(tag)
	if key == "" || label == "" {
		return fmt.Errorf("remove tag: a subject and a tag are required")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM model_tags WHERE model_id = ? AND tag = ?`, key, label); err != nil {
		return fmt.Errorf("remove tag: %w", err)
	}
	return nil
}

// Tags returns every tag, grouped by subject and sorted within each, so a
// listing renders the same order every time it is drawn.
func (s *Store) Tags(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model_id, tag FROM model_tags ORDER BY model_id, tag`)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		out[id] = append(out[id], tag)
	}
	return out, rows.Err()
}

// TagKey normalises a label: trimmed, lowercased, and inner whitespace
// collapsed to single hyphens.
//
// Lowercased because "Coding" and "coding" are one label that a person typed
// twice, and a filter that treats them as two is a filter that quietly hides
// half the matches.
func TagKey(tag string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(tag))), "-")
}
