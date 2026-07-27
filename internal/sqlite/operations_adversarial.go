package sqlite

import (
	"context"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
)

// AdversarialRemoveJournalOperationFK recreates the supported pre-FK journal
// shape while preserving all rows and other constraints. It exists only for the
// end-to-end migration and rollback corpus.
func (db *DB) AdversarialRemoveJournalOperationFK() (err error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialRemoveJournalOperationFK: lease connection: %w", err)
	}
	defer scope.release()
	if _, err = scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	defer func() { _, _ = scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=ON") }()
	if err = runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		if _, err := scope.conn.ExecContext(scope.ctx, "DROP VIEW IF EXISTS journal_attributed"); err != nil {
			return fmt.Errorf("remove journal operation FK fixture: drop view: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "CREATE TABLE journal_legacy (journal_id INTEGER PRIMARY KEY AUTOINCREMENT,kind_id INTEGER NOT NULL REFERENCES journal_kinds(id),actor_id TEXT REFERENCES agents(id),recorded_at INTEGER NOT NULL,produced_by_operation_journal_id INTEGER,CHECK ((actor_id IS NULL) = (produced_by_operation_journal_id IS NOT NULL)),CHECK (kind_id <> 1 OR produced_by_operation_journal_id IS NOT NULL)) STRICT"); err != nil {
			return fmt.Errorf("remove journal operation FK fixture: create legacy table: %w", err)
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_legacy SELECT * FROM journal"); err != nil {
			return fmt.Errorf("remove journal operation FK fixture: copy rows: %w", err)
		}
		for _, ddl := range []string{"DROP TABLE journal", "ALTER TABLE journal_legacy RENAME TO journal", "CREATE INDEX idx_journal_kind ON journal(kind_id)", "CREATE INDEX idx_journal_actor ON journal(actor_id)", "CREATE INDEX idx_journal_pboj ON journal(produced_by_operation_journal_id)", "CREATE INDEX idx_journal_recorded_at ON journal(recorded_at,journal_id)", journalAttributedViewDDL} {
			if _, err := scope.conn.ExecContext(scope.ctx, ddl); err != nil {
				return fmt.Errorf("remove journal operation FK fixture: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// AdversarialInstallV1OperationConstraint recreates the schema emitted by the
// previous release so migration tests can prove its V1-specific SQL authority is removed.
func (db *DB) AdversarialInstallV1OperationConstraint() (err error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialInstallV1OperationConstraint: lease connection: %w", err)
	}
	defer scope.release()
	if _, err = scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	defer func() { _, _ = scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=ON") }()
	err = runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		for _, ddl := range []string{"DROP TRIGGER IF EXISTS journal_operations_canonical_insert", "DROP TRIGGER IF EXISTS journal_operations_canonical_update", "CREATE TABLE journal_operations_v1 (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id),operation_id TEXT NOT NULL UNIQUE,authority_journal_id INTEGER REFERENCES journal_authorities(journal_id),command_digest BLOB NOT NULL,mutation_digest BLOB NOT NULL,mutation_encoding_version TEXT,canonical_mutation BLOB,CHECK ((mutation_encoding_version IS NULL AND canonical_mutation IS NULL) OR (mutation_encoding_version='provenance.mutation.v1' AND length(canonical_mutation)>0))) STRICT"} {
			if _, err := scope.conn.ExecContext(scope.ctx, ddl); err != nil {
				return fmt.Errorf("install V1 operation constraint fixture: %w", err)
			}
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_operations_v1 SELECT * FROM journal_operations"); err != nil {
			return fmt.Errorf("install V1 operation constraint fixture: copy rows: %w", err)
		}
		for _, ddl := range []string{"DROP TABLE journal_operations", "ALTER TABLE journal_operations_v1 RENAME TO journal_operations", "CREATE TRIGGER journal_operations_canonical_insert BEFORE INSERT ON journal_operations WHEN NEW.mutation_encoding_version IS NOT NULL AND NEW.mutation_encoding_version!='provenance.mutation.v1' BEGIN SELECT RAISE(ABORT,'V1 only'); END", "CREATE TRIGGER journal_operations_canonical_update BEFORE UPDATE OF mutation_encoding_version ON journal_operations WHEN NEW.mutation_encoding_version IS NOT NULL AND NEW.mutation_encoding_version!='provenance.mutation.v1' BEGIN SELECT RAISE(ABORT,'V1 only'); END"} {
			if _, err := scope.conn.ExecContext(scope.ctx, ddl); err != nil {
				return fmt.Errorf("install V1 operation constraint fixture: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// operations_adversarial.go holds narrow write seams that deliberately leave the
// journal in a state a production writer never would, so the adversarial proof
// corpus can drive the production VerifyIntegrity guard (§10 rule 8) and the
// rule-9 result-slot integrity check (§3.2) against real violations. Production
// paths (Apply) always write consistent rows; these seams are used only by the
// corpus and are never part of the Journal surface.

// AdversarialJournalRowTwoSubtypes writes one journal row of kind=decision and
// gives it rows in BOTH journal_decisions and journal_evidence, violating
// subtype exclusivity (§10 rule 8). Returns the offending JournalID.
func (db *DB) AdversarialJournalRowTwoSubtypes(actor journal.ActorID) (journal.JournalID, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("AdversarialJournalRowTwoSubtypes: lease connection: %w", err)
	}
	defer scope.release()
	var jid int64
	if err := runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		var err error
		jid, err = scope.insertJournalRow(journal.JournalKindDecision, actor, 0, nil)
		if err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_decisions (journal_id, decision_kind, task_id, payload) VALUES (?1, ?2, ?4, ?3)", jid, "pasture.review.vote", "{}", nil); err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_evidence (journal_id, evidence_kind, task_id, content_digest, payload) VALUES (?1, ?2, ?5, ?3, ?4)", jid, "pasture.git.commit", []byte("x"), "{}", nil); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return journal.JournalID(jid), nil
}

// AdversarialSubordinateRowCarryingActor writes a valid operation anchor and then
// a SUBORDINATE task_event row (produced_by_operation_journal_id = the anchor) that
// illegally carries a stored actor_id — the anchor-only-actor-placement violation
// (§2.1, §10 rule 5) a production writer never emits (insertJournalRow writes
// NULL on produced rows). The journal CHECK constraint normally blocks such a row,
// so this seam sets PRAGMA ignore_check_constraints around the insert to land the
// row past the CHECK, exercising the production VerifyIntegrity placement guard
// directly (mirroring the missing-subtype seam). The subordinate row is given its
// matching journal_task_events row so the ONLY violation is actor placement, not
// subtype totality. Returns the offending subordinate JournalID; the caller is
// expected to VerifyIntegrity and observe ErrActorPlacement.
func (db *DB) AdversarialSubordinateRowCarryingActor(actor journal.ActorID, task journal.TaskID) (journal.JournalID, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("AdversarialSubordinateRowCarryingActor: lease connection: %w", err)
	}
	defer scope.release()
	// Bypass the structural CHECK so the reducer-level placement guard is what
	// catches the row (the CHECK is exercised on the production write path instead).
	if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA ignore_check_constraints=ON"); err != nil {
		return 0, fmt.Errorf("AdversarialSubordinateRowCarryingActor: disable CHECK enforcement: %w", err)
	}
	defer func() { _, _ = scope.conn.ExecContext(scope.ctx, "PRAGMA ignore_check_constraints=OFF") }()

	var subordinateJID int64
	if err := runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		// Valid operation anchor (actor stored, PBOJID NULL) so the subordinate row's
		// producing-operation FK resolves.
		anchorJID, err := scope.insertJournalRow(journal.JournalKindOperation, actor, 0, nil)
		if err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_operations (journal_id, operation_id, authority_journal_id, command_digest, mutation_digest)\n\t\t VALUES (?1, ?2, ?5, ?3, ?4)", anchorJID, fmt.Sprintf("adversarial-subord-op-%d", anchorJID), []byte("c"), []byte("m"), nil); err != nil {
			return err
		}
		// Subordinate task_event row carrying an actor it must not: PBOJID set AND
		// actor_id set. insertJournalRow would write NULL, so insert directly.
		if err := scope.conn.QueryRowContext(scope.ctx, "INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id) VALUES (?1, ?2, ?3, ?4) RETURNING journal_id", int(journal.JournalKindTaskEvent), actor.String(), int64(0), anchorJID).Scan(&subordinateJID); err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, insertJournalTaskEventSQL, subordinateJID, task.String(), string(journal.EventKindTaskUpdated), "{}"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return journal.JournalID(subordinateJID), nil
}

// AdversarialSubtypeMismatchingKind writes one journal row of kind=decision that
// (in addition to its matching journal_decisions row) carries a journal_operations
// subtype row, violating discriminator agreement (§10 rule 8).
func (db *DB) AdversarialSubtypeMismatchingKind(actor journal.ActorID) (journal.JournalID, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("AdversarialSubtypeMismatchingKind: lease connection: %w", err)
	}
	defer scope.release()
	var jid int64
	if err := runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		var err error
		jid, err = scope.insertJournalRow(journal.JournalKindDecision, actor, 0, nil)
		if err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_decisions (journal_id, decision_kind, task_id, payload) VALUES (?1, ?2, ?4, ?3)", jid, "pasture.review.vote", "{}", nil); err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_operations (journal_id, operation_id, authority_journal_id, command_digest, mutation_digest)\n\t\t VALUES (?1, ?2, ?5, ?3, ?4)", jid, fmt.Sprintf("adversarial-op-%d", jid), []byte("c"), []byte("m"), nil); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return journal.JournalID(jid), nil
}

// AdversarialAuthorityDetailMismatch writes one journal row of kind=authority as a
// bootstrap authority (with its bootstrap detail) but then attaches an assignment
// transition row to it, violating authority-level discriminator agreement
// (§10 rule 8, second inheritance level). task must be an existing task.
func (db *DB) AdversarialAuthorityDetailMismatch(actor journal.ActorID, task journal.TaskID) (journal.JournalID, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("AdversarialAuthorityDetailMismatch: lease connection: %w", err)
	}
	defer scope.release()
	var jid int64
	if err := runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		var err error
		jid, err = scope.insertJournalRow(journal.JournalKindAuthority, actor, 0, nil)
		if err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, insertJournalAuthoritySQL, jid, authKindBootstrapID, fmt.Sprintf("adversarial-auth-%d", jid)); err != nil {
			return err
		}
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_authority_bootstraps (journal_id, label) VALUES (?1, ?2)", jid, "adversarial"); err != nil {
			return err
		}
		assignment := fmt.Sprintf("adversarial-episode-%d", jid)
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5)", assignment, task.String(), slotOwnerResponsibilityID, actor.String(), nil); err != nil {
			return err
		}
		// The transition points at the bootstrap authority above — the mismatch.
		if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_authority_assignment_transitions (journal_id, assignment_id, transition_id) VALUES (?1, ?2, ?3)", jid, assignment, transitionStartedID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return journal.JournalID(jid), nil
}

