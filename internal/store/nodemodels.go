package store

// Which models each node should be holding.
//
// Desired state only. Nothing here observes a node or talks to one; the agent
// reads its own assignments from the reply to the report it was already
// sending, and reconciles towards them. See internal/node for why that
// direction matters.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// NodeModel is one assignment.
type NodeModel struct {
	Node       string    `json:"node"`
	RelPath    string    `json:"rel_path"`
	AssignedAt time.Time `json:"assigned_at"`
}

// AssignModel records that a node should hold a model. Assigning one it already
// has is not an error: the operator's intent is satisfied either way, and the
// agent reconciles to the same place.
func (s *Store) AssignModel(ctx context.Context, node, relPath string) error {
	name, path := strings.TrimSpace(node), RepoKey(relPath)
	if name == "" || path == "" {
		return fmt.Errorf("assign model: a node and a repository path are required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO node_models (node, rel_path, assigned_at) VALUES (?, ?, ?)
		 ON CONFLICT(node, rel_path) DO NOTHING`,
		name, path, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("assign model: %w", err)
	}
	return nil
}

// UnassignModel drops an assignment.
//
// It does not delete anything from the node. The agent is never told to remove
// weights: a file a node stopped needing costs disk, while an agent that
// deletes on instruction is a permanent liability, and the trade is not close.
// Freeing space on a node is done on the node.
func (s *Store) UnassignModel(ctx context.Context, node, relPath string) error {
	name, path := strings.TrimSpace(node), RepoKey(relPath)
	if name == "" || path == "" {
		return fmt.Errorf("unassign model: a node and a repository path are required")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM node_models WHERE node = ? AND rel_path = ?`, name, path); err != nil {
		return fmt.Errorf("unassign model: %w", err)
	}
	return nil
}

// NodeModels returns one node's assignments, which is the question the report
// handler asks on every push and therefore the one that must stay cheap.
func (s *Store) NodeModels(ctx context.Context, node string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rel_path FROM node_models WHERE node = ? ORDER BY rel_path`,
		strings.TrimSpace(node))
	if err != nil {
		return nil, fmt.Errorf("list node models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan node model: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AllNodeModels returns every assignment grouped by node, for the fleet screen.
func (s *Store) AllNodeModels(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node, rel_path FROM node_models ORDER BY node, rel_path`)
	if err != nil {
		return nil, fmt.Errorf("list assignments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string][]string{}
	for rows.Next() {
		var node, path string
		if err := rows.Scan(&node, &path); err != nil {
			return nil, fmt.Errorf("scan assignment: %w", err)
		}
		out[node] = append(out[node], path)
	}
	return out, rows.Err()
}

// NodesHolding lists the nodes assigned a given model, which is what the
// repository screen needs before offering to delete it: erasing weights three
// machines are expecting is worth a warning.
func (s *Store) NodesHolding(ctx context.Context, relPath string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node FROM node_models WHERE rel_path = ? ORDER BY node`, RepoKey(relPath))
	if err != nil {
		return nil, fmt.Errorf("list nodes holding: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
