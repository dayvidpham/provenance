package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Every PRODUCTION tasks-row WRITE flows through the journaled reducer fold, never a
// direct un-journaled *DB mutator. Creation is the fold's own watermark-carrying INSERT
// (foldTaskCreateLocked in operations.go), reached only through a journaled
// EffectTaskCreate (Session.Create / an Atomic op). Metadata updates and closure are the
// fold's materialization step (materializeTaskEventColumnsLocked), reached through a
// journaled EffectTaskEvent (Session.Update / Session.CloseTask); status and owner are
// reducer-exclusive projections advanced only by lifecycle events and assignment
// episodes. The former direct-write mutators were retired for this single path:
// graph.Store.AddVertex no longer creates rows, and db.InsertTask / db.UpdateTask /
// db.CloseTask are gone (they were un-journaled writes into status_id/owner_id/closed_at,
// exactly the divergence §8.1 forbids). Base-layer tests that need an on-disk pre-journal
// task row use the OLD-schema seeding seam (db.SeedLegacyTaskRow / db.SeedLegacyTask,
// legacy_seed.go); tests of the update/close path drive the fold via db.Apply, never a
// direct mutator. This file now holds only read queries over the tasks projection.

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