// AdversarialForeignResultSlotRejected runs the production rule-9 own-operation
// integrity check (§3.2, §10 rule 9) for a result slot on operation anchorOp that
// names a produced row belonging to a different operation, returning the typed
// ErrResultSlotIntegrity the reducer would raise before commit. It writes nothing.
func (db *DB) AdversarialForeignResultSlotRejected(anchorOp, foreignProduced journal.JournalID) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialForeignResultSlotRejected: lease connection: %w", err)
	}
	defer scope.release()
	return scope.requireResultSlotOwnOperation(int64(anchorOp), int64(foreignProduced))
}

// AdversarialApplyWithFault applies one operation but injects a fault immediately
// after the effect at faultAfterEffectIndex is folded, exercising §9.5 fail-closed
// atomicity through the production foldOperation path: the whole operation, including
// its anchor, must roll back with nothing committed. It writes nothing on return.
// Production Apply passes no fault hook; this seam is used only by the corpus to
// drive crash/cancellation-mid-batch and transfer-crash histories.
func (db *DB) AdversarialApplyWithFault(in journal.OperationInput, faultAfterEffectIndex int) (journal.CommittedResult, error) {
	if err := validateApplyInput(in); err != nil {
		return journal.CommittedResult{}, err
	}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return journal.CommittedResult{}, fmt.Errorf("AdversarialApplyWithFault: lease connection: %w", err)
	}
	defer scope.release()
	hook := func(i int) error {
		if i == faultAfterEffectIndex {
			return fmt.Errorf("%w: fault injected after effect %d (§9.5)", journal.ErrInjectedFault, i)
		}
		return nil
	}
	return scope.foldOperation(in, hook)
}

