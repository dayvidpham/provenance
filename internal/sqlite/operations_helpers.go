package sqlite

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// operations_helpers.go holds the low-level reducer steps and read-path
// reconstruction that Apply (operations.go) composes. Every function assumes the
// DB mutex is held and runs inside Apply's single transaction (§9.5), so it
// observes the state produced by all earlier effects of the same operation
// (§9.3). LookupCommitted and the pure authorization predicate are the public
// read surfaces.

// ---------------------------------------------------------------------------
// Row inserts
// ---------------------------------------------------------------------------

func (db *DB) insertJournalRowLocked(kind journal.JournalKind, actor journal.ActorID, recordedAt int64, pboj *int64) (int64, error) {
	// Anchor-only actor placement (§2.1, §10 rule 5): a subordinate row (pboj set —
	// produced by an operation) stores actor_id NULL and derives its committing actor
	// from its anchor (§8.5); only an anchor row (pboj nil) stores the actor. The
	// journal CHECK constraint enforces this same invariant structurally.
	var pbojArg, actorArg any
	if pboj != nil {
		pbojArg = *pboj
	} else {
		actorArg = actor.String()
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id) VALUES (?1, ?2, ?3, ?4)`,
		&sqlitex.ExecOptions{Args: []any{int(kind), actorArg, recordedAt, pbojArg}}); err != nil {
		return 0, fmt.Errorf("insert journal row (kind %s): %w", kind, err)
	}
	return db.conn.LastInsertRowID(), nil
}

func (db *DB) insertOperationRowLocked(anchor int64, in journal.OperationInput) error {
	var authArg any
	if in.AuthorityJournalID != nil {
		authArg = int64(*in.AuthorityJournalID)
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_operations (journal_id, operation_id, authority_journal_id, command_digest, mutation_digest)
		 VALUES (?1, ?2, ?3, ?4, ?5)`,
		&sqlitex.ExecOptions{Args: []any{anchor, string(in.OperationID), authArg, in.CommandDigest, in.MutationDigest}}); err != nil {
		return fmt.Errorf("insert journal_operations for %q: %w", in.OperationID, err)
	}
	return nil
}

func (db *DB) insertAuthorityAssignmentTransitionLocked(jid int64, assignment journal.AssignmentID, transitionID int) error {
	opAuthID := fmt.Sprintf("authority--assignment--%d", jid)
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_authorities (journal_id, authority_kind_id, operation_authority_id) VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{jid, authKindAssignmentID, opAuthID}}); err != nil {
		return fmt.Errorf("insert journal_authorities (assignment): %w", err)
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_authority_assignment_transitions (journal_id, assignment_id, transition_id) VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{jid, string(assignment), transitionID}}); err != nil {
		return fmt.Errorf("insert assignment transition (%s): %w", journal.AssignmentTransition(transitionID), err)
	}
	return nil
}

func (db *DB) insertResultSlotLocked(anchor int64, slot journal.ResultSlotID, producedJID int64) error {
	// rule 9 own-operation integrity (§3.2, §10 rule 9): the produced row must
	// have been produced by this same operation. Always holds on the normal path
	// (producedJID is a row this operation just inserted), enforced anyway.
	if err := db.requireResultSlotOwnOperationLocked(anchor, producedJID); err != nil {
		return err
	}
	if err := sqlitex.Execute(db.conn,
		`INSERT INTO journal_operation_result_slots (journal_id, result_slot_id, produced_journal_id) VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{anchor, string(slot), producedJID}}); err != nil {
		return fmt.Errorf("insert result slot %q: %w", slot, err)
	}
	return nil
}

func (db *DB) requireResultSlotOwnOperationLocked(anchor, producedJID int64) error {
	var producer int64
	var isNull = true
	if err := sqlitex.Execute(db.conn,
		`SELECT produced_by_operation_journal_id FROM journal WHERE journal_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{producedJID}, ResultFunc: func(stmt *zs.Stmt) error {
			if stmt.ColumnType(0) != zs.TypeNull {
				producer = stmt.ColumnInt64(0)
				isNull = false
			}
			return nil
		}}); err != nil {
		return fmt.Errorf("rule-9 check: load produced row %d: %w", producedJID, err)
	}
	if isNull || producer != anchor {
		return fmt.Errorf(
			"%w: result slot on operation anchor %d references produced row %d whose own producing "+
				"operation is %d — where: result-slot fold (§3.2, §10 rule 9); when: before commit; "+
				"impact: nothing committed; fix: a result slot may only map to a row its own operation produced",
			journal.ErrResultSlotIntegrity, anchor, producedJID, producer)
	}
	return nil
}

