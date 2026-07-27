package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// Every PRODUCTION tasks-row WRITE flows through the journaled reducer fold, never a
// direct un-journaled *DB mutator. Creation is the fold's own watermark-carrying INSERT
// (foldTaskCreate in operations.go), reached only through a journaled
// EffectTaskCreate (Session.Create / an Atomic op). Metadata updates and closure are the
// fold's materialization step (materializeTaskEventColumns), reached through a
// journaled EffectTaskEvent (Session.Update / Session.CloseTask); status and owner are
// reducer-exclusive projections advanced only by lifecycle events and assignment
// episodes. The former direct-write mutators were retired for this single path:
// graph.Store.AddVertex no longer creates rows, and db.InsertTask / db.UpdateTask /
// db.CloseTask are gone (they were un-journaled writes into status_id/owner_id/closed_at,
// exactly the divergence §8.1 forbids). Base-layer tests that need an on-disk pre-journal
// task row use the OLD-schema seeding seam (db.SeedLegacyTaskRow / db.SeedLegacyTask,
// legacy_seed.go); tests of the update/close path drive the fold via db.Apply, never a
// direct mutator. This file now holds only read queries over the tasks projection.

const taskColumnsSQL = `id, namespace, title, description, status_id, priority_id, type_id,
	phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason`

const taskColumnsWithTableAliasSQL = `t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id, t.type_id,
	t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at, t.closed_at, t.close_reason`

// GetTask retrieves a task by ID. Returns (task, true, nil) if found,
// (zero, false, nil) if not found, or (zero, false, err) on error.
func (db *DB) GetTask(id ptypes.TaskID) (ptypes.Task, bool, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.Task{}, false, fmt.Errorf("sqlite.GetTask %q: %w", id.String(), err)
	}
	defer scope.release()

	task, err := ScanTask(scope.conn.QueryRowContext(scope.ctx,
		"SELECT "+taskColumnsSQL+" FROM tasks WHERE id = ?1", id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return ptypes.Task{}, false, nil
	}
	if err != nil {
		return ptypes.Task{}, false, fmt.Errorf("sqlite.GetTask %q: %w", id.String(), err)
	}
	return task, true, nil
}

// ListTasks returns tasks matching the given filter. An empty filter returns all
// tasks ordered by creation time (ascending).
func (db *DB) ListTasks(filter ptypes.ListFilter) ([]ptypes.Task, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, fmt.Errorf("sqlite.ListTasks: %w", err)
	}
	defer scope.release()

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

	rows, err := scope.conn.QueryContext(scope.ctx, `SELECT `+taskColumnsSQL+` FROM tasks
		WHERE (NOT ?1 OR status_id=?2)
		  AND (NOT ?3 OR priority_id=?4)
		  AND (NOT ?5 OR type_id=?6)
		  AND (NOT ?7 OR phase_id=?8)
		  AND (NOT ?9 OR namespace=?10)
		  AND (NOT ?11 OR EXISTS (SELECT ?13 FROM labels l WHERE l.task_id=tasks.id AND l.name=?12))
		ORDER BY created_at ASC`,
		flag(filter.Status != nil), status,
		flag(filter.Priority != nil), priority,
		flag(filter.Type != nil), taskType,
		flag(filter.Phase != nil), phase,
		flag(filter.Namespace != ""), filter.Namespace,
		flag(filter.Label != ""), filter.Label, 1,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite.ListTasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]ptypes.Task, 0)
	for rows.Next() {
		task, err := ScanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite.ListTasks: scan task row: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.ListTasks: iterate task rows: %w", err)
	}
	return tasks, nil
}

// TaskCount returns the total number of tasks via COUNT(*).
// This is O(1) in SQLite (index scan).
func (db *DB) TaskCount() (int, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("sqlite.TaskCount: %w", err)
	}
	defer scope.release()

	var count int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM tasks").Scan(&count); err != nil {
		return 0, fmt.Errorf("sqlite.TaskCount: %w", err)
	}
	return count, nil
}

// ReadyTasks returns tasks that are not closed and have no open blockers.
func (db *DB) ReadyTasks() ([]ptypes.Task, error) {
	return db.listTasksByBlockerState(false)
}

// BlockedTasks returns tasks that are not closed and have at least one open blocker.
func (db *DB) BlockedTasks() ([]ptypes.Task, error) {
	return db.listTasksByBlockerState(true)
}

func (db *DB) listTasksByBlockerState(blocked bool) ([]ptypes.Task, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		if blocked {
			return nil, fmt.Errorf("sqlite.BlockedTasks: %w", err)
		}
		return nil, fmt.Errorf("sqlite.ReadyTasks: %w", err)
	}
	defer scope.release()

	predicate := "NOT EXISTS"
	method := "sqlite.ReadyTasks"
	if blocked {
		predicate = "EXISTS"
		method = "sqlite.BlockedTasks"
	}
	rows, err := scope.conn.QueryContext(scope.ctx, `SELECT `+taskColumnsWithTableAliasSQL+` FROM tasks t
		WHERE t.status_id != ?1
		AND `+predicate+` (
			SELECT ?3 FROM edges e
			JOIN tasks blocker ON e.target_id = blocker.id
			WHERE e.source_id = t.id AND e.kind_id = ?2 AND blocker.status_id != ?1
		)
		ORDER BY t.priority_id ASC, t.created_at ASC`, int(ptypes.StatusClosed), int(ptypes.EdgeBlockedBy), 1)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	defer rows.Close()

	tasks := make([]ptypes.Task, 0)
	for rows.Next() {
		task, err := ScanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan task row: %w", method, err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate task rows: %w", method, err)
	}
	return tasks, nil
}