// AdversarialMigrateWithFault runs a legacy-baseline migration but injects a fault
// immediately after the baseline for the task at faultAfterTaskIndex commits,
// exercising §13's whole-batch fail-closed atomicity through the production
// migration path: every baseline written so far, not just the faulted task, must
// roll back atomically. It writes nothing on return.
func (db *DB) AdversarialMigrateWithFault(in journal.MigrationInput, faultAfterTaskIndex int) (journal.MigrationResult, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return journal.MigrationResult{}, fmt.Errorf("AdversarialMigrateWithFault: lease connection: %w", err)
	}
	defer scope.release()
	hook := func(i int) error {
		if i == faultAfterTaskIndex {
			return fmt.Errorf("%w: fault injected after baseline %d (§9.5, §13)", journal.ErrMigrationFault, i)
		}
		return nil
	}
	return scope.migrateWithFault(in, hook)
}

// AdversarialAddColumn adds an unreviewed extra column to a journal table,
// corrupting the external schema shape so the corpus can drive the fail-closed
// preflight (§13). Column names come from the closed corpus, never caller input.
type AdversarialColumnAddition uint8

const AdversarialAddUnreviewedTaskEventColumn AdversarialColumnAddition = 1

func (addition AdversarialColumnAddition) query() string {
	switch addition {
	case AdversarialAddUnreviewedTaskEventColumn:
		return "ALTER TABLE journal_task_events ADD COLUMN unreviewed TEXT"
	default:
		panic("unknown adversarial column addition")
	}
}

