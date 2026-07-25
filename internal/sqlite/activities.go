package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// StartActivity records the start of an activity for the given agent.
// A UUIDv7 ActivityID is assigned automatically.
func (db *DB) StartActivity(agentID ptypes.AgentID, phase ptypes.Phase, stage ptypes.Stage, notes string) (ptypes.Activity, error) {
	now := time.Now().UTC()
	activity := ptypes.Activity{
		ID:        ptypes.ActivityID{Namespace: agentID.Namespace, UUID: uuid.Must(uuid.NewV7())},
		AgentID:   agentID,
		Phase:     phase,
		Stage:     stage,
		StartedAt: now,
		Notes:     notes,
	}

	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.Activity{}, fmt.Errorf("sqlite.StartActivity: lease connection: %w", err)
	}
	defer scope.release()
	if err := sqlitex.Execute(scope.conn, "INSERT INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", &sqlitex.ExecOptions{Args: []any{
		activity.ID.String(), activity.AgentID.String(),
		int(activity.Phase), int(activity.Stage),
		activity.StartedAt.UnixNano(), nil, activity.Notes,
	}}); err != nil {
		return ptypes.Activity{}, fmt.Errorf(
			"sqlite.StartActivity: failed to insert activity for agent %q: %w — "+
				"ensure the agent is registered before starting an activity",
			agentID.String(), err,
		)
	}
	return activity, nil
}

// StartActivityWithID records the start of an activity using a CALLER-SUPPLIED
// ActivityID, idempotently. Unlike StartActivity (which mints a random UUIDv7),
// the caller owns the id; a second call with the same id is a no-op
// (INSERT ... ON CONFLICT(id) DO NOTHING) and the existing row is returned. This
// makes activity emission safe to replay — e.g. when a durable-workflow step
// re-executes after a crash, a deterministic id (such as a name-based UUIDv5
// over the workflow's logical identity) collapses the duplicate to one row.
func (db *DB) StartActivityWithID(id ptypes.ActivityID, agentID ptypes.AgentID, phase ptypes.Phase, stage ptypes.Stage, notes string) (ptypes.Activity, error) {
	now := time.Now().UTC()
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.Activity{}, fmt.Errorf("sqlite.StartActivityWithID: lease connection: %w", err)
	}
	defer scope.release()

	var act ptypes.Activity
	var found bool
	if err := runTransaction(scope.conn, func() error {
		if err := sqlitex.Execute(scope.conn, "INSERT INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)\n\t\t ON CONFLICT(id) DO NOTHING", &sqlitex.ExecOptions{Args: []any{
			id.String(), agentID.String(), int(phase), int(stage), now.UnixNano(), nil, notes,
		}}); err != nil {
			return fmt.Errorf("failed to insert activity %q for agent %q: %w — ensure the agent is registered before starting an activity", id.String(), agentID.String(), err)
		}
		// Re-fetch the canonical row: the inserted row or the pre-existing row on
		// direct API replay. Journal ActivityCreate conflict rules are separate.
		return sqlitex.Execute(scope.conn, "SELECT id, agent_id, phase_id, stage_id, started_at, ended_at, notes\n\t\t FROM activities WHERE id = ?1", &sqlitex.ExecOptions{
			Args: []any{id.String()}, ResultFunc: func(stmt *zs.Stmt) error {
				var scanErr error
				act, scanErr = ScanActivity(stmt)
				if scanErr != nil {
					return scanErr
				}
				found = true
				return nil
			},
		})
	}); err != nil {
		return ptypes.Activity{}, fmt.Errorf("sqlite.StartActivityWithID: transactional insert and re-fetch: %w", err)
	}
	if !found {
		return ptypes.Activity{}, fmt.Errorf(
			"sqlite.StartActivityWithID: activity %q not found after insert; why: the canonical row disappeared during the transaction; where: direct activity replay re-fetch; when: after INSERT ON CONFLICT; impact: no activity can be returned; fix: verify database integrity and retry", id.String())
	}
	return act, nil
}

// EndActivity records the end time of an activity. Returns the updated activity.
// Returns ptypes.ErrNotFound if the activity does not exist.
func (db *DB) EndActivity(id ptypes.ActivityID) (ptypes.Activity, error) {
	endTime := time.Now().UTC()
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.Activity{}, fmt.Errorf("sqlite.EndActivity: lease connection: %w", err)
	}
	defer scope.release()

	var act ptypes.Activity
	var found bool
	if err := runTransaction(scope.conn, func() error {
		if err := sqlitex.Execute(scope.conn, "UPDATE activities SET ended_at = ?2 WHERE id = ?1", &sqlitex.ExecOptions{Args: []any{id.String(), endTime.UnixNano()}}); err != nil {
			return err
		}
		return sqlitex.Execute(scope.conn, "SELECT id, agent_id, phase_id, stage_id, started_at, ended_at, notes\n\t\t FROM activities WHERE id = ?1", &sqlitex.ExecOptions{
			Args: []any{id.String()}, ResultFunc: func(stmt *zs.Stmt) error {
				var scanErr error
				act, scanErr = ScanActivity(stmt)
				if scanErr != nil {
					return scanErr
				}
				found = true
				return nil
			},
		})
	}); err != nil {
		return ptypes.Activity{}, fmt.Errorf("sqlite.EndActivity: transactional update and re-fetch: %w", err)
	}
	if !found {
		return ptypes.Activity{}, fmt.Errorf(
			"%w: EndActivity — activity %q not found — verify the ActivityID was obtained from StartActivity",
			ptypes.ErrNotFound, id.String(),
		)
	}
	return act, nil
}

// GetActivities returns all activities, optionally filtered by agent.
// Pass nil to return activities for all agents.
func (db *DB) GetActivities(agentID *ptypes.AgentID) ([]ptypes.Activity, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetActivities: lease connection: %w", err)
	}
	defer scope.release()

	var agent any
	if agentID != nil {
		agent = agentID.String()
	}

	var activities []ptypes.Activity
	err = sqlitex.Execute(scope.conn, "SELECT id,agent_id,phase_id,stage_id,started_at,ended_at,notes FROM activities WHERE (NOT ?1 OR agent_id=?2) ORDER BY started_at ASC", &sqlitex.ExecOptions{
		Args: []any{agentID != nil, agent},
		ResultFunc: func(stmt *zs.Stmt) error {
			act, err := ScanActivity(stmt)
			if err != nil {
				return err
			}
			activities = append(activities, act)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetActivities: %w", err)
	}
	return activities, nil
}
