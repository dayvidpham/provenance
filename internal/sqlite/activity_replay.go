package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
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
		if _, err := scope.conn.ExecContext(scope.ctx, stmt); err != nil {
			return fmt.Errorf("ensureActivityCreationsSchema: %w", err)
		}
	}
	return nil
}

// foldActivityCreate folds one EffectActivityCreate inside Apply's write
// transaction. The caller owns scope.conn and the enclosing Apply savepoint.
func (scope *connScope) foldActivityCreate(in journal.OperationInput, jid int64, eff journal.Effect) error {
	recordedAt := in.RecordedAt
	if eff.RecordedAtOverride != nil {
		recordedAt = *eff.RecordedAtOverride
	}
	return foldV1ActivityCreate(scope.ctx, allocationSQLTx{conn: scope.conn}, in, jid, recordedAt, eff)
}
