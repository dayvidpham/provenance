package sqlite

import (
	"context"
	"fmt"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// AddLabel attaches a label to a task. Idempotent (INSERT OR IGNORE).
func (db *DB) AddLabel(id ptypes.TaskID, label string) error {
	scope, err := db.bindConn(context.Background())
	if err != nil {
		return fmt.Errorf("sqlite.AddLabel %q: %w", id.String(), err)
	}
	defer scope.release()
	return sqlitex.Execute(scope.conn, "INSERT OR IGNORE INTO labels (task_id, name) VALUES (?1, ?2)", &sqlitex.ExecOptions{Args: []any{id.String(), label}})
}

// RemoveLabel detaches a label from a task. Idempotent (no error if not present).
func (db *DB) RemoveLabel(id ptypes.TaskID, label string) error {
	scope, err := db.bindConn(context.Background())
	if err != nil {
		return fmt.Errorf("sqlite.RemoveLabel %q: %w", id.String(), err)
	}
	defer scope.release()
	return sqlitex.Execute(scope.conn, "DELETE FROM labels WHERE task_id = ?1 AND name = ?2", &sqlitex.ExecOptions{Args: []any{id.String(), label}})
}

// GetLabels returns all labels attached to a task, sorted alphabetically.
func (db *DB) GetLabels(id ptypes.TaskID) ([]string, error) {
	scope, err := db.bindConn(context.Background())
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetLabels %q: %w", id.String(), err)
	}
	defer scope.release()
	var labels []string
	err = sqlitex.Execute(scope.conn, "SELECT name FROM labels WHERE task_id = ?1 ORDER BY name ASC", &sqlitex.ExecOptions{
		Args: []any{id.String()},
		ResultFunc: func(stmt *zs.Stmt) error {
			labels = append(labels, stmt.ColumnText(0))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetLabels: %w", err)
	}
	return labels, nil
}
