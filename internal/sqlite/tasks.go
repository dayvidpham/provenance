package sqlite

import (
	"fmt"
	"time"

	"github.com/dayvidpham/providence"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// InsertTask inserts a new task row.
// task.ID, task.CreatedAt, and task.UpdatedAt must be set by the caller.
// task.Phase must be a valid Phase value.
func InsertTask(db *DB, task providence.Task) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	var ownerVal any
	if task.Owner != nil {
		ownerVal = task.Owner.String()
	}

	return sqlitex.Execute(db.conn, `
		INSERT INTO tasks
			(id, namespace, title, description, status_id, priority_id, type_id,
			 phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason)
		VALUES
			(?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14)`,
		&sqlitex.ExecOptions{
			Args: []any{
				task.ID.String(),
				task.ID.Namespace,
				task.Title,
				task.Description,
				int(task.Status),
				int(task.Priority),
				int(task.Type),
				int(task.Phase),
				ownerVal,
				task.Notes,
				task.CreatedAt.UnixNano(),
				task.UpdatedAt.UnixNano(),
				timeToNullInt(task.ClosedAt),
				task.CloseReason,
			},
		})
}

// GetTask retrieves a task by ID.
// Returns (Task, true, nil) if found; (Task{}, false, nil) if not found.
func GetTask(db *DB, id providence.TaskID) (providence.Task, bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var task providence.Task
	var found bool

	err := sqlitex.Execute(db.conn,
		`SELECT id, namespace, title, description, status_id, priority_id, type_id,
		        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
		 FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				task, err = scanTask(stmt)
				if err != nil {
					return err
				}
				found = true
				return nil
			},
		})
	if err != nil {
		return providence.Task{}, false, fmt.Errorf(
			"sqlite.GetTask: failed to query task %q: %w — "+
				"check that the database is accessible and not corrupted",
			id.String(), err,
		)
	}
	return task, found, nil
}

// UpdateTask applies partial updates to a task.
// Only non-nil fields in fields are written; updated_at is always refreshed.
func UpdateTask(db *DB, id providence.TaskID, fields providence.UpdateFields, now time.Time) (providence.Task, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Build SET clause dynamically.
	setClauses := []string{"updated_at = ?1"}
	args := []any{now.UnixNano()}
	argIdx := 2

	if fields.Title != nil {
		setClauses = append(setClauses, fmt.Sprintf("title = ?%d", argIdx))
		args = append(args, *fields.Title)
		argIdx++
	}
	if fields.Description != nil {
		setClauses = append(setClauses, fmt.Sprintf("description = ?%d", argIdx))
		args = append(args, *fields.Description)
		argIdx++
	}
	if fields.Status != nil {
		setClauses = append(setClauses, fmt.Sprintf("status_id = ?%d", argIdx))
		args = append(args, int(*fields.Status))
		argIdx++
	}
	if fields.Priority != nil {
		setClauses = append(setClauses, fmt.Sprintf("priority_id = ?%d", argIdx))
		args = append(args, int(*fields.Priority))
		argIdx++
	}
	if fields.Phase != nil {
		setClauses = append(setClauses, fmt.Sprintf("phase_id = ?%d", argIdx))
		args = append(args, int(*fields.Phase))
		argIdx++
	}
	if fields.Notes != nil {
		setClauses = append(setClauses, fmt.Sprintf("notes = ?%d", argIdx))
		args = append(args, *fields.Notes)
		argIdx++
	}
	if fields.Owner != nil {
		s := fields.Owner.String()
		setClauses = append(setClauses, fmt.Sprintf("owner_id = ?%d", argIdx))
		args = append(args, s)
		argIdx++
	}

	// WHERE clause uses the next arg index.
	args = append(args, id.String())
	whereIdx := argIdx

	setSQL := joinClauses(setClauses)
	query := fmt.Sprintf(
		`UPDATE tasks SET %s WHERE id = ?%d`,
		setSQL, whereIdx,
	)

	err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{Args: args})
	if err != nil {
		return providence.Task{}, fmt.Errorf(
			"sqlite.UpdateTask: failed to update task %q: %w — "+
				"ensure the task exists and field values are valid",
			id.String(), err,
		)
	}

	// Re-fetch to return the updated row.
	var task providence.Task
	var found bool
	fetchErr := sqlitex.Execute(db.conn,
		`SELECT id, namespace, title, description, status_id, priority_id, type_id,
		        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
		 FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				task, err = scanTask(stmt)
				if err != nil {
					return err
				}
				found = true
				return nil
			},
		})
	if fetchErr != nil {
		return providence.Task{}, fmt.Errorf(
			"sqlite.UpdateTask: failed to re-fetch task %q after update: %w",
			id.String(), fetchErr,
		)
	}
	if !found {
		return providence.Task{}, fmt.Errorf("%w: task %q not found in sqlite.UpdateTask", providence.ErrNotFound, id.String())
	}
	return task, nil
}

// CloseTask sets a task's status to closed, records the reason and closed_at timestamp.
func CloseTask(db *DB, id providence.TaskID, reason string, now time.Time) (providence.Task, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	closedAtNano := now.UnixNano()
	err := sqlitex.Execute(db.conn,
		`UPDATE tasks SET status_id = 2, close_reason = ?2, closed_at = ?3, updated_at = ?4
		 WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String(), reason, closedAtNano, now.UnixNano()},
		})
	if err != nil {
		return providence.Task{}, fmt.Errorf(
			"sqlite.CloseTask: failed to close task %q: %w — "+
				"check that the task exists and the database is writable",
			id.String(), err,
		)
	}

	var task providence.Task
	var found bool
	fetchErr := sqlitex.Execute(db.conn,
		`SELECT id, namespace, title, description, status_id, priority_id, type_id,
		        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
		 FROM tasks WHERE id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{id.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				task, err = scanTask(stmt)
				if err != nil {
					return err
				}
				found = true
				return nil
			},
		})
	if fetchErr != nil {
		return providence.Task{}, fmt.Errorf("sqlite.CloseTask: re-fetch after close: %w", fetchErr)
	}
	if !found {
		return providence.Task{}, fmt.Errorf("%w: task %q not found in sqlite.CloseTask", providence.ErrNotFound, id.String())
	}
	return task, nil
}