func (db *DB) insertAttributionLocked(task journal.TaskID, actor journal.ActorID, jid int64) error {
	if err := sqlitex.Execute(db.conn,
		`INSERT OR IGNORE INTO task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)`,
		&sqlitex.ExecOptions{Args: []any{task.String(), actor.String(), jid}}); err != nil {
		return fmt.Errorf("update task_attributions: %w", err)
	}
	return nil
}

func (db *DB) advanceWatermarkLocked(task journal.TaskID, jid int64) error {
	if err := sqlitex.Execute(db.conn,
		`UPDATE tasks SET last_journal_id = ?1 WHERE id = ?2`,
		&sqlitex.ExecOptions{Args: []any{jid, task.String()}}); err != nil {
		return fmt.Errorf("advance tasks.last_journal_id: %w", err)
	}
	return nil
}

// recomputeTaskOwnerLocked materializes the owner-responsibility projection
// (§8.1): tasks.owner_id becomes the current active owner episode's occupant, or
// NULL when none is active. The watermark advances to jid.
func (db *DB) recomputeTaskOwnerLocked(task journal.TaskID, jid int64) error {
	var owner any
	if err := sqlitex.Execute(db.conn,
		`SELECT e.actor_id FROM journal_authority_assignment_episodes e
		 JOIN journal_authority_assignment_transitions started
		   ON started.assignment_id = e.assignment_id AND started.transition_id = ?2
		 WHERE e.task_id = ?1 AND e.slot_id = ?3
		   AND NOT EXISTS (SELECT 1 FROM journal_authority_assignment_transitions ended
		                   WHERE ended.assignment_id = e.assignment_id AND ended.transition_id = ?4)
		 ORDER BY started.journal_id DESC LIMIT 1`,
		&sqlitex.ExecOptions{
			Args:       []any{task.String(), transitionStartedID, slotOwnerResponsibilityID, transitionEndedID},
			ResultFunc: func(stmt *zs.Stmt) error { owner = stmt.ColumnText(0); return nil },
		}); err != nil {
		return fmt.Errorf("recompute task owner: %w", err)
	}
	if err := sqlitex.Execute(db.conn,
		`UPDATE tasks SET owner_id = ?1, last_journal_id = ?2 WHERE id = ?3`,
		&sqlitex.ExecOptions{Args: []any{owner, jid, task.String()}}); err != nil {
		return fmt.Errorf("update tasks owner projection: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Episode/authority state queries (§4.4, §14)
// ---------------------------------------------------------------------------

func (db *DB) episodeStartedLocked(assignment journal.AssignmentID) (bool, error) {
	return db.transitionExistsLocked(assignment, transitionStartedID)
}

func (db *DB) episodeEndedLocked(assignment journal.AssignmentID) (ended bool, exists bool, err error) {
	exists, err = db.episodeExistsLocked(assignment)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, false, nil
	}
	ended, err = db.transitionExistsLocked(assignment, transitionEndedID)
	return ended, true, err
}

func (db *DB) episodeExistsLocked(assignment journal.AssignmentID) (bool, error) {
	found := false
	if err := sqlitex.Execute(db.conn,
		`SELECT 1 FROM journal_authority_assignment_episodes WHERE assignment_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{string(assignment)}, ResultFunc: func(*zs.Stmt) error { found = true; return nil }}); err != nil {
		return false, fmt.Errorf("episode exists %q: %w", assignment, err)
	}
	return found, nil
}

func (db *DB) transitionExistsLocked(assignment journal.AssignmentID, transitionID int) (bool, error) {
	found := false
	if err := sqlitex.Execute(db.conn,
		`SELECT 1 FROM journal_authority_assignment_transitions WHERE assignment_id = ?1 AND transition_id = ?2`,
		&sqlitex.ExecOptions{Args: []any{string(assignment), transitionID}, ResultFunc: func(*zs.Stmt) error { found = true; return nil }}); err != nil {
		return false, fmt.Errorf("transition exists %q/%d: %w", assignment, transitionID, err)
	}
	return found, nil
}

func (db *DB) episodeTaskLocked(assignment journal.AssignmentID) (journal.TaskID, error) {
	var raw string
	if err := sqlitex.Execute(db.conn,
		`SELECT task_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{string(assignment)}, ResultFunc: func(stmt *zs.Stmt) error { raw = stmt.ColumnText(0); return nil }}); err != nil {
		return journal.TaskID{}, fmt.Errorf("episode task %q: %w", assignment, err)
	}
	if raw == "" {
		return journal.TaskID{}, fmt.Errorf("episode %q has no task", assignment)
	}
	return journalParseTask(raw)
}

func (db *DB) taskHasActiveOwnerEpisodeLocked(task journal.TaskID) (bool, error) {
	found := false
	if err := sqlitex.Execute(db.conn,
		`SELECT 1 FROM journal_authority_assignment_episodes e
		 WHERE e.task_id = ?1 AND e.slot_id = ?2
		   AND EXISTS (SELECT 1 FROM journal_authority_assignment_transitions s WHERE s.assignment_id = e.assignment_id AND s.transition_id = ?3)
		   AND NOT EXISTS (SELECT 1 FROM journal_authority_assignment_transitions x WHERE x.assignment_id = e.assignment_id AND x.transition_id = ?4)
		 LIMIT 1`,
		&sqlitex.ExecOptions{
			Args:       []any{task.String(), slotOwnerResponsibilityID, transitionStartedID, transitionEndedID},
			ResultFunc: func(*zs.Stmt) error { found = true; return nil },
		}); err != nil {
		return false, fmt.Errorf("active owner episode %q: %w", task, err)
	}
	return found, nil
}

// ---------------------------------------------------------------------------
// Genesis + authority scope validation (§4.6, §9.3, §10 rules 6-7, §14.1)
// ---------------------------------------------------------------------------

func (db *DB) operationCountLocked() (int, error) {
	var n int
	if err := sqlitex.Execute(db.conn, `SELECT COUNT(*) FROM journal_operations`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("count operations: %w", err)
	}
	return n, nil
}

func (db *DB) validateGenesisLocked(in journal.OperationInput) error {
	count, err := db.operationCountLocked()
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf(
			"%w: operation %q presents a NULL authority against a non-empty journal — "+
				"where: genesis validity (§4.6, §10 rule 6); when: before commit; impact: nothing "+
				"committed; fix: a NULL authority is accepted only on the first operation in an empty "+
				"journal; issue this operation under an existing authority",
			journal.ErrGenesis, in.OperationID)
	}
	if len(in.Effects) != 1 || in.Effects[0].Sort != journal.EffectBootstrapAuthority {
		return fmt.Errorf(
			"%w: genesis operation %q must produce exactly one bootstrap authority and nothing else "+
				"(saw %d effects) — where: genesis sole-effect (§10 rule 7); when: before commit; "+
				"impact: nothing committed; fix: a genesis operation's only effect is one bootstrap authority",
			journal.ErrGenesis, in.OperationID, len(in.Effects))
	}
	return nil
}

