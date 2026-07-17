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