func (db *DB) AdversarialAddColumn(addition AdversarialColumnAddition) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialAddColumn: lease connection: %w", err)
	}
	defer scope.release()
	// DDL identifiers cannot be bound as parameters; table/column come from the
	// closed corpus, never caller input, so identifier interpolation is safe here.
	if _, err := scope.conn.ExecContext(scope.ctx, addition.query()); err != nil {
		return fmt.Errorf("AdversarialAddColumn: %w", err)
	}
	return nil
}

// AdversarialDropColumn removes an expected column from a journal table, the
// symmetric corruption to AdversarialAddColumn, so the corpus can drive the
// missing-expected-column preflight failure (§13).
type AdversarialColumnDrop uint8

const AdversarialDropTaskEventPayload AdversarialColumnDrop = 1

func (drop AdversarialColumnDrop) query() string {
	switch drop {
	case AdversarialDropTaskEventPayload:
		return "ALTER TABLE journal_task_events DROP COLUMN payload"
	default:
		panic("unknown adversarial column drop")
	}
}

func (db *DB) AdversarialDropColumn(drop AdversarialColumnDrop) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialDropColumn: lease connection: %w", err)
	}
	defer scope.release()
	// DDL identifiers cannot be bound; table/column come from the closed corpus.
	if _, err := scope.conn.ExecContext(scope.ctx, drop.query()); err != nil {
		return fmt.Errorf("AdversarialDropColumn: %w", err)
	}
	return nil
}

// AdversarialDropTable drops a journal table entirely (a corrupted or partially
// provisioned deployment), so the corpus can drive the missing-table preflight
// failure (§13). Foreign-key enforcement is toggled off around the drop so a table
// referenced by empty child tables can be removed; the database is left in the
// deliberately corrupt state the corpus then opens against.
type AdversarialTableDrop uint8

const AdversarialDropJournalTable AdversarialTableDrop = 1

func (drop AdversarialTableDrop) query() string {
	switch drop {
	case AdversarialDropJournalTable:
		return "DROP TABLE journal"
	default:
		panic("unknown adversarial table drop")
	}
}

func (db *DB) AdversarialDropTable(drop AdversarialTableDrop) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialDropTable: lease connection: %w", err)
	}
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("AdversarialDropTable: disable FK: %w", err)
	}
	defer func() { _, _ = scope.conn.ExecContext(scope.ctx, "PRAGMA foreign_keys=ON") }()
	// DDL identifier cannot be bound; table comes from the closed corpus.
	if _, err := scope.conn.ExecContext(scope.ctx, drop.query()); err != nil {
		return fmt.Errorf("AdversarialDropTable: %w", err)
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
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return journal.MigrationResult{}, fmt.Errorf("AdversarialMigrateFabricatedEndedTimestamp: lease connection: %w", err)
	}
	defer scope.release()
	if err := scope.preflightSchema(); err != nil {
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
type AdversarialTableAddition uint8

const AdversarialAddUnreviewedJournalTable AdversarialTableAddition = 1

func (addition AdversarialTableAddition) query() string {
	switch addition {
	case AdversarialAddUnreviewedJournalTable:
		return "CREATE TABLE journal_unreviewed (journal_id INTEGER PRIMARY KEY) STRICT"
	default:
		panic("unknown adversarial table addition")
	}
}

func (db *DB) AdversarialAddTable(addition AdversarialTableAddition) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialAddTable: lease connection: %w", err)
	}
	defer scope.release()
	// DDL identifier cannot be bound as a parameter; table comes from the closed
	// corpus, never caller input, so identifier interpolation is safe here.
	if _, err := scope.conn.ExecContext(scope.ctx, addition.query()); err != nil {
		return fmt.Errorf("AdversarialAddTable: %w", err)
	}
	return nil
}

