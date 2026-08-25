package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// InsertEdge inserts a typed edge.
func (db *DB) InsertEdge(sourceID ptypes.TaskID, targetID string, kind ptypes.EdgeKind, now time.Time) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("sqlite.InsertEdge %q -> %q: %w", sourceID.String(), targetID, err)
	}
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT OR IGNORE INTO edges (source_id, target_id, kind_id, created_at) VALUES (?1, ?2, ?3, ?4)", sourceID.String(), targetID, int(kind), now.UnixNano()); err != nil {
		return fmt.Errorf("sqlite.InsertEdge %q -> %q: %w", sourceID.String(), targetID, err)
	}
	return nil
}

// DeleteEdge deletes an edge.
func (db *DB) DeleteEdge(sourceID ptypes.TaskID, targetID string, kind ptypes.EdgeKind) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("sqlite.DeleteEdge %q -> %q: %w", sourceID.String(), targetID, err)
	}
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "DELETE FROM edges WHERE source_id = ?1 AND target_id = ?2 AND kind_id = ?3", sourceID.String(), targetID, int(kind)); err != nil {
		return fmt.Errorf("sqlite.DeleteEdge %q -> %q: %w", sourceID.String(), targetID, err)
	}
	return nil
}

// GetEdges returns edges originating from sourceID, optionally filtered by kind.
// Pass nil for kind to get all edge kinds.
func (db *DB) GetEdges(sourceID ptypes.TaskID, kind *ptypes.EdgeKind) ([]ptypes.Edge, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetEdges %q: %w", sourceID.String(), err)
	}
	defer scope.release()

	var kindValue any
	if kind != nil {
		kindValue = int(*kind)
	}
	rows, err := scope.conn.QueryContext(scope.ctx, "SELECT source_id,target_id,kind_id FROM edges WHERE source_id=?1 AND (NOT ?2 OR kind_id=?3) ORDER BY created_at ASC", sourceID.String(), kind != nil, kindValue)
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetEdges %q: %w", sourceID.String(), err)
	}
	defer rows.Close()

	edges := make([]ptypes.Edge, 0)
	for rows.Next() {
		var edge ptypes.Edge
		var kindID int
		if err := rows.Scan(&edge.SourceID, &edge.TargetID, &kindID); err != nil {
			return nil, fmt.Errorf("sqlite.GetEdges %q: scan edge row: %w", sourceID.String(), err)
		}
		edge.Kind = ptypes.EdgeKind(kindID)
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.GetEdges %q: iterate edge rows: %w", sourceID.String(), err)
	}
	return edges, nil
}

// GetBlockedByEdges returns all EdgeBlockedBy edges in the database.
func (db *DB) GetBlockedByEdges() ([]ptypes.Edge, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetBlockedByEdges: %w", err)
	}
	defer scope.release()

	rows, err := scope.conn.QueryContext(scope.ctx, "SELECT source_id, target_id, kind_id FROM edges WHERE kind_id = ?1 ORDER BY created_at ASC", int(ptypes.EdgeBlockedBy))
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetBlockedByEdges: %w", err)
	}
	defer rows.Close()

	edges := make([]ptypes.Edge, 0)
	for rows.Next() {
		var edge ptypes.Edge
		var kindID int
		if err := rows.Scan(&edge.SourceID, &edge.TargetID, &kindID); err != nil {
			return nil, fmt.Errorf("sqlite.GetBlockedByEdges: scan edge row: %w", err)
		}
		edge.Kind = ptypes.EdgeKind(kindID)
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.GetBlockedByEdges: iterate edge rows: %w", err)
	}
	return edges, nil
}

// GetDepTree returns all blocked-by edges reachable from rootID via DFS.
// The result is in DFS traversal order.
func (db *DB) GetDepTree(rootID ptypes.TaskID) ([]ptypes.Edge, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetDepTree %q: %w", rootID.String(), err)
	}
	defer scope.release()

	rows, err := scope.conn.QueryContext(scope.ctx, "SELECT source_id, target_id FROM edges WHERE kind_id = ?1", int(ptypes.EdgeBlockedBy))
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetDepTree: %w", err)
	}
	defer rows.Close()
	adj := make(map[string][]string)
	for rows.Next() {
		var source, target string
		if err := rows.Scan(&source, &target); err != nil {
			return nil, fmt.Errorf("sqlite.GetDepTree: scan edge row: %w", err)
		}
		adj[source] = append(adj[source], target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.GetDepTree: iterate edge rows: %w", err)
	}

	var result []ptypes.Edge
	visited := map[string]bool{rootID.String(): true}
	var dfs func(string)
	dfs = func(source string) {
		for _, target := range adj[source] {
			result = append(result, ptypes.Edge{SourceID: source, TargetID: target, Kind: ptypes.EdgeBlockedBy})
			if !visited[target] {
				visited[target] = true
				dfs(target)
			}
		}
	}
	dfs(rootID.String())
	return result, nil
}
