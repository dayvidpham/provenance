package sqlite

import (
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	"zombiezen.com/go/sqlite/sqlitex"
)

// operations_adversarial.go holds narrow write seams that deliberately leave the
// journal in a state a production writer never would, so the adversarial proof
// corpus can drive the production VerifyIntegrity guard (§10 rule 8) and the
// rule-9 result-slot integrity check (§3.2) against real violations. Production
// paths (Apply) always write consistent rows; these seams are used only by the
// corpus and are never part of the JournalAPI surface.

// AdversarialJournalRowTwoSubtypes writes one journal row of kind=decision and
// gives it rows in BOTH journal_decisions and journal_evidence, violating
// subtype exclusivity (§10 rule 8). Returns the offending JournalID.
func (db *DB) AdversarialJournalRowTwoSubtypes(actor journal.ActorID) (journal.JournalID, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var txErr error
	endTx := sqlitex.Transaction(db.conn)
	defer endTx(&txErr)
	jid, err := db.insertJournalRowLocked(journal.JournalKindDecision, actor, 0, nil)
	if err != nil {
		txErr = err
		return 0, txErr
	}
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_decisions (JournalID, decision_kind, task_id, payload) VALUES (?1, 'pasture.review.vote', NULL, '{}')`,
		&sqlitex.ExecOptions{Args: []any{jid}}); txErr != nil {
		return 0, txErr
	}
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_evidence (JournalID, evidence_kind, task_id, content_digest, payload) VALUES (?1, 'pasture.git.commit', NULL, ?2, '{}')`,
		&sqlitex.ExecOptions{Args: []any{jid, []byte("x")}}); txErr != nil {
		return 0, txErr
	}
	return journal.JournalID(jid), nil
}

