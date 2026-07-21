package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// AddLabel attaches a label to a task. Idempotent (INSERT OR IGNORE).
// Acquires the DB mutex.
func (db *DB) AddLabel(id ptypes.TaskID, label string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return executeStatement(db.conn,
		tasksInsertLabelscc95,
		&sqlitex.ExecOptions{Args: []any{id.String(), label}})
}

// RemoveLabel detaches a label from a task. Idempotent (no error if not present).
// Acquires the DB mutex.
func (db *DB) RemoveLabel(id ptypes.TaskID, label string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return executeStatement(db.conn,
		tasksDeleteLabelsdf09,
		&sqlitex.ExecOptions{Args: []any{id.String(), label}})
}

// GetLabels returns all labels attached to a task, sorted alphabetically.
// Acquires the DB mutex.
func (db *DB) GetLabels(id ptypes.TaskID) ([]string, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var labels []string
	err := executeStatement(db.conn,
		tasksSelectLabels9e23,
		&sqlitex.ExecOptions{
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