// AdversarialProjectionField names the projection column an out-of-band corruption
// seam writes past the reducer, so the corpus stays a closed set rather than
// accepting arbitrary column names.
type AdversarialProjectionField uint8

const (
	AdversarialFieldOwner AdversarialProjectionField = iota + 1
	AdversarialFieldStatus
	AdversarialFieldWatermark
)

// AdversarialCorruptTaskProjection writes directly to a task's stored projection
// column, BYPASSING the shared reducer — the out-of-band corruption §15's
// convergence check exists to detect (analogous to the AdversarialAddColumn schema
// seams). It is the seam that proves ReplayProjections' ProjectionDivergenceError
// actually fires: a production writer only ever advances tasks.* through
// projectJournalRow, so this deliberately installs a value no ordered journal
// history would derive. The field comes from the closed AdversarialProjectionField
// set, never free-form caller input.
func (db *DB) AdversarialCorruptTaskProjection(task journal.TaskID, field AdversarialProjectionField, value any) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialCorruptTaskProjection: lease connection: %w", err)
	}
	defer scope.release()
	switch field {
	case AdversarialFieldOwner, AdversarialFieldStatus, AdversarialFieldWatermark:
	default:
		return fmt.Errorf("AdversarialCorruptTaskProjection: unknown projection field %q (closed set: owner_id/status_id/last_journal_id)", field)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, field.query(), value, task.String()); err != nil {
		return fmt.Errorf("AdversarialCorruptTaskProjection %q field %d: %w", task, field, err)
	}
	return nil
}

func (field AdversarialProjectionField) query() string {
	switch field {
	case AdversarialFieldOwner:
		return "UPDATE tasks SET owner_id=?1 WHERE id=?2"
	case AdversarialFieldStatus:
		return "UPDATE tasks SET status_id=?1 WHERE id=?2"
	case AdversarialFieldWatermark:
		return "UPDATE tasks SET last_journal_id=?1 WHERE id=?2"
	default:
		panic("unknown adversarial projection field")
	}
}

// AdversarialCorruptCommentBody rewrites a committed comment's body directly in the
// comments projection, BYPASSING the shared reducer — the out-of-band content corruption
// the §15 FULL-TUPLE convergence check exists to detect on a fold-derived comment (a
// key-only check reads the tampered body back unchanged and falsely reports convergence).
// A production writer only ever materializes comments.body through
// projectMutationFamilyRow from the journaled comment payload, so this installs a
// body no ordered journal history would derive. The comment id comes from the caller's
// committed row; body is the corpus's chosen tamper value, never a column identifier.
func (db *DB) AdversarialCorruptCommentBody(commentID, body string) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialCorruptCommentBody: lease connection: %w", err)
	}
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE comments SET body = ?1 WHERE id = ?2", body, commentID); err != nil {
		return fmt.Errorf("AdversarialCorruptCommentBody %q: %w", commentID, err)
	}
	return nil
}

// AdversarialInsertSpuriousAttribution writes a task_attributions edge directly,
// BYPASSING the shared reducer, so the corpus can prove ReplayProjections detects a
// spurious attribution edge no ordered journal history would derive (§8.2, §15).
func (db *DB) AdversarialInsertSpuriousAttribution(task journal.TaskID, actor journal.ActorID, jid journal.JournalID) error {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return fmt.Errorf("AdversarialInsertSpuriousAttribution: lease connection: %w", err)
	}
	defer scope.release()
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT OR REPLACE INTO task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)", task.String(), actor.String(), int64(jid)); err != nil {
		return fmt.Errorf("AdversarialInsertSpuriousAttribution %q/%q: %w", task, actor, err)
	}
	return nil
}