func (db *DB) requireAuthorityExistsLocked(authJID journal.JournalID) error {
	found := false
	if err := sqlitex.Execute(db.conn,
		`SELECT 1 FROM journal_authorities WHERE journal_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{int64(authJID)}, ResultFunc: func(*zs.Stmt) error { found = true; return nil }}); err != nil {
		return fmt.Errorf("require authority %d: %w", authJID, err)
	}
	if !found {
		return fmt.Errorf(
			"%w: operation cites authority %d which is not a committed journal_authorities row — "+
				"where: authority resolution (§4.2); when: before commit; impact: nothing committed; "+
				"fix: cite an authority produced by an earlier committed operation",
			journal.ErrAuthorityScope, authJID)
	}
	return nil
}

// requireAuthorityGovernsLocked authorizes a task-bearing effect against the
// operation's authority at the effect's own JournalID (§9.3, §14.1). A genesis
// operation never reaches here (its sole effect is a bootstrap, task-free).
func (db *DB) requireAuthorityGovernsLocked(in journal.OperationInput, effectJID int64, task journal.TaskID) error {
	if in.AuthorityJournalID == nil {
		return fmt.Errorf(
			"%w: a task-bearing effect on %q requires a non-NULL authority (§4.6 restricts NULL "+
				"authority to a genesis operation's sole bootstrap effect)", journal.ErrGenesis, task)
	}
	governs, err := db.authorityGovernsTaskAtLocked(*in.AuthorityJournalID, task, effectJID)
	if err != nil {
		return err
	}
	if !governs {
		return fmt.Errorf(
			"%w: authority %d does not govern task %q at journal position %d — where: per-effect "+
				"authorization (§9.3, §14.1); when: before commit; impact: nothing committed; fix: use the "+
				"bootstrap authority, or an assignment authority whose active episode is on this exact task, "+
				"committed with a strictly smaller JournalID than the effect",
			journal.ErrAuthorityScope, *in.AuthorityJournalID, task, effectJID)
	}
	return nil
}

// authorityGovernsTaskAtLocked answers whether the authority at authJID governs
// targetTask for an effect committed at beforeJID (§9.3): a bootstrap authority
// (the system root) governs every task; an assignment authority governs ONLY the
// task of its own active episode. There is no edge-graph governance — a
// scheduling edge such as blocked_by carries no ownership semantics, so a task
// merely reachable through one is NOT governed (see the authority note in
// docs/journal-relational-contract.md §14.1). The authority must strictly precede
// the effect by JournalID (never by RecordedAt, §12).
func (db *DB) authorityGovernsTaskAtLocked(authJID journal.JournalID, targetTask journal.TaskID, beforeJID int64) (bool, error) {
	if int64(authJID) >= beforeJID {
		return false, nil // authority does not precede the effect (§9.3)
	}
	var kind = -1
	if err := sqlitex.Execute(db.conn,
		`SELECT authority_kind_id FROM journal_authorities WHERE journal_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{int64(authJID)}, ResultFunc: func(stmt *zs.Stmt) error { kind = stmt.ColumnInt(0); return nil }}); err != nil {
		return false, fmt.Errorf("authority kind %d: %w", authJID, err)
	}
	switch kind {
	case authKindBootstrapID:
		return true, nil
	case authKindAssignmentID:
		return db.assignmentAuthorityGovernsLocked(authJID, targetTask)
	default:
		return false, nil // unknown/absent authority governs nothing
	}
}

