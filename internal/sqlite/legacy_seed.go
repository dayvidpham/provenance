package sqlite

import (
	"context"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

// SeedLegacyTaskRow is a narrow test-only seam that creates a pre-journal task
// row with no watermark. Production task creation stays in the journal fold.
func (db *DB) SeedLegacyTaskRow(task ptypes.Task) error {
	if task.ID.Namespace == "" {
		return fmt.Errorf("provenance: SeedLegacyTaskRow requires a namespaced legacy TaskID; where: legacy test fixture; impact: nothing seeded; fix: supply a TaskID with a namespace")
	}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("SeedLegacyTaskRow: lease connection: %w", err)
	}
	defer scope.release()
	return scope.insertLegacyTaskRow(task)
}

func (db *DB) SeedLegacyTask(row journal.LegacyTaskRow) error {
	if row.ID.Namespace == "" {
		return fmt.Errorf("provenance: SeedLegacyTask requires a namespaced legacy TaskID; where: legacy test fixture; impact: nothing seeded; fix: supply a TaskID with a namespace")
	}
	task := ptypes.Task{
		ID: row.ID, Title: "legacy task", Status: ptypes.Status(int(row.Status)),
		Priority: ptypes.PriorityMedium, Type: ptypes.TaskTypeTask, Phase: ptypes.PhaseUnscoped,
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(), ClosedAt: row.ClosedAt,
	}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("SeedLegacyTask: lease connection: %w", err)
	}
	defer scope.release()
	return scope.insertLegacyTaskRow(task)
}

func (scope *connScope) insertLegacyTaskRow(task ptypes.Task) error {
	if err := scope.downgradeTasksWatermarkToLegacy(); err != nil {
		return fmt.Errorf("provenance: SeedLegacyTaskRow downgrade tasks to legacy shape: %w", err)
	}
	var owner any
	if task.Owner != nil {
		owner = task.Owner.String()
	}
	var closedAt any
	if task.ClosedAt != nil {
		closedAt = task.ClosedAt.UTC().UnixNano()
	}
	if _, err := scope.conn.ExecContext(scope.ctx, `INSERT INTO tasks
		(id, namespace, title, description, status_id, priority_id, type_id,
		 phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14)`,
		task.ID.String(), task.ID.Namespace, task.Title, task.Description,
		int(task.Status), int(task.Priority), int(task.Type), int(task.Phase), owner, task.Notes,
		task.CreatedAt.UTC().UnixNano(), task.UpdatedAt.UTC().UnixNano(), closedAt, task.CloseReason,
	); err != nil {
		return fmt.Errorf("provenance: SeedLegacyTaskRow insert legacy row %q: %w", task.ID, err)
	}
	return nil
}