// AdversarialResolveOperationIDInsertRace drives the §9.6-bullet-2 race-translation
// path (resolveOperationIDInsertRace) directly. Before pool migration that
// path was unreachable — Apply's §9.4 lookup always observed a concurrent
// writer's committed row before reaching the anchor insert — so this seam invokes
// the translation the reducer runs when the anchor insert loses the UNIQUE race:
// it re-reads the now-committed row for in.OperationID and returns the typed
// idempotent result or typed CommittedConflict the caller is promised, never a raw
// SQLite constraint error. It writes nothing. Callers pass an input whose
// OperationID is already committed (simulating the winner's row).
func (db *DB) AdversarialResolveOperationIDInsertRace(in journal.OperationInput) (journal.CommittedResult, error) {
	prepared, err := journal.Canonicalize(in)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	callerMutationDigest := append([]byte(nil), in.MutationDigest...)
	in.MutationDigest = prepared.DerivedDigest()
	in.Effects = prepared.NormalizedEffects()
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return journal.CommittedResult{}, fmt.Errorf("AdversarialResolveOperationIDInsertRace: lease connection: %w", err)
	}
	defer scope.release()
	return scope.resolveOperationIDInsertRace(in, callerMutationDigest)
}

// AdversarialCyclicParentChain seeds a CORRUPT cyclic parent-citation chain that
// the production start-effect citation guard (§14.5) would reject, bypassing that
// guard by writing directly: episode X on taskX and episode Y on taskY each carry
// an active started transition, and their parent_assignment_id columns point at
// each other (X→Y→X). It also seeds an unrelated active episode Z on taskZ whose
// started transition is the authority the corpus queries. It returns Z's authority
// JournalID, taskX (the governance target), and a beforeJID strictly greater than
// every seeded journal row so the whole chain is live. The corpus then asks whether
// Z's authority governs taskX; the §14.5 governance walk must fail closed with
// ErrCorruptParentChain (bounded, visited-tracked traversal) rather than looping.
func (db *DB) AdversarialCyclicParentChain(actor journal.ActorID, taskX, taskY, taskZ journal.TaskID) (zAuth journal.JournalID, target journal.TaskID, beforeJID journal.JournalID, err error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, journal.TaskID{}, 0, fmt.Errorf("AdversarialCyclicParentChain: lease connection: %w", err)
	}
	defer scope.release()
	var jz, maxJID int64
	if err := runScopedTransaction(scope.ctx, scope.conn, "BEGIN", func() error {
		if _, err := scope.seedActiveEpisode(actor, taskX, "cyclic-parent-X", nil); err != nil {
			return err
		}
		if _, err := scope.seedActiveEpisode(actor, taskY, "cyclic-parent-Y", "cyclic-parent-X"); err != nil {
			return err
		}
		// Close the cycle: X.parent = Y. A production start effect can never write this
		// (its cycle guard rejects it); the direct UPDATE is the corruption under test.
		if _, err := scope.conn.ExecContext(scope.ctx, "UPDATE journal_authority_assignment_episodes SET parent_assignment_id = ?1 WHERE assignment_id = ?2", "cyclic-parent-Y", "cyclic-parent-X"); err != nil {
			return err
		}
		var err error
		jz, err = scope.seedActiveEpisode(actor, taskZ, "cyclic-parent-Z", nil)
		if err != nil {
			return err
		}
		if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COALESCE(MAX(journal_id), ?1) FROM journal", 0).Scan(&maxJID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return 0, journal.TaskID{}, 0, err
	}
	return journal.JournalID(jz), taskX, journal.JournalID(maxJID + 1), nil
}

// seedActiveEpisode writes one active episode (a started transition + its
// journal_authorities row + the episode row) on `task`, optionally citing a
// parent, and returns the started transition's authority JournalID. It is the
// shared adversarial builder for the §14.5 cycle seam; production episodes are
// only ever written through foldAssignmentStart.
func (scope *connScope) seedActiveEpisode(actor journal.ActorID, task journal.TaskID, assignment journal.AssignmentID, parent any) (int64, error) {
	jid, err := scope.insertJournalRow(journal.JournalKindAuthority, actor, 0, nil)
	if err != nil {
		return 0, err
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id, parent_assignment_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?6, ?5)", string(assignment), task.String(), slotOwnerResponsibilityID, actor.String(), parent, nil); err != nil {
		return 0, fmt.Errorf("seed episode %q: %w", assignment, err)
	}
	if err := scope.insertAuthorityAssignmentTransition(jid, assignment, transitionStartedID); err != nil {
		return 0, err
	}
	return jid, nil
}
