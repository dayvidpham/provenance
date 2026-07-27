package sqlite

import (
	"context"
	"fmt"
)

// tasksWatermarkShape describes the three on-disk task watermark forms.
type tasksWatermarkShape uint8

const (
	tasksWatermarkNative tasksWatermarkShape = iota + 1
	tasksWatermarkNullable
	tasksWatermarkColumnless
)

func (shape tasksWatermarkShape) createRebuildQuery() string {
	switch shape {
	case tasksWatermarkNative:
		return "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '',last_journal_id INTEGER NOT NULL REFERENCES journal(journal_id)) STRICT"
	case tasksWatermarkNullable:
		return "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '',last_journal_id INTEGER REFERENCES journal(journal_id)) STRICT"
	case tasksWatermarkColumnless:
		return "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '') STRICT"
	default:
		panic("unknown tasks watermark shape")
	}
}

func (shape tasksWatermarkShape) copyQuery() string {
	switch shape {
	case tasksWatermarkNative, tasksWatermarkNullable:
		return "INSERT INTO tasks_watermark_rebuild (id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id) SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks"
	case tasksWatermarkColumnless:
		return "INSERT INTO tasks_watermark_rebuild (id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason) SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason FROM tasks"
	default:
		panic("unknown tasks watermark shape")
	}
}

func (scope *connScope) tasksWatermarkColumnInfo() (present bool, notNull bool, err error) {
	rows, err := scope.conn.QueryContext(scope.ctx, "PRAGMA table_info(tasks)")
	if err != nil {
		return false, false, fmt.Errorf("tasksWatermarkColumnInfo: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNullValue, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNullValue, &defaultValue, &primaryKey); err != nil {
			return false, false, fmt.Errorf("tasksWatermarkColumnInfo: scan table_info: %w", err)
		}
		if name == "last_journal_id" {
			present = true
			notNull = notNullValue == 1
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("tasksWatermarkColumnInfo: %w", err)
	}
	return present, notNull, nil
}

func (scope *connScope) countUnanchoredTasks() (int, error) {
	present, _, err := scope.tasksWatermarkColumnInfo()
	if err != nil {
		return 0, err
	}
	if !present {
		return 0, fmt.Errorf("countUnanchoredTasks: tasks table has no last_journal_id column after migration column-add; fix: inspect the migration path")
	}
	var count int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM tasks WHERE last_journal_id IS NULL").Scan(&count); err != nil {
		return 0, fmt.Errorf("countUnanchoredTasks: %w", err)
	}
	return count, nil
}

// rebuildTasksWatermark performs the foreign-key-safe SQLite rebuild on this
// exact connection. PRAGMA foreign_keys is connection-local and cannot change
// inside a transaction, so it is toggled before the explicit BEGIN IMMEDIATE.
func (scope *connScope) rebuildTasksWatermark(shape tasksWatermarkShape) (err error) {
	if _, err = scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("rebuildTasksWatermark: disable FK enforcement: %w", err)
	}
	defer func() {
		if _, restoreErr := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=ON"); restoreErr != nil && err == nil {
			err = fmt.Errorf("rebuildTasksWatermark: restore FK enforcement: %w", restoreErr)
		}
	}()
	return runImmediateTransaction(scope.ctx, scope.conn, func() error {
		if _, err := scope.conn.ExecContext(scope.ctx, shape.createRebuildQuery()); err != nil {
			return fmt.Errorf("rebuildTasksWatermark: create rebuild table: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, shape.copyQuery()); err != nil {
			return fmt.Errorf("rebuildTasksWatermark: copy rows: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "DROP TABLE tasks"); err != nil {
			return fmt.Errorf("rebuildTasksWatermark: drop old table: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "ALTER TABLE tasks_watermark_rebuild RENAME TO tasks"); err != nil {
			return fmt.Errorf("rebuildTasksWatermark: rename rebuilt table: %w", err)
		}
		for _, statement := range []string{
			"CREATE INDEX IF NOT EXISTS idx_tasks_namespace ON tasks (namespace)",
			"CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status_id)",
			"CREATE INDEX IF NOT EXISTS idx_tasks_priority ON tasks (priority_id)",
			"CREATE INDEX IF NOT EXISTS idx_tasks_type ON tasks (type_id)",
			"CREATE INDEX IF NOT EXISTS idx_tasks_phase ON tasks (phase_id)",
			"CREATE INDEX IF NOT EXISTS idx_tasks_owner ON tasks (owner_id)",
		} {
			if _, err := scope.conn.ExecContext(scope.ctx, statement); err != nil {
				return fmt.Errorf("rebuildTasksWatermark: recreate index: %w", err)
			}
		}
		rows, err := scope.conn.QueryContext(scope.ctx, "PRAGMA foreign_key_check")
		if err != nil {
			return fmt.Errorf("rebuildTasksWatermark: foreign_key_check: %w", err)
		}
		defer rows.Close()
		violations := 0
		for rows.Next() {
			violations++
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rebuildTasksWatermark: foreign_key_check: %w", err)
		}
		if violations != 0 {
			return fmt.Errorf("rebuildTasksWatermark: rebuild left %d foreign-key violations; the transaction will roll back; fix: repair child rows before retrying", violations)
		}
		return nil
	})
}

func (scope *connScope) downgradeTasksWatermarkToLegacy() error {
	present, notNull, err := scope.tasksWatermarkColumnInfo()
	if err != nil {
		return err
	}
	if !present || !notNull {
		return nil
	}
	return scope.rebuildTasksWatermark(tasksWatermarkNullable)
}

// DowngradeTasksToColumnlessLegacy is a narrow test-only legacy fixture seam.
func (db *DB) DowngradeTasksToColumnlessLegacy() error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("DowngradeTasksToColumnlessLegacy: lease connection: %w", err)
	}
	defer scope.release()
	present, _, err := scope.tasksWatermarkColumnInfo()
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return scope.rebuildTasksWatermark(tasksWatermarkColumnless)
}

func (scope *connScope) ensureTasksWatermarkColumn() error {
	present, _, err := scope.tasksWatermarkColumnInfo()
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "ALTER TABLE tasks ADD COLUMN last_journal_id INTEGER REFERENCES journal(journal_id)"); err != nil {
		return fmt.Errorf("ensureTasksWatermarkColumn: add legacy last_journal_id column: %w", err)
	}
	return nil
}