// AdversarialSubordinateRowCarryingActor writes a valid operation anchor and then
// a SUBORDINATE task_event row (produced_by_operation_journal_id = the anchor) that
// illegally carries a stored actor_id — the anchor-only-actor-placement violation
// (§2.1, §10 rule 5) a production writer never emits (insertJournalRowLocked writes
// NULL on produced rows). The journal CHECK constraint normally blocks such a row,
// so this seam sets PRAGMA ignore_check_constraints around the insert to land the
// row past the CHECK, exercising the production VerifyIntegrity placement guard
// directly (mirroring the missing-subtype seam). The subordinate row is given its
// matching journal_task_events row so the ONLY violation is actor placement, not
// subtype totality. Returns the offending subordinate JournalID; the caller is
// expected to VerifyIntegrity and observe ErrActorPlacement.
func (db *DB) AdversarialSubordinateRowCarryingActor(actor journal.ActorID, task journal.TaskID) (journal.JournalID, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	// Bypass the structural CHECK so the reducer-level placement guard is what
	// catches the row (the CHECK is exercised on the production write path instead).
	if err := sqlitex.ExecuteTransient(db.conn, `PRAGMA ignore_check_constraints=ON`, nil); err != nil {
		return 0, fmt.Errorf("AdversarialSubordinateRowCarryingActor: disable CHECK enforcement: %w", err)
	}
	defer func() { _ = sqlitex.ExecuteTransient(db.conn, `PRAGMA ignore_check_constraints=OFF`, nil) }()

	var txErr error
	endTx := sqlitex.Transaction(db.conn)
	defer endTx(&txErr)

	// Valid operation anchor (actor stored, PBOJID NULL) so the subordinate row's
	// producing-operation FK resolves.
	anchorJID, err := db.insertJournalRowLocked(journal.JournalKindOperation, actor, 0, nil)
	if err != nil {
		txErr = err
		return 0, txErr
	}
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_operations (JournalID, operation_id, authority_journal_id, command_digest, mutation_digest)
		 VALUES (?1, ?2, NULL, ?3, ?4)`,
		&sqlitex.ExecOptions{Args: []any{anchorJID, fmt.Sprintf("adversarial-subord-op-%d", anchorJID), []byte("c"), []byte("m")}}); txErr != nil {
		return 0, txErr
	}
	// Subordinate task_event row carrying an actor it must not: PBOJID set AND
	// actor_id set. insertJournalRowLocked would write NULL, so insert directly.
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id) VALUES (?1, ?2, 0, ?3)`,
		&sqlitex.ExecOptions{Args: []any{int(journal.JournalKindTaskEvent), actor.String(), anchorJID}}); txErr != nil {
		return 0, txErr
	}
	subordinateJID := db.conn.LastInsertRowID()
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_task_events (JournalID, task_id, event_kind, payload) VALUES (?1, ?2, 'provenance.task.updated', '{}')`,
		&sqlitex.ExecOptions{Args: []any{subordinateJID, task.String()}}); txErr != nil {
		return 0, txErr
	}
	return journal.JournalID(subordinateJID), nil
}

// AdversarialSubtypeMismatchingKind writes one journal row of kind=decision that
// (in addition to its matching journal_decisions row) carries a journal_operations
// subtype row, violating discriminator agreement (§10 rule 8).
func (db *DB) AdversarialSubtypeMismatchingKind(actor journal.ActorID) (journal.JournalID, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var txErr error
	endTx := sqlitex.Transaction(db.conn)
	defer endTx(&txErr)
	jid, err := db.insertJournalRowLocked(journal.JournalKindDecision, actor, 0, nil)
	if err != nil {
		txErr = err
		return 0, txErr
	}
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_decisions (JournalID, decision_kind, task_id, payload) VALUES (?1, 'pasture.review.vote', NULL, '{}')`,
		&sqlitex.ExecOptions{Args: []any{jid}}); txErr != nil {
		return 0, txErr
	}
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_operations (JournalID, operation_id, authority_journal_id, command_digest, mutation_digest)
		 VALUES (?1, ?2, NULL, ?3, ?4)`,
		&sqlitex.ExecOptions{Args: []any{jid, fmt.Sprintf("adversarial-op-%d", jid), []byte("c"), []byte("m")}}); txErr != nil {
		return 0, txErr
	}
	return journal.JournalID(jid), nil
}

// AdversarialAuthorityDetailMismatch writes one journal row of kind=authority as a
// bootstrap authority (with its bootstrap detail) but then attaches an assignment
// transition row to it, violating authority-level discriminator agreement
// (§10 rule 8, second inheritance level). task must be an existing task.
func (db *DB) AdversarialAuthorityDetailMismatch(actor journal.ActorID, task journal.TaskID) (journal.JournalID, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var txErr error
	endTx := sqlitex.Transaction(db.conn)
	defer endTx(&txErr)
	jid, err := db.insertJournalRowLocked(journal.JournalKindAuthority, actor, 0, nil)
	if err != nil {
		txErr = err
		return 0, txErr
	}
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_authorities (JournalID, authority_kind_id, operation_authority_id) VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{jid, authKindBootstrapID, fmt.Sprintf("adversarial-auth-%d", jid)}}); txErr != nil {
		return 0, txErr
	}
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_authority_bootstraps (JournalID, label) VALUES (?1, 'adversarial')`,
		&sqlitex.ExecOptions{Args: []any{jid}}); txErr != nil {
		return 0, txErr
	}
	assignment := fmt.Sprintf("adversarial-episode-%d", jid)
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id)
		 VALUES (?1, ?2, ?3, ?4, NULL)`,
		&sqlitex.ExecOptions{Args: []any{assignment, task.String(), slotOwnerResponsibilityID, actor.String()}}); txErr != nil {
		return 0, txErr
	}
	// The transition points at the bootstrap authority above — the mismatch.
	if txErr = sqlitex.Execute(db.conn,
		`INSERT INTO journal_authority_assignment_transitions (JournalID, assignment_id, transition_id) VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{jid, assignment, transitionStartedID}}); txErr != nil {
		return 0, txErr
	}
	return journal.JournalID(jid), nil
}

// AdversarialForeignResultSlotRejected runs the production rule-9 own-operation
// integrity check (§3.2, §10 rule 9) for a result slot on operation anchorOp that
// names a produced row belonging to a different operation, returning the typed
// ErrResultSlotIntegrity the reducer would raise before commit. It writes nothing.
func (db *DB) AdversarialForeignResultSlotRejected(anchorOp, foreignProduced journal.JournalID) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.requireResultSlotOwnOperationLocked(int64(anchorOp), int64(foreignProduced))
}

