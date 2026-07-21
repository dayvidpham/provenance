package sqlite

import (
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// InsertEdge inserts a typed edge. Acquires the DB mutex.
func (db *DB) InsertEdge(sourceID ptypes.TaskID, targetID string, kind ptypes.EdgeKind, now time.Time) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return sqlitex.Execute(db.conn,
		`INSERT OR IGNORE INTO edges (source_id, target_id, kind_id, created_at) VALUES (?1, ?2, ?3, ?4)`,
		&sqlitex.ExecOptions{Args: []any{sourceID.String(), targetID, int(kind), now.UnixNano()}})
}

// DeleteEdge deletes an edge. Acquires the DB mutex.
func (db *DB) DeleteEdge(sourceID ptypes.TaskID, targetID string, kind ptypes.EdgeKind) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return sqlitex.Execute(db.conn,
		`DELETE FROM edges WHERE source_id = ?1 AND target_id = ?2 AND kind_id = ?3`,
		&sqlitex.ExecOptions{Args: []any{sourceID.String(), targetID, int(kind)}})
}

// GetEdges returns edges originating from sourceID, optionally filtered by kind.
// Pass nil for kind to get all edge kinds. Acquires the DB mutex.
func (db *DB) GetEdges(sourceID ptypes.TaskID, kind *ptypes.EdgeKind) ([]ptypes.Edge, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var kindValue any
	if kind != nil {
		kindValue = int(*kind)
	}

	var edges []ptypes.Edge
	err := sqlitex.Execute(db.conn, `SELECT source_id,target_id,kind_id FROM edges WHERE source_id=?1 AND (NOT ?2 OR kind_id=?3) ORDER BY created_at ASC`, &sqlitex.ExecOptions{
		Args: []any{sourceID.String(), kind != nil, kindValue},
		ResultFunc: func(stmt *zs.Stmt) error {
			edges = append(edges, ptypes.Edge{
				SourceID: stmt.ColumnText(0),
				TargetID: stmt.ColumnText(1),
				Kind:     ptypes.EdgeKind(stmt.ColumnInt(2)),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetEdges %q: %w", sourceID.String(), err)
	}
	return edges, nil
}

// GetBlockedByEdges returns all EdgeBlockedBy edges in the database.
// Acquires the DB mutex.
func (db *DB) GetBlockedByEdges() ([]ptypes.Edge, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var edges []ptypes.Edge
	err := sqlitex.Execute(db.conn,
		`SELECT source_id, target_id, kind_id FROM edges WHERE kind_id = ?1 ORDER BY created_at ASC`,
		&sqlitex.ExecOptions{Args: []any{int(ptypes.EdgeBlockedBy)},
			ResultFunc: func(stmt *zs.Stmt) error {
				edges = append(edges, ptypes.Edge{
					SourceID: stmt.ColumnText(0),
					TargetID: stmt.ColumnText(1),
					Kind:     ptypes.EdgeBlockedBy,
				})
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetBlockedByEdges: %w", err)
	}
	return edges, nil
}

// GetDepTree returns all blocked-by edges reachable from rootID via DFS.
// The result is in DFS traversal order. Acquires the DB mutex.
func (db *DB) GetDepTree(rootID ptypes.TaskID) ([]ptypes.Edge, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	adj := make(map[string][]string)
	if err := sqlitex.Execute(db.conn,
		`SELECT source_id, target_id FROM edges WHERE kind_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{int(ptypes.EdgeBlockedBy)},
			ResultFunc: func(stmt *zs.Stmt) error {
				src := stmt.ColumnText(0)
				tgt := stmt.ColumnText(1)
				adj[src] = append(adj[src], tgt)
				return nil
			},
		}); err != nil {
		return nil, fmt.Errorf("sqlite.GetDepTree: %w", err)
	}

	var result []ptypes.Edge
	visited := make(map[string]bool)
	var dfs func(srcID string)
	dfs = func(srcID string) {
		for _, tgtID := range adj[srcID] {
			result = append(result, ptypes.Edge{SourceID: srcID, TargetID: tgtID, Kind: ptypes.EdgeBlockedBy})
			if !visited[tgtID] {
				visited[tgtID] = true
				dfs(tgtID)
			}
		}
	}
	visited[rootID.String()] = true
	dfs(rootID.String())
	return result, nil
}