// ListTasks returns all tasks matching filter. Empty filter returns all tasks.
func ListTasks(db *DB, filter providence.ListFilter) ([]providence.Task, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	query := `SELECT id, namespace, title, description, status_id, priority_id, type_id,
	                 phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason
	          FROM tasks WHERE 1=1`
	var args []any
	argIdx := 1

	if filter.Status != nil {
		query += fmt.Sprintf(" AND status_id = ?%d", argIdx)
		args = append(args, int(*filter.Status))
		argIdx++
	}
	if filter.Priority != nil {
		query += fmt.Sprintf(" AND priority_id = ?%d", argIdx)
		args = append(args, int(*filter.Priority))
		argIdx++
	}
	if filter.Type != nil {
		query += fmt.Sprintf(" AND type_id = ?%d", argIdx)
		args = append(args, int(*filter.Type))
		argIdx++
	}
	if filter.Phase != nil {
		query += fmt.Sprintf(" AND phase_id = ?%d", argIdx)
		args = append(args, int(*filter.Phase))
		argIdx++
	}
	if filter.Namespace != "" {
		query += fmt.Sprintf(" AND namespace = ?%d", argIdx)
		args = append(args, filter.Namespace)
		argIdx++
	}
	if filter.Label != "" {
		query += fmt.Sprintf(
			" AND EXISTS (SELECT 1 FROM labels l WHERE l.task_id = tasks.id AND l.name = ?%d)",
			argIdx,
		)
		args = append(args, filter.Label)
		argIdx++
	}

	_ = argIdx // suppress unused warning

	query += " ORDER BY created_at ASC"

	var tasks []providence.Task
	err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *zs.Stmt) error {
			t, err := scanTask(stmt)
			if err != nil {
				return err
			}
			tasks = append(tasks, t)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf(
			"sqlite.ListTasks: query failed: %w — "+
				"check that the database is accessible and filter values are valid",
			err,
		)
	}
	return tasks, nil
}

// ReadyTasks returns tasks that are not closed and have no open blocked-by dependencies.
func ReadyTasks(db *DB) ([]providence.Task, error) {
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
			WHERE e.source_id = t.id
			  AND e.kind_id = 0
			  AND blocker.status_id != 2
		)
		ORDER BY t.priority_id ASC, t.created_at ASC`

	var tasks []providence.Task
	err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zs.Stmt) error {
			t, err := scanTask(stmt)
			if err != nil {
				return err
			}
			tasks = append(tasks, t)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.ReadyTasks: %w", err)
	}
	return tasks, nil
}

// BlockedTasks returns tasks that are not closed and have at least one open blocker.
func BlockedTasks(db *DB) ([]providence.Task, error) {
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
			WHERE e.source_id = t.id
			  AND e.kind_id = 0
			  AND blocker.status_id != 2
		)
		ORDER BY t.priority_id ASC, t.created_at ASC`

	var tasks []providence.Task
	err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zs.Stmt) error {
			t, err := scanTask(stmt)
			if err != nil {
				return err
			}
			tasks = append(tasks, t)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.BlockedTasks: %w", err)
	}
	return tasks, nil
}

// scanTask reads a task from a prepared statement result row.
// Column order must match the SELECT projection used in queries above.
func scanTask(stmt *zs.Stmt) (providence.Task, error) {
	idStr := stmt.ColumnText(0)
	id, err := providence.ParseTaskID(idStr)
	if err != nil {
		return providence.Task{}, fmt.Errorf("scanTask: invalid task ID %q: %w", idStr, err)
	}

	createdAt := time.Unix(0, stmt.ColumnInt64(10)).UTC()
	updatedAt := time.Unix(0, stmt.ColumnInt64(11)).UTC()

	var closedAt *time.Time
	if !stmt.ColumnIsNull(12) {
		t := time.Unix(0, stmt.ColumnInt64(12)).UTC()
		closedAt = &t
	}

	var ownerID *providence.AgentID
	if !stmt.ColumnIsNull(8) {
		aid, err := providence.ParseAgentID(stmt.ColumnText(8))
		if err != nil {
			return providence.Task{}, fmt.Errorf("scanTask: invalid owner_id %q: %w", stmt.ColumnText(8), err)
		}
		ownerID = &aid
	}

	return providence.Task{
		ID:          id,
		Title:       stmt.ColumnText(2),
		Description: stmt.ColumnText(3),
		Status:      providence.Status(stmt.ColumnInt(4)),
		Priority:    providence.Priority(stmt.ColumnInt(5)),
		Type:        providence.TaskType(stmt.ColumnInt(6)),
		Phase:       providence.Phase(stmt.ColumnInt(7)),
		Owner:       ownerID,
		Notes:       stmt.ColumnText(9),
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		ClosedAt:    closedAt,
		CloseReason: stmt.ColumnText(13),
	}, nil
}

// timeToNullInt converts a *time.Time to a nullable int64 (nil -> nil).
func timeToNullInt(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

// joinClauses joins a slice of SQL SET clauses with commas.
func joinClauses(clauses []string) string {
	result := ""
	for i, c := range clauses {
		if i > 0 {
			result += ", "
		}
		result += c
	}
	return result
}
