package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// plans.go implements the plan-layer persistence (roadmap §3.1): the plans /
// plan_steps projection tables, the idempotent activities.plan_id column-add
// migration, the built-in "pasture-12-phase" plan seed, and the plan reads. These
// are direct-write projection tables (like activities), outside the §15
// journal-convergence set.

// builtinPlanPhases is the ordered Phase sequence the built-in plan reifies: the
// 12 protocol phases plus the 'unscoped' catch-all last (13 steps total), in
// Phase enum order.
var builtinPlanPhases = []ptypes.Phase{
	ptypes.PhaseRequest, ptypes.PhaseElicit, ptypes.PhasePropose, ptypes.PhaseReview,
	ptypes.PhasePlanUAT, ptypes.PhaseRatify, ptypes.PhaseHandoff, ptypes.PhaseImplPlan,
	ptypes.PhaseWorkerSlices, ptypes.PhaseCodeReview, ptypes.PhaseImplUAT, ptypes.PhaseLanding,
	ptypes.PhaseUnscoped,
}

// ensureActivitiesPlanColumnLocked adds the nullable activities.plan_id FK column
// if it is absent (roadmap §3.1 additive migration). Idempotent: a no-op once the
// column exists. Mirrors ensureTasksWatermarkColumnLocked. Assumes db.mu is held.
func (db *DB) ensureActivitiesPlanColumnLocked() error {
	cols, err := db.tableColumnsLocked("activities")
	if err != nil {
		return err
	}
	if _, ok := cols["plan_id"]; ok {
		return nil
	}
	if err := sqlitex.ExecuteTransient(db.conn,
		"ALTER TABLE activities ADD COLUMN plan_id TEXT REFERENCES plans(id)", nil); err != nil {
		return fmt.Errorf(
			"ensureActivitiesPlanColumn: add plan_id column — where: plan-layer additive migration "+
				"(§3.1); when: before any activity references a plan; impact: nothing committed; fix: the "+
				"existing activities table shape is not understood: %w", err)
	}
	return nil
}

// seedBuiltinPlanLocked seeds the single built-in "pasture-12-phase" plan and its
// 13 steps idempotently (roadmap §3.1). INSERT OR IGNORE preserves any existing
// rows on re-open. Assumes db.mu is held; runs inside the activation transaction so
// its FKs are validated by VerifyIntegrity before commit.
func (db *DB) seedBuiltinPlanLocked() error {
	planID := ptypes.BuiltinPlanID().String()
	if err := sqlitex.Execute(db.conn,
		"INSERT OR IGNORE INTO plans (id, title, version) VALUES (?1, ?2, ?3)",
		&sqlitex.ExecOptions{Args: []any{planID, ptypes.BuiltinPlanTitle, ptypes.BuiltinPlanVersion}}); err != nil {
		return fmt.Errorf("seedBuiltinPlan: insert plan: %w", err)
	}
	for ordinal, phase := range builtinPlanPhases {
		if err := sqlitex.Execute(db.conn,
			"INSERT OR IGNORE INTO plan_steps (plan_id, ordinal, phase_id, title) VALUES (?1, ?2, ?3, ?4)",
			&sqlitex.ExecOptions{Args: []any{planID, ordinal, int(phase), phase.String()}}); err != nil {
			return fmt.Errorf("seedBuiltinPlan: insert step %d: %w", ordinal, err)
		}
	}
	return nil
}

// GetPlans returns every plan, ordered deterministically by ID. Acquires the DB
// mutex.
func (db *DB) GetPlans() ([]ptypes.Plan, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var plans []ptypes.Plan
	err := sqlitex.Execute(db.conn, "SELECT id, title, version FROM plans ORDER BY id ASC", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *zs.Stmt) error {
			id, perr := ptypes.ParsePlanID(stmt.ColumnText(0))
			if perr != nil {
				return fmt.Errorf("scan plan: invalid plan id %q: %w", stmt.ColumnText(0), perr)
			}
			plans = append(plans, ptypes.Plan{ID: id, Title: stmt.ColumnText(1), Version: stmt.ColumnText(2)})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetPlans: %w", err)
	}
	return plans, nil
}

// GetPlanSteps returns the steps of one plan, ordered by ordinal. Acquires the DB
// mutex.
func (db *DB) GetPlanSteps(planID ptypes.PlanID) ([]ptypes.PlanStep, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	var steps []ptypes.PlanStep
	err := sqlitex.Execute(db.conn, "SELECT ordinal, phase_id, title FROM plan_steps WHERE plan_id = ?1 ORDER BY ordinal ASC", &sqlitex.ExecOptions{
		Args: []any{planID.String()},
		ResultFunc: func(stmt *zs.Stmt) error {
			steps = append(steps, ptypes.PlanStep{
				PlanID:  planID,
				Ordinal: stmt.ColumnInt(0),
				Phase:   ptypes.Phase(stmt.ColumnInt(1)),
				Title:   stmt.ColumnText(2),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetPlanSteps %q: %w", planID.String(), err)
	}
	return steps, nil
}