// AdversarialApplyWithFault applies one operation but injects a fault immediately
// after the effect at faultAfterEffectIndex is folded, exercising §9.5 fail-closed
// atomicity through the production applyLocked path: the whole operation, including
// its anchor, must roll back with nothing committed. It writes nothing on return.
// Production Apply passes no fault hook; this seam is used only by the corpus to
// drive crash/cancellation-mid-batch and transfer-crash histories.
func (db *DB) AdversarialApplyWithFault(in journal.OperationInput, faultAfterEffectIndex int) (journal.CommittedResult, error) {
	if err := validateApplyInput(in); err != nil {
		return journal.CommittedResult{}, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	hook := func(i int) error {
		if i == faultAfterEffectIndex {
			return fmt.Errorf("%w: fault injected after effect %d (§9.5)", journal.ErrInjectedFault, i)
		}
		return nil
	}
	return db.applyLocked(in, hook)
}

// AdversarialMigrateWithFault runs a legacy-baseline migration but injects a fault
// immediately after the baseline for the task at faultAfterTaskIndex commits,
// exercising §13's whole-batch fail-closed atomicity through the production
// migration path: every baseline written so far, not just the faulted task, must
// roll back atomically. It writes nothing on return.
func (db *DB) AdversarialMigrateWithFault(in journal.MigrationInput, faultAfterTaskIndex int) (journal.MigrationResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	hook := func(i int) error {
		if i == faultAfterTaskIndex {
			return fmt.Errorf("%w: fault injected after baseline %d (§9.5, §13)", journal.ErrMigrationFault, i)
		}
		return nil
	}
	return db.migrateLockedWithFault(in, hook)
}

// AdversarialAddColumn adds an unreviewed extra column to a journal table,
// corrupting the external schema shape so the corpus can drive the fail-closed
// preflight (§13). Column names come from the closed corpus, never caller input.
func (db *DB) AdversarialAddColumn(table, column string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	// DDL identifiers cannot be bound as parameters; table/column come from the
	// closed corpus, never caller input, so identifier interpolation is safe here.
	stmt := fmt.Sprintf(`ALTER TABLE %q ADD COLUMN %q TEXT`, table, column)
	if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
		return fmt.Errorf("AdversarialAddColumn %q.%q: %w", table, column, err)
	}
	return nil
}

// AdversarialDropColumn removes an expected column from a journal table, the
// symmetric corruption to AdversarialAddColumn, so the corpus can drive the
// missing-expected-column preflight failure (§13).
func (db *DB) AdversarialDropColumn(table, column string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	// DDL identifiers cannot be bound; table/column come from the closed corpus.
	stmt := fmt.Sprintf(`ALTER TABLE %q DROP COLUMN %q`, table, column)
	if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
		return fmt.Errorf("AdversarialDropColumn %q.%q: %w", table, column, err)
	}
	return nil
}

// AdversarialDropTable drops a journal table entirely (a corrupted or partially
// provisioned deployment), so the corpus can drive the missing-table preflight
// failure (§13). Foreign-key enforcement is toggled off around the drop so a table
// referenced by empty child tables can be removed; the database is left in the
// deliberately corrupt state the corpus then opens against.
func (db *DB) AdversarialDropTable(table string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := sqlitex.ExecuteTransient(db.conn, `PRAGMA foreign_keys=OFF`, nil); err != nil {
		return fmt.Errorf("AdversarialDropTable %q: disable FK: %w", table, err)
	}
	defer func() { _ = sqlitex.ExecuteTransient(db.conn, `PRAGMA foreign_keys=ON`, nil) }()
	// DDL identifier cannot be bound; table comes from the closed corpus.
	stmt := fmt.Sprintf(`DROP TABLE %q`, table)
	if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
		return fmt.Errorf("AdversarialDropTable %q: %w", table, err)
	}
	return nil
}

// AdversarialMigrateFabricatedEndedTimestamp attempts to migrate a single legacy
// task while fabricating the ended transition's RecordedAt with the migration's own
// wall-clock time instead of the legacy closed_at/updated_at, exercising regression
// (g)'s honest-timestamp guard (§13). The production guard rejects it with
// ErrDishonestMigrationTimestamp before any write; nothing is committed. The input
// must carry exactly one closed, owned legacy task.
func (db *DB) AdversarialMigrateFabricatedEndedTimestamp(in journal.MigrationInput, wallClockNanos int64) (journal.MigrationResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.preflightSchemaLocked(); err != nil {
		return journal.MigrationResult{}, err
	}
	if len(in.Legacy) != 1 {
		return journal.MigrationResult{}, fmt.Errorf("AdversarialMigrateFabricatedEndedTimestamp: expected exactly one legacy task, got %d", len(in.Legacy))
	}
	lt := in.Legacy[0]
	owner, ok := in.Owners[lt.RawOwner]
	if !ok {
		return journal.MigrationResult{}, fmt.Errorf("AdversarialMigrateFabricatedEndedTimestamp: owner %q is not registered", lt.RawOwner)
	}
	op := baselineOperation(in, lt, &owner)
	// Fabricate: overwrite the ended transition's RecordedAt with a wall-clock read
	// that traces to no legacy column — the exact dishonesty regression (g) forbids.
	fabricated := false
	for i := range op.Effects {
		if op.Effects[i].Sort == journal.EffectAssignmentEnd {
			wc := wallClockNanos
			op.Effects[i].RecordedAtOverride = &wc
			fabricated = true
		}
	}
	if !fabricated {
		return journal.MigrationResult{}, fmt.Errorf("AdversarialMigrateFabricatedEndedTimestamp: legacy task %s has no ended transition to fabricate", lt.ID.String())
	}
	if err := assertHonestBaselineTimestamps(lt, op); err != nil {
		return journal.MigrationResult{}, err // the guard rejects before any write
	}
	// Unreachable: the guard above always rejects a fabricated wall-clock stamp.
	return journal.MigrationResult{}, fmt.Errorf("AdversarialMigrateFabricatedEndedTimestamp: fabricated timestamp was not rejected by the honest-timestamp guard")
}

