package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

// StartActivity records the start of an activity for the given agent.
// A UUIDv7 ActivityID is assigned automatically.
func (db *DB) StartActivity(agentID ptypes.AgentID, phase ptypes.Phase, stage ptypes.Stage, notes string) (ptypes.Activity, error) {
	now := time.Now().UTC()
	activity := ptypes.Activity{ID: ptypes.ActivityID{Namespace: agentID.Namespace, UUID: uuid.Must(uuid.NewV7())}, AgentID: agentID, Phase: phase, Stage: stage, StartedAt: now, Notes: notes}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.Activity{}, fmt.Errorf("sqlite.StartActivity: lease connection: %w", err)
	}
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", activity.ID.String(), activity.AgentID.String(), int(activity.Phase), int(activity.Stage), activity.StartedAt.UnixNano(), nil, activity.Notes); err != nil {
		return ptypes.Activity{}, fmt.Errorf("sqlite.StartActivity: failed to insert activity for agent %q: %w — ensure the agent is registered before starting an activity", agentID.String(), err)
	}
	return activity, nil
}

// StartActivityWithID records the start of an activity using a CALLER-SUPPLIED
// ActivityID, idempotently. Unlike StartActivity (which mints a random UUIDv7),
// the caller owns the id; a second call with the same id is a no-op and returns
// the canonical existing row.
func (db *DB) StartActivityWithID(id ptypes.ActivityID, agentID ptypes.AgentID, phase ptypes.Phase, stage ptypes.Stage, notes string) (ptypes.Activity, error) {
	now := time.Now().UTC()
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return ptypes.Activity{}, fmt.Errorf("sqlite.StartActivityWithID: lease connection: %w", err)
	}
	defer scope.release()

	var activity ptypes.Activity
	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7) ON CONFLICT(id) DO NOTHING", id.String(), agentID.String(), int(phase), int(stage), now.UnixNano(), nil, notes); err != nil {
			return fmt.Errorf("failed to insert activity %q for agent %q: %w — ensure the agent is registered before starting an activity", id.String(), agentID.String(), err)
		}
		var err error
		activity, err = ScanActivity(scope.conn.QueryRowContext(scope.ctx, "SELECT id, agent_id, phase_id, stage_id, started_at, ended_at, notes FROM activities WHERE id = ?1", id.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite.StartActivityWithID: activity %q not found after insert; why: the canonical row disappeared during the transaction; where: direct activity replay re-fetch; when: after INSERT ON CONFLICT; impact: no activity can be returned; fix: verify database integrity and retry", id.String())
		}
		return err
	}); err != nil {
		return ptypes.Activity{}, fmt.Errorf("sqlite.StartActivityWithID: transactional insert and re-fetch: %w", err)
	}
	return activity, nil
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

	var activity ptypes.Activity
	if err := runImmediateTransaction(scope.ctx, scope.conn, func() error {
		if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE activities SET ended_at = ?2 WHERE id = ?1", id.String(), endTime.UnixNano()); err != nil {
			return err
		}
		var err error
		activity, err = ScanActivity(scope.conn.QueryRowContext(scope.ctx, "SELECT id, agent_id, phase_id, stage_id, started_at, ended_at, notes FROM activities WHERE id = ?1", id.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: EndActivity — activity %q not found — verify the ActivityID was obtained from StartActivity", ptypes.ErrNotFound, id.String())
		}
		return err
	}); err != nil {
		return ptypes.Activity{}, fmt.Errorf("sqlite.EndActivity: transactional update and re-fetch: %w", err)
	}
	return activity, nil
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
	rows, err := scope.conn.QueryContext(scope.ctx, "SELECT id,agent_id,phase_id,stage_id,started_at,ended_at,notes FROM activities WHERE (NOT ?1 OR agent_id=?2) ORDER BY started_at ASC", agentID != nil, agent)
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetActivities: %w", err)
	}
	defer rows.Close()
	activities := make([]ptypes.Activity, 0)
	for rows.Next() {
		activity, err := ScanActivity(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite.GetActivities: scan activity row: %w", err)
		}
		activities = append(activities, activity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.GetActivities: iterate activity rows: %w", err)
	}
	return activities, nil
}
