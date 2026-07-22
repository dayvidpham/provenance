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
	err := sqlitex.Execute(db.conn, "SELECT id, namespace, title, description, status_id, priority_id, type_id,\n\t\t        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason\n\t\t FROM tasks WHERE id = ?1", &sqlitex.ExecOptions{
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

	flag := func(enabled bool) int {
		if enabled {
			return 1
		}
		return 0
	}
	var status, priority, taskType, phase any
	if filter.Status != nil {
		status = int(*filter.Status)
	}
	if filter.Priority != nil {
		priority = int(*filter.Priority)
	}
	if filter.Type != nil {
		taskType = int(*filter.Type)
	}
	if filter.Phase != nil {
		phase = int(*filter.Phase)
	}

	var tasks []ptypes.Task
	err := sqlitex.Execute(db.conn, "SELECT id,namespace,title,description,status_id,priority_id,type_id,\n\t\tphase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason FROM tasks\n\t\tWHERE (NOT ?1 OR status_id=?2)\n\t\t  AND (NOT ?3 OR priority_id=?4)\n\t\t  AND (NOT ?5 OR type_id=?6)\n\t\t  AND (NOT ?7 OR phase_id=?8)\n\t\t  AND (NOT ?9 OR namespace=?10)\n\t\t  AND (NOT ?11 OR EXISTS (SELECT ?13 FROM labels l WHERE l.task_id=tasks.id AND l.name=?12))\n\t\tORDER BY created_at ASC", &sqlitex.ExecOptions{
		Args: []any{
			flag(filter.Status != nil), status,
			flag(filter.Priority != nil), priority,
			flag(filter.Type != nil), taskType,
			flag(filter.Phase != nil), phase,
			flag(filter.Namespace != ""), filter.Namespace,
			flag(filter.Label != ""), filter.Label, 1,
		},
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
	err := sqlitex.Execute(db.conn, "SELECT COUNT(*) FROM tasks", &sqlitex.ExecOptions{
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

	var tasks []ptypes.Task
	err := sqlitex.Execute(db.conn, "\n\t\tSELECT t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id,\n\t\t       t.type_id, t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at,\n\t\t       t.closed_at, t.close_reason\n\t\tFROM tasks t\n\t\tWHERE t.status_id != ?1\n\t\tAND NOT EXISTS (\n\t\t\tSELECT ?3 FROM edges e\n\t\t\tJOIN tasks blocker ON e.target_id = blocker.id\n\t\t\tWHERE e.source_id = t.id AND e.kind_id = ?2 AND blocker.status_id != ?1\n\t\t)\n\t\tORDER BY t.priority_id ASC, t.created_at ASC", &sqlitex.ExecOptions{
		Args: []any{int(ptypes.StatusClosed), int(ptypes.EdgeBlockedBy), 1},
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

	var tasks []ptypes.Task
	err := sqlitex.Execute(db.conn, "\n\t\tSELECT t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id,\n\t\t       t.type_id, t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at,\n\t\t       t.closed_at, t.close_reason\n\t\tFROM tasks t\n\t\tWHERE t.status_id != ?1\n\t\tAND EXISTS (\n\t\t\tSELECT ?3 FROM edges e\n\t\t\tJOIN tasks blocker ON e.target_id = blocker.id\n\t\t\tWHERE e.source_id = t.id AND e.kind_id = ?2 AND blocker.status_id != ?1\n\t\t)\n\t\tORDER BY t.priority_id ASC, t.created_at ASC", &sqlitex.ExecOptions{
		Args: []any{int(ptypes.StatusClosed), int(ptypes.EdgeBlockedBy), 1},
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