// AdversarialAddTable creates an unrecognized extra journal-spine-named table
// (a stray migration artifact, a manually-created table, or a future schema
// version's table left after a downgrade), corrupting the external schema shape so
// the corpus can drive the fail-closed extra-table preflight (§13). The table name
// comes from the closed corpus, never caller input.
func (db *DB) AdversarialAddTable(table string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	// DDL identifier cannot be bound as a parameter; table comes from the closed
	// corpus, never caller input, so identifier interpolation is safe here.
	stmt := fmt.Sprintf(`CREATE TABLE %q (JournalID INTEGER PRIMARY KEY) STRICT`, table)
	if err := sqlitex.ExecuteTransient(db.conn, stmt, nil); err != nil {
		return fmt.Errorf("AdversarialAddTable %q: %w", table, err)
	}
	return nil
}

// AdversarialProjectionField names the projection column an out-of-band corruption
// seam writes past the reducer, so the corpus stays a closed set rather than
// accepting arbitrary column names.
type AdversarialProjectionField string

const (
	AdversarialFieldOwner     AdversarialProjectionField = "owner_id"
	AdversarialFieldStatus    AdversarialProjectionField = "status_id"
	AdversarialFieldWatermark AdversarialProjectionField = "last_journal_id"
)

// AdversarialCorruptTaskProjection writes directly to a task's stored projection
// column, BYPASSING the shared reducer — the out-of-band corruption §15's
// convergence check exists to detect (analogous to the AdversarialAddColumn schema
// seams). It is the seam that proves ReplayProjections' ProjectionDivergenceError
// actually fires: a production writer only ever advances tasks.* through
// projectJournalRowLocked, so this deliberately installs a value no ordered journal
// history would derive. The field comes from the closed AdversarialProjectionField
// set, never free-form caller input.
func (db *DB) AdversarialCorruptTaskProjection(task journal.TaskID, field AdversarialProjectionField, value any) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	switch field {
	case AdversarialFieldOwner, AdversarialFieldStatus, AdversarialFieldWatermark:
	default:
		return fmt.Errorf("AdversarialCorruptTaskProjection: unknown projection field %q (closed set: owner_id/status_id/last_journal_id)", field)
	}
	// The column name is one of the closed constants above, never caller input.
	stmt := fmt.Sprintf(`UPDATE tasks SET %s = ?1 WHERE id = ?2`, string(field))
	if err := sqlitex.Execute(db.conn, stmt,
		&sqlitex.ExecOptions{Args: []any{value, task.String()}}); err != nil {
		return fmt.Errorf("AdversarialCorruptTaskProjection %q.%s: %w", task, field, err)
	}
	return nil
}

// AdversarialInsertSpuriousAttribution writes a task_attributions edge directly,
// BYPASSING the shared reducer, so the corpus can prove ReplayProjections detects a
// spurious attribution edge no ordered journal history would derive (§8.2, §15).
func (db *DB) AdversarialInsertSpuriousAttribution(task journal.TaskID, actor journal.ActorID, jid journal.JournalID) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := sqlitex.Execute(db.conn,
		`INSERT OR REPLACE INTO task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{task.String(), actor.String(), int64(jid)}}); err != nil {
		return fmt.Errorf("AdversarialInsertSpuriousAttribution %q/%q: %w", task, actor, err)
	}
	return nil
}

// AdversarialResolveOperationIDInsertRace drives the §9.6-bullet-2 race-translation
// path (resolveOperationIDInsertRaceLocked) directly. Under the in-process db.mu
// that path is unreachable — Apply's §9.4 lookup always observes a concurrent
// writer's committed row before reaching the anchor insert — so this seam invokes
// the translation the reducer runs when the anchor insert loses the UNIQUE race:
// it re-reads the now-committed row for in.OperationID and returns the typed
// idempotent result or typed CommittedConflict the caller is promised, never a raw
// SQLite constraint error. It writes nothing. Callers pass an input whose
// OperationID is already committed (simulating the winner's row).
func (db *DB) AdversarialResolveOperationIDInsertRace(in journal.OperationInput) (journal.CommittedResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.resolveOperationIDInsertRaceLocked(in)
}
