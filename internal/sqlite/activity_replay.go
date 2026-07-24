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
//  2. Inserts the activities row (INSERT OR IGNORE — idempotent on exact replay).
//  3. Checks for a foreign-operation ActivityID collision in
//     journal_activity_creations; returns typed *ActivityConflict if found.
//  4. Inserts journal_activity_creations (journal_id, activity_id).
//
// The result-slot recording (insertResultSlotLocked) is the caller's
// responsibility (foldEffectLocked in operations.go), consistent with all other
// effect kinds.
//
// ActivityID collision semantics: a collision means a *different* committed
// operation owns this ActivityID. The fold rolls back the whole operation via
// the returned error. DBOS callers treat this as a terminal domain failure.

// ensureActivityCreationsSchema idempotently creates journal_activity_creations.
// Called from ensureOperationsSchema (operations.go) during DB activation.
func (db *DB) ensureActivityCreationsSchema() error {
	ddl := []string{
		// journal_activity_creations: one row per journaled Activity birth.
		//   journal_id PK FK journal          → ties birth to global journal spine
		//   activity_id UNIQUE FK activities  → ActivityID is born exactly once
		"CREATE TABLE IF NOT EXISTS journal_activity_creations (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id), activity_id TEXT NOT NULL UNIQUE REFERENCES activities(id)) STRICT",
		"CREATE INDEX IF NOT EXISTS idx_journal_activity_creations_activity ON journal_activity_creations (activity_id)",
	}
	for _, stmt := range ddl {
		if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
			return fmt.Errorf("ensureActivityCreationsSchema: %w", err)
		}
	}
	return nil
}

// foldActivityCreateLocked folds one EffectActivityCreate inside Apply's write
// transaction. The caller must hold db.mu and be inside the Apply savepoint.
func (db *DB) foldActivityCreateLocked(in journal.OperationInput, jid int64, eff journal.Effect) error {
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

	// Step 1: Insert the activities row (INSERT OR IGNORE — idempotent for
	// exact replay; the row may already exist from a prior attempt that was
	// rolled back at a later step).
	recordedAt := in.RecordedAt
	if eff.RecordedAtOverride != nil {
		recordedAt = *eff.RecordedAtOverride
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT OR IGNORE INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)`,
		&sqlitex.ExecOptions{Args: []any{
			activityID.String(), agentID.String(),
			int(eff.ActivityPhase), int(eff.ActivityStage),
			recordedAt, nil, eff.ActivityNotes,
		}}); err != nil {
		return fmt.Errorf(
			"Apply: operation %q EffectActivityCreate: insert activities row for %q: %w — "+
				"where: EffectActivityCreate fold, activities INSERT; when: before journal_activity_creations INSERT; "+
				"impact: operation rolled back without writes to journal_activity_creations; "+
				"fix: ensure agent %q is registered before applying this operation",
			in.OperationID, activityID.String(), err, agentID.String())
	}

	// Step 2: Check for a foreign-operation collision on this ActivityID.
	var claimingJournalID int64
	found := false
	if err := sqlitex.Execute(db.conn,
		`SELECT journal_id FROM journal_activity_creations WHERE activity_id = ?1`,
		&sqlitex.ExecOptions{
			Args: []any{activityID.String()},
			ResultFunc: func(stmt *zs.Stmt) error {
				claimingJournalID = stmt.ColumnInt64(0)
				found = true
				return nil
			},
		}); err != nil {
		return fmt.Errorf(
			"Apply: operation %q EffectActivityCreate: lookup existing journal_activity_creations for %q: %w",
			in.OperationID, activityID.String(), err)
	}
	if found {
		return &journal.ActivityConflict{
			ActivityID:        activityID,
			ExistingJournalID: journal.JournalID(claimingJournalID),
		}
	}

	// Step 3: Insert journal_activity_creations (birth record).
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_activity_creations (journal_id, activity_id) VALUES (?1, ?2)`,
		&sqlitex.ExecOptions{Args: []any{jid, activityID.String()}}); err != nil {
		if isUniqueViolation(err) {
			// Defense-in-depth: concurrent insert of the same activity_id.
			var existingJID int64
			_ = sqlitex.Execute(db.conn,
				`SELECT journal_id FROM journal_activity_creations WHERE activity_id = ?1`,
				&sqlitex.ExecOptions{
					Args: []any{activityID.String()},
					ResultFunc: func(stmt *zs.Stmt) error {
						existingJID = stmt.ColumnInt64(0)
						return nil
					},
				})
			if existingJID != 0 {
				return &journal.ActivityConflict{
					ActivityID:        activityID,
					ExistingJournalID: journal.JournalID(existingJID),
				}
			}
		}
		return fmt.Errorf(
			"Apply: operation %q EffectActivityCreate: insert journal_activity_creations for %q: %w — "+
				"where: EffectActivityCreate fold, journal_activity_creations INSERT; when: after activities INSERT; "+
				"impact: operation rolled back; "+
				"fix: verify database integrity and retry",
			in.OperationID, activityID.String(), err)
	}
	return nil
}

// lookupActivityIDForJournalRowLocked retrieves the ActivityID for a
// journal_activity_creations row by journal_id. Used by result-slot reconstruction.
// The caller must hold db.mu and be inside a transaction.
func (db *DB) lookupActivityIDForJournalRowLocked(jid int64) (ptypes.ActivityID, bool, error) {
	var actID ptypes.ActivityID
	found := false
	if err := sqlitex.Execute(db.conn,
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
		return ptypes.ActivityID{}, false, fmt.Errorf("lookupActivityIDForJournalRowLocked(%d): %w", jid, err)
	}
	return actID, found, nil
}
