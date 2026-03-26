package sqlite

import (
	"fmt"
	"strings"
	"time"

	"github.com/dayvidpham/providence/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// InsertTask inserts a task row into the tasks table. Acquires the DB mutex.
func (db *DB) InsertTask(task ptypes.Task) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	var ownerVal any
	if task.Owner != nil {
		ownerVal = task.Owner.String()
	}

	return sqlitex.Execute(db.conn,
		`INSERT INTO tasks
			(id, namespace, title, description, status_id, priority_id, type_id,
			 phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14)`,
		&sqlitex.ExecOptions{Args: []any{
			task.ID.String(), task.ID.Namespace, task.Title, task.Description,
			int(task.Status), int(task.Priority), int(task.Type), int(task.Phase),
			ownerVal, task.Notes,
			task.CreatedAt.UnixNano(), task.UpdatedAt.UnixNano(),
			TimeToNullInt(task.ClosedAt), task.CloseReason,
		}})
}

// GetTask retrieves a task by ID. Returns (task, true, nil) if found,
// (zero, false, nil) if not found, or (zero, false, err) on error. Acquires the DB mutex.
func (db *DB) GetTask(id ptypes.TaskID) (ptypes.Task, bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var task ptypes.Task
	var found bool
	err := sqlitex.Execute(db.conn,
		`SELECT id, namespace, title, description, status_id, priority_id, type_id,
		        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
		 FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				task, err = ScanTask(stmt)
				if err != nil {
					return err
				}
				found = true
				return nil
			},
		})
	if err != nil {
		return ptypes.Task{}, false, fmt.Errorf("sqlite.GetTask %q: %w", id.String(), err)
	}
	return task, found, nil
}

// UpdateTask applies partial updates to a task. Returns the updated task.
// Returns ptypes.ErrNotFound if the task does not exist. Acquires the DB mutex.
func (db *DB) UpdateTask(id ptypes.TaskID, fields ptypes.UpdateFields, now time.Time) (ptypes.Task, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	setClauses := []string{"updated_at = ?1"}
	args := []any{now.UnixNano()}
	idx := 2

	if fields.Title != nil {
		setClauses = append(setClauses, fmt.Sprintf("title = ?%d", idx))
		args = append(args, *fields.Title)
		idx++
	}
	if fields.Description != nil {
		setClauses = append(setClauses, fmt.Sprintf("description = ?%d", idx))
		args = append(args, *fields.Description)
		idx++
	}
	if fields.Status != nil {
		setClauses = append(setClauses, fmt.Sprintf("status_id = ?%d", idx))
		args = append(args, int(*fields.Status))
		idx++
	}
	if fields.Priority != nil {
		setClauses = append(setClauses, fmt.Sprintf("priority_id = ?%d", idx))
		args = append(args, int(*fields.Priority))
		idx++
	}
	if fields.Phase != nil {
		setClauses = append(setClauses, fmt.Sprintf("phase_id = ?%d", idx))
		args = append(args, int(*fields.Phase))
		idx++
	}
	if fields.Notes != nil {
		setClauses = append(setClauses, fmt.Sprintf("notes = ?%d", idx))
		args = append(args, *fields.Notes)
		idx++
	}
	if fields.Owner != nil {
		setClauses = append(setClauses, fmt.Sprintf("owner_id = ?%d", idx))
		args = append(args, fields.Owner.String())
		idx++
	}

	args = append(args, id.String())
	whereIdx := idx
	query := fmt.Sprintf(`UPDATE tasks SET %s WHERE id = ?%d`,
		strings.Join(setClauses, ", "), whereIdx)

	if err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		return ptypes.Task{}, fmt.Errorf("sqlite.UpdateTask %q: %w", id.String(), err)
	}

	var task ptypes.Task
	var found bool
	if err := sqlitex.Execute(db.conn,
		`SELECT id, namespace, title, description, status_id, priority_id, type_id,
		        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
		 FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				task, err = ScanTask(stmt)
				found = true
				return err
			},
		}); err != nil {
		return ptypes.Task{}, fmt.Errorf("sqlite.UpdateTask re-fetch %q: %w", id.String(), err)
	}
	if !found {
		return ptypes.Task{}, fmt.Errorf("%w: task %q not found after update", ptypes.ErrNotFound, id.String())
	}
	return task, nil
}

