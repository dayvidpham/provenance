package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// activity_replay.go implements the EffectActivityCreate fold inside Apply's
// write transaction and the journal_activity_creations schema.
//
// The fold:
//  1. Validates ActivityID and AgentID (Canonicalize already does this; we
//     double-check for defense-in-depth).
//  2. Strictly inserts the activities row. Exact OperationID replay has already
//     short-circuited before folding, so any existing ActivityID is a conflict.
//  3. On an ActivityID uniqueness conflict only, looks up an existing journal
//     birth attribution and returns typed *ActivityConflict.
//  4. After a fresh activity insert, inserts journal_activity_creations.
//
// The result-slot recording (insertResultSlot) is the caller's
// responsibility (foldEffect in operations.go), consistent with all other
// effect kinds.
//
// ActivityID collision semantics: every pre-existing ActivityID conflicts,
// including activities created outside the journal. The fold rolls back the
// whole operation via the returned error. DBOS callers treat this as a terminal
// domain failure.

// ensureActivityCreationsSchema idempotently creates journal_activity_creations.
// Called from ensureOperationsSchema (operations.go) during DB activation.
func (scope *connScope) ensureActivityCreationsSchema() error {
	ddl := []string{
		// journal_activity_creations: one row per journaled Activity birth.
		//   journal_id PK FK journal          → ties birth to global journal spine
		//   activity_id UNIQUE FK activities  → ActivityID is born exactly once
		"CREATE TABLE IF NOT EXISTS journal_activity_creations (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id), activity_id TEXT NOT NULL UNIQUE REFERENCES activities(id)) STRICT",
		"CREATE INDEX IF NOT EXISTS idx_journal_activity_creations_activity ON journal_activity_creations (activity_id)",
	}
	for _, stmt := range ddl {
		if err := sqlitex.ExecuteTransient(scope.conn, stmt, nil); err != nil {
			return fmt.Errorf("ensureActivityCreationsSchema: %w", err)
		}
	}
	return nil
}

// foldActivityCreate folds one EffectActivityCreate inside Apply's write
// transaction. The caller owns scope.conn and the enclosing Apply savepoint.
func (scope *connScope) foldActivityCreate(in journal.OperationInput, jid int64, eff journal.Effect) error {
	activityID := eff.ActivityID
	if activityID.Namespace == "" {
		return fmt.Errorf(
			"Apply: operation %q EffectActivityCreate: ActivityID has an empty namespace — "+
				"where: EffectActivityCreate fold; when: before writes; "+
				"impact: operation rejected without writes; "+
				"fix: supply a valid namespaced ActivityID (Canonicalize enforces this before Apply)",
			in.OperationID)
	}
	agentID := ptypes.AgentID(eff.ActivityAgentID)
	if agentID.Namespace == "" {
		return fmt.Errorf(
			"Apply: operation %q EffectActivityCreate: ActivityAgentID has an empty namespace — "+
				"where: EffectActivityCreate fold; when: before writes; "+
				"impact: operation rejected without writes; "+
				"fix: supply a valid namespaced AgentID",
			in.OperationID)
	}

	// Step 1: Strictly insert the activities row. Exact OperationID replay exits
	// before folding, and the Apply savepoint removes every partial prior attempt.
	recordedAt := in.RecordedAt
	if eff.RecordedAtOverride != nil {
		recordedAt = *eff.RecordedAtOverride
	}
	if err := sqlitex.Execute(scope.conn,
		`INSERT INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)`,
		&sqlitex.ExecOptions{Args: []any{
			activityID.String(), agentID.String(),
			int(eff.ActivityPhase), int(eff.ActivityStage),
			recordedAt, nil, eff.ActivityNotes,
		}}); err != nil {
		if isUniqueViolation(err) {
			var claimingJournalID int64
			if lookupErr := sqlitex.Execute(scope.conn,
				`SELECT journal_id FROM journal_activity_creations WHERE activity_id = ?1`,
				&sqlitex.ExecOptions{
					Args: []any{activityID.String()},
					ResultFunc: func(stmt *zs.Stmt) error {
						claimingJournalID = stmt.ColumnInt64(0)
						return nil
					},
				}); lookupErr != nil {
				return fmt.Errorf(
					"Apply: operation %q EffectActivityCreate: attribute ActivityID collision for %q: %w — "+
						"where: EffectActivityCreate fold, journal_activity_creations lookup after activities uniqueness failure; "+
						"when: before returning ActivityConflict; impact: operation rolled back and collision ownership is unknown; "+
						"fix: verify journal_activity_creations integrity and retry",
					in.OperationID, activityID.String(), lookupErr)
			}
			return &journal.ActivityConflict{
				ActivityID:        activityID,
				ExistingJournalID: journal.JournalID(claimingJournalID),
			}
		}
		return fmt.Errorf(
			"Apply: operation %q EffectActivityCreate: insert activities row for %q: %w — "+
				"where: EffectActivityCreate fold, activities INSERT; when: before journal_activity_creations INSERT; "+
				"impact: operation rolled back without writes to journal_activity_creations; "+
				"fix: ensure agent %q is registered before applying this operation",
			in.OperationID, activityID.String(), err, agentID.String())
	}

	// Step 2: Insert journal_activity_creations (birth record). Any failure rolls
	// the fresh activities row back with the enclosing Apply savepoint.
	if err := sqlitex.Execute(scope.conn,
		`INSERT INTO journal_activity_creations (journal_id, activity_id) VALUES (?1, ?2)`,
		&sqlitex.ExecOptions{Args: []any{jid, activityID.String()}}); err != nil {
		return fmt.Errorf(
			"Apply: operation %q EffectActivityCreate: insert journal_activity_creations for %q: %w — "+
				"where: EffectActivityCreate fold, journal_activity_creations INSERT; when: after activities INSERT; "+
				"impact: operation rolled back; "+
				"fix: verify database integrity and retry",
			in.OperationID, activityID.String(), err)
	}
	return nil
}

// lookupActivityIDForJournalRow retrieves the ActivityID for a
// journal_activity_creations row by journal_id. Used by result-slot reconstruction.
// The caller owns scope.conn and its transaction.
func (scope *connScope) lookupActivityIDForJournalRow(jid int64) (ptypes.ActivityID, bool, error) {
	var actID ptypes.ActivityID
	found := false
	if err := sqlitex.Execute(scope.conn,
		`SELECT activity_id FROM journal_activity_creations WHERE journal_id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{jid},
			ResultFunc: func(stmt *zs.Stmt) error {
				var err error
				actID, err = ptypes.ParseActivityID(stmt.ColumnText(0))
				if err != nil {
					return fmt.Errorf("decode stored activity_id for journal row %d: %w", jid, err)
				}
				found = true
				return nil
			},
		}); err != nil {
		return ptypes.ActivityID{}, false, fmt.Errorf("lookupActivityIDForJournalRow(%d): %w", jid, err)
	}
	return actID, found, nil
}