func (db *DB) assignmentAuthorityGovernsLocked(authJID journal.JournalID, targetTask journal.TaskID) (bool, error) {
	// Resolve the assignment episode this authority (a transition row) belongs to.
	var assignment string
	if err := sqlitex.Execute(db.conn,
		`SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{int64(authJID)}, ResultFunc: func(stmt *zs.Stmt) error { assignment = stmt.ColumnText(0); return nil }}); err != nil {
		return false, fmt.Errorf("authority assignment %d: %w", authJID, err)
	}
	if assignment == "" {
		return false, nil
	}
	// The assignment must still be active (a started-but-not-ended episode).
	ended, exists, err := db.episodeEndedLocked(journal.AssignmentID(assignment))
	if err != nil {
		return false, err
	}
	if !exists || ended {
		return false, nil
	}
	authTask, err := db.episodeTaskLocked(journal.AssignmentID(assignment))
	if err != nil {
		return false, err
	}
	// An assignment authority governs ONLY the task of its own active episode. It
	// grants no authority over any other task, including one merely reachable
	// through a scheduling edge (blocked_by), which carries no ownership semantics.
	// A broader governance chain (e.g. a genuine governing-parent relation) would
	// require a contract amendment before implementation (§14.1 authority note).
	return authTask.String() == targetTask.String(), nil
}

// validateClosesEndAssignmentsLocked rejects an operation that closes a task
// (a provenance.task.closed effect) while leaving an active owner-responsibility
// episode on it (§8.1 / owner_responsibility regression c): the close and the
// episode end must not drift apart.
func (db *DB) validateClosesEndAssignmentsLocked(anchor int64, effects []journal.Effect) error {
	for _, eff := range effects {
		if eff.Sort != journal.EffectTaskEvent || eff.EventKind != "provenance.task.closed" {
			continue
		}
		active, err := db.taskHasActiveOwnerEpisodeLocked(eff.TaskID)
		if err != nil {
			return err
		}
		if active {
			return fmt.Errorf(
				"%w: task %q was closed but retains an active owner-responsibility episode — where: "+
					"close-ends-assignment gate (§8.1); when: before commit; impact: nothing committed; "+
					"fix: end the active owner episode in the same operation as the close",
				journal.ErrCloseWithoutEnding, eff.TaskID)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Replay identity + committed-result reconstruction (§3.2, §9.4)
// ---------------------------------------------------------------------------

type storedOperation struct {
	anchor   int64
	identity journal.StoredOperationIdentity
}

func (db *DB) lookupOperationLocked(op journal.OperationID) (storedOperation, bool, error) {
	var out storedOperation
	found := false
	if err := sqlitex.Execute(db.conn,
		`SELECT journal_id, authority_journal_id, command_digest, mutation_digest FROM journal_operations WHERE operation_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{string(op)}, ResultFunc: func(stmt *zs.Stmt) error {
			found = true
			out.anchor = stmt.ColumnInt64(0)
			if stmt.ColumnType(1) != zs.TypeNull {
				a := journal.JournalID(stmt.ColumnInt64(1))
				out.identity.AuthorityJournalID = &a
			}
			out.identity.CommandDigest = readBlob(stmt, 2)
			out.identity.MutationDigest = readBlob(stmt, 3)
			return nil
		}}); err != nil {
		return storedOperation{}, false, fmt.Errorf("lookup operation %q: %w", op, err)
	}
	if !found {
		return storedOperation{}, false, nil
	}
	// The committing actor lives on the anchor journal row.
	if err := sqlitex.Execute(db.conn,
		`SELECT actor_id FROM journal WHERE journal_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{out.anchor}, ResultFunc: func(stmt *zs.Stmt) error {
			actor, err := journalParseActor(stmt.ColumnText(0))
			if err != nil {
				return err
			}
			out.identity.ActorID = actor
			return nil
		}}); err != nil {
		return storedOperation{}, false, fmt.Errorf("lookup operation actor %q: %w", op, err)
	}
	return out, true, nil
}

// identityMismatch compares the stored and proposed four-field replay identities
// (§9.4). It returns the first differing field name and ok=false on mismatch.
func identityMismatch(stored, proposed journal.StoredOperationIdentity) (string, bool) {
	if stored.ActorID != proposed.ActorID {
		return "actor", false
	}
	if !journalIDPtrEqual(stored.AuthorityJournalID, proposed.AuthorityJournalID) {
		return "authority", false
	}
	if !bytes.Equal(stored.CommandDigest, proposed.CommandDigest) {
		return "command digest", false
	}
	if !bytes.Equal(stored.MutationDigest, proposed.MutationDigest) {
		return "mutation digest", false
	}
	return "", true
}

func journalIDPtrEqual(a, b *journal.JournalID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// committedOutcomeForExistingLocked resolves the §9.4 outcome for an OperationID
// that already has a committed row. An exact four-field identity match returns the
// original committed result short-circuited (no re-execution, nil error). Any
// mismatch returns the closed-sum CommittedConflict variant carrying the typed
// *OperationConflict payload, alongside an error that wraps BOTH the
// ErrOperationConflict sentinel and the *OperationConflict value with %w — so a
// caller recovers it with errors.Is(err, ErrOperationConflict) or
// errors.As(err, &*OperationConflict), and a caller switching on res.Kind sees
// CommittedConflict (§11, §9.6). Shared by the Apply short-circuit and the
// concurrent-insert race translation so both surface the identical typed shape.
func (db *DB) committedOutcomeForExistingLocked(in journal.OperationInput, existing storedOperation) (journal.CommittedResult, error) {
	if field, ok := identityMismatch(existing.identity, journal.StoredOperationIdentity{
		ActorID:            in.ActorID,
		AuthorityJournalID: in.AuthorityJournalID,
		CommandDigest:      in.CommandDigest,
		MutationDigest:     in.MutationDigest,
	}); !ok {
		conflict := &journal.OperationConflict{OperationID: in.OperationID, Field: field}
		return journal.CommittedResult{Kind: journal.CommittedConflict, Conflict: conflict},
			fmt.Errorf("%w: %w", journal.ErrOperationConflict, conflict)
	}
	res, err := db.reconstructCommittedLocked(existing.anchor)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	res.ShortCircuited = true
	return res, nil
}

// resolveOperationIDInsertRaceLocked implements §9.6's second bullet: when the
// anchor insert violates journal_operations.OperationID UNIQUE because a
// concurrent writer committed the same new OperationID first, the reducer catches
// that violation and re-runs the §9.4 idempotent-replay comparison against the
// now-committed row, returning the typed idempotent result or the typed
// CommittedConflict — never the raw SQLite constraint error. Under the in-process
// db.mu this is unreachable (Apply's §9.4 lookup observes the committed row before
// ever reaching the insert); it is the defense-in-depth path for a future
// multi-connection/multi-process writer.
func (db *DB) resolveOperationIDInsertRaceLocked(in journal.OperationInput) (journal.CommittedResult, error) {
	existing, found, err := db.lookupOperationLocked(in.OperationID)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	if !found {
		// The UNIQUE violation proved a row exists, but this transaction's read
		// snapshot cannot see it (the winning writer committed on another
		// connection after this transaction's snapshot began). Surface a typed
		// conflict rather than the raw SQLite constraint error (§9.6).
		conflict := &journal.OperationConflict{OperationID: in.OperationID, Field: "operation id (lost a concurrent insert)"}
		return journal.CommittedResult{Kind: journal.CommittedConflict, Conflict: conflict},
			fmt.Errorf("%w: %w", journal.ErrOperationConflict, conflict)
	}
	return db.committedOutcomeForExistingLocked(in, existing)
}

func (db *DB) reconstructCommittedLocked(anchor int64) (journal.CommittedResult, error) {
	res := journal.CommittedResult{Kind: journal.CommittedExact, AnchorJournalID: journal.JournalID(anchor)}
	// EmittedEvents: the flat task_event closure in JournalID order (§2.1, §3.2).
	if err := sqlitex.Execute(db.conn,
		`SELECT journal_id FROM journal WHERE produced_by_operation_journal_id = ?1 AND kind_id = ?2 ORDER BY journal_id ASC`,
		&sqlitex.ExecOptions{Args: []any{anchor, int(journal.JournalKindTaskEvent)}, ResultFunc: func(stmt *zs.Stmt) error {
			res.EmittedEvents = append(res.EmittedEvents, journal.JournalID(stmt.ColumnInt64(0)))
			return nil
		}}); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("reconstruct emitted events: %w", err)
	}
	// Slot-keyed result map (§3.2), bucketed by JournalKind.
	if err := sqlitex.Execute(db.conn,
		`SELECT s.result_slot_id, s.produced_journal_id, j.kind_id, te.task_id
		 FROM journal_operation_result_slots s
		 JOIN journal j ON j.journal_id = s.produced_journal_id
		 LEFT JOIN journal_task_events te ON te.journal_id = s.produced_journal_id
		 WHERE s.journal_id = ?1 ORDER BY s.result_slot_id ASC`,
		&sqlitex.ExecOptions{Args: []any{anchor}, ResultFunc: func(stmt *zs.Stmt) error {
			binding := journal.ResultSlotBinding{
				Slot:              journal.ResultSlotID(stmt.ColumnText(0)),
				ProducedJournalID: journal.JournalID(stmt.ColumnInt64(1)),
				Kind:              journal.JournalKind(stmt.ColumnInt(2)),
			}
			if stmt.ColumnType(3) != zs.TypeNull {
				if tid, err := journalParseTask(stmt.ColumnText(3)); err == nil {
					binding.TaskID = &tid
				}
			}
			res.ResultSlots = append(res.ResultSlots, binding)
			return nil
		}}); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("reconstruct result slots: %w", err)
	}
	return res, nil
}

// LookupCommitted returns the committed result for an OperationID (§9.4): the
// closed Absent variant with no side effects for a never-applied operation, or
// the Exact variant with the reconstructed EmittedEvents closure and slot map.
func (db *DB) LookupCommitted(op journal.OperationID) (journal.CommittedResult, error) {
	if err := journal.ValidateOperationID(op); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("LookupCommitted: %w", err)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	stored, found, err := db.lookupOperationLocked(op)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	if !found {
		return journal.CommittedResult{Kind: journal.CommittedAbsent}, nil
	}
	return db.reconstructCommittedLocked(stored.anchor)
}

// AuthorityGovernsTaskAt is the pure authorization predicate (§9.3, §14.1),
// exposed so the ordering-vs-authority corpus histories can assert that an
// authority committed after an effect (greater JournalID) never authorizes it,
// regardless of RecordedAt (§12).
func (db *DB) AuthorityGovernsTaskAt(authJID journal.JournalID, task journal.TaskID, beforeJID journal.JournalID) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.authorityGovernsTaskAtLocked(authJID, task, int64(beforeJID))
}

// CountAuthoritiesOfKind returns how many journal_authorities rows carry the
// given authority_kind_id. It is an audit/read helper (e.g. asserting a genesis
// retry created no second bootstrap authority).
func (db *DB) CountAuthoritiesOfKind(kind int) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var n int
	if err := sqlitex.Execute(db.conn,
		`SELECT COUNT(*) FROM journal_authorities WHERE authority_kind_id = ?1`,
		&sqlitex.ExecOptions{Args: []any{kind}, ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("CountAuthoritiesOfKind %d: %w", kind, err)
	}
	return n, nil
}

// CountSuccessorEpisodes returns how many episodes on a task cite a predecessor
// (i.e. were created by a transfer). It is an audit/read helper used to prove a
// losing CAS transfer wrote nothing.
func (db *DB) CountSuccessorEpisodes(task journal.TaskID) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var n int
	if err := sqlitex.Execute(db.conn,
		`SELECT COUNT(*) FROM journal_authority_assignment_episodes WHERE task_id = ?1 AND predecessor_assignment_id IS NOT NULL`,
		&sqlitex.ExecOptions{Args: []any{task.String()}, ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("CountSuccessorEpisodes %q: %w", task, err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Small utilities
// ---------------------------------------------------------------------------

func readBlob(stmt *zs.Stmt, col int) []byte {
	n := stmt.ColumnLen(col)
	buf := make([]byte, n)
	stmt.ColumnBytes(col, buf)
	return buf
}

func isUniqueViolation(err error) bool {
	return err != nil && (zs.ErrCode(err) == zs.ResultConstraintUnique || zs.ErrCode(err) == zs.ResultConstraintPrimaryKey ||
		errors.Is(err, errUniqueSentinel))
}

// errUniqueSentinel is unused at runtime; it keeps isUniqueViolation total even
// if a future zombiezen version changes its extended-code surface.
var errUniqueSentinel = errors.New("unique constraint")