// CloseTask marks a task as closed with the given reason. Returns the updated task.
// Returns ptypes.ErrNotFound if the task does not exist after the update. Acquires the DB mutex.
func (db *DB) CloseTask(id ptypes.TaskID, reason string, now time.Time) (ptypes.Task, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	if err := sqlitex.Execute(db.conn,
		`UPDATE tasks SET status_id = 2, close_reason = ?2, closed_at = ?3, updated_at = ?4 WHERE id = ?1`,
		&sqlitex.ExecOptions{Args: []any{id.String(), reason, now.UnixNano(), now.UnixNano()}}); err != nil {
		return ptypes.Task{}, fmt.Errorf("sqlite.CloseTask %q: %w", id.String(), err)
	}

	var task ptypes.Task
	var found bool
	if err := sqlitex.Execute(db.conn,
		`SELECT id, namespace, title, description, status_id, priority_id, type_id,
		        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
		 FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				task, err = ScanTask(stmt)
				found = true
				return err
			},
		}); err != nil {
		return ptypes.Task{}, fmt.Errorf("sqlite.CloseTask re-fetch %q: %w", id.String(), err)
	}
	if !found {
		return ptypes.Task{}, fmt.Errorf("%w: task %q not found after close", ptypes.ErrNotFound, id.String())
	}
	return task, nil
}

// ListTasks returns tasks matching the given filter. An empty filter returns all
// tasks ordered by creation time (ascending). Acquires the DB mutex.
func (db *DB) ListTasks(filter ptypes.ListFilter) ([]ptypes.Task, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	query := `SELECT id, namespace, title, description, status_id, priority_id, type_id,
	                 phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
	          FROM tasks WHERE 1=1`
	var args []any
	idx := 1

	if filter.Status != nil {
		query += fmt.Sprintf(" AND status_id = ?%d", idx)
		args = append(args, int(*filter.Status))
		idx++
	}
	if filter.Priority != nil {
		query += fmt.Sprintf(" AND priority_id = ?%d", idx)
		args = append(args, int(*filter.Priority))
		idx++
	}
	if filter.Type != nil {
		query += fmt.Sprintf(" AND type_id = ?%d", idx)
		args = append(args, int(*filter.Type))
		idx++
	}
	if filter.Phase != nil {
		query += fmt.Sprintf(" AND phase_id = ?%d", idx)
		args = append(args, int(*filter.Phase))
		idx++
	}
	if filter.Namespace != "" {
		query += fmt.Sprintf(" AND namespace = ?%d", idx)
		args = append(args, filter.Namespace)
		idx++
	}
	if filter.Label != "" {
		query += fmt.Sprintf(
			" AND EXISTS (SELECT 1 FROM labels l WHERE l.task_id = tasks.id AND l.name = ?%d)", idx,
		)
		args = append(args, filter.Label)
		idx++
	}
	_ = idx
	query += " ORDER BY created_at ASC"

	var tasks []ptypes.Task
	err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *zs.Stmt) error {
			task, err := ScanTask(stmt)
			if err != nil {
				return err
			}
			tasks = append(tasks, task)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.ListTasks: %w", err)
	}
	return tasks, nil
}

// TaskCount returns the total number of tasks via COUNT(*).
// This is O(1) in SQLite (index scan). Acquires the DB mutex.
func (db *DB) TaskCount() (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var count int
	err := sqlitex.Execute(db.conn,
		`SELECT COUNT(*) FROM tasks`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *zs.Stmt) error {
				count = stmt.ColumnInt(0)
				return nil
			},
		})
	if err != nil {
		return 0, fmt.Errorf("sqlite.TaskCount: %w", err)
	}
	return count, nil
}

// ReadyTasks returns tasks that are not closed and have no open blockers.
// Acquires the DB mutex.
func (db *DB) ReadyTasks() ([]ptypes.Task, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	const query = `
		SELECT t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id,
		       t.type_id, t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at,
		       t.closed_at, t.close_reason
		FROM tasks t
		WHERE t.status_id != 2
		AND NOT EXISTS (
			SELECT 1 FROM edges e
			JOIN tasks blocker ON e.target_id = blocker.id
			WHERE e.source_id = t.id AND e.kind_id = 0 AND blocker.status_id != 2
		)
		ORDER BY t.priority_id ASC, t.created_at ASC`

	var tasks []ptypes.Task
	err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zs.Stmt) error {
			task, err := ScanTask(stmt)
			if err != nil {
				return err
			}
			tasks = append(tasks, task)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.ReadyTasks: %w", err)
	}
	return tasks, nil
}

// BlockedTasks returns tasks that are not closed and have at least one open blocker.
// Acquires the DB mutex.
func (db *DB) BlockedTasks() ([]ptypes.Task, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	const query = `
		SELECT t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id,
		       t.type_id, t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at,
		       t.closed_at, t.close_reason
		FROM tasks t
		WHERE t.status_id != 2
		AND EXISTS (
			SELECT 1 FROM edges e
			JOIN tasks blocker ON e.target_id = blocker.id
			WHERE e.source_id = t.id AND e.kind_id = 0 AND blocker.status_id != 2
		)
		ORDER BY t.priority_id ASC, t.created_at ASC`

	var tasks []ptypes.Task
	err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zs.Stmt) error {
			task, err := ScanTask(stmt)
			if err != nil {
				return err
			}
			tasks = append(tasks, task)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.BlockedTasks: %w", err)
	}
	return tasks, nil
}
