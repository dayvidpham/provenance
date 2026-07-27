package sqlite

import (
	"context"
	"fmt"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// AddLabel attaches a label to a task. Idempotent (INSERT OR IGNORE).
func (db *DB) AddLabel(id ptypes.TaskID, label string) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("sqlite.AddLabel %q: %w", id.String(), err)
	}
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT OR IGNORE INTO labels (task_id, name) VALUES (?1, ?2)", id.String(), label); err != nil {
		return fmt.Errorf("sqlite.AddLabel %q: %w", id.String(), err)
	}
	return nil
}

// RemoveLabel detaches a label from a task. Idempotent (no error if not present).
func (db *DB) RemoveLabel(id ptypes.TaskID, label string) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("sqlite.RemoveLabel %q: %w", id.String(), err)
	}
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "DELETE FROM labels WHERE task_id = ?1 AND name = ?2", id.String(), label); err != nil {
		return fmt.Errorf("sqlite.RemoveLabel %q: %w", id.String(), err)
	}
	return nil
}

// GetLabels returns all labels attached to a task, sorted alphabetically.
func (db *DB) GetLabels(id ptypes.TaskID) ([]string, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetLabels %q: %w", id.String(), err)
	}
	defer scope.release()
	rows, err := scope.conn.QueryContext(scope.ctx, "SELECT name FROM labels WHERE task_id = ?1 ORDER BY name ASC", id.String())
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetLabels: %w", err)
	}
	defer rows.Close()
	labels := make([]string, 0)
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("sqlite.GetLabels: scan label row: %w", err)
		}
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.GetLabels: iterate label rows: %w", err)
	}
	return labels, nil
}
