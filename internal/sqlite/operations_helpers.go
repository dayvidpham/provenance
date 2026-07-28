package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/internal/fusedtx"
	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	moderncsqlite "modernc.org/sqlite"
)

// operations_helpers.go holds the low-level reducer steps and read-path
// reconstruction that Apply (operations.go) composes. Every scope helper assumes
// the caller owns scope.conn and runs inside Apply's single transaction (§9.5), so it
// observes the state produced by all earlier effects of the same operation
// (§9.3). LookupCommitted and the pure authorization predicate are the public
// read surfaces.

// ---------------------------------------------------------------------------
// Row inserts
// ---------------------------------------------------------------------------

// queryRows executes a query on this scope's pinned connection and consumes its
// rows before returning the connection to any caller. It is deliberately narrow:
// callers still use database/sql's standard SQL contract, while this helper keeps
// every multi-row reducer read context-aware and guarantees both Rows.Close and
// Rows.Err are checked.
func (scope *connScope) queryRows(query string, args []any, consume func(*sql.Rows) error) (err error) {
	rows, err := scope.conn.QueryContext(scope.ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			if err == nil {
				err = fmt.Errorf("close SQL rows: %w", closeErr)
			} else {
				err = errors.Join(err, fmt.Errorf("close SQL rows: %w", closeErr))
			}
		}
	}()
	for rows.Next() {
		if err := consume(rows); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// runScopedSavepoint keeps a connection-affine operation atomic while allowing it
// to compose with activation and migration transactions that already own the same
// pinned connection. Names are static package constants, never caller input.
func (scope *connScope) runScopedSavepoint(operation func() error) (err error) {
	if _, err = scope.conn.ExecContext(scope.ctx, "SAVEPOINT provenance_scope"); err != nil {
		return fmt.Errorf("start scoped savepoint: %w", err)
	}
	released := false
	defer func() {
		if released {
			return
		}
		if _, rollbackErr := scope.conn.ExecContext(scope.ctx, "ROLLBACK TO SAVEPOINT provenance_scope"); rollbackErr != nil {
			if err == nil {
				err = fmt.Errorf("rollback scoped savepoint: %w", rollbackErr)
			} else {
				err = errors.Join(err, fmt.Errorf("rollback scoped savepoint: %w", rollbackErr))
			}
		}
		if _, releaseErr := scope.conn.ExecContext(scope.ctx, "RELEASE SAVEPOINT provenance_scope"); releaseErr != nil {
			if err == nil {
				err = fmt.Errorf("release rolled-back scoped savepoint: %w", releaseErr)
			} else {
				err = errors.Join(err, fmt.Errorf("release rolled-back scoped savepoint: %w", releaseErr))
			}
		}
	}()
	if err = operation(); err != nil {
		return err
	}
	if _, err = scope.conn.ExecContext(scope.ctx, "RELEASE SAVEPOINT provenance_scope"); err != nil {
		return fmt.Errorf("release scoped savepoint: %w", err)
	}
	released = true
	return nil
}

func (scope *connScope) insertJournalRow(kind journal.JournalKind, actor journal.ActorID, recordedAt int64, pboj *int64) (int64, error) {
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
	var journalID int64
	if err := scope.conn.QueryRowContext(scope.ctx, "INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id) VALUES (?1, ?2, ?3, ?4) RETURNING journal_id", int(kind), actorArg, recordedAt, pbojArg).Scan(&journalID); err != nil {
		return 0, fmt.Errorf("insert journal row (kind %s): %w", kind, err)
	}
	return journalID, nil
}

func (scope *connScope) insertOperationRow(anchor int64, in journal.OperationInput, prepared journal.CanonicalMutation) error {
	var authArg any
	if in.AuthorityJournalID != nil {
		authArg = int64(*in.AuthorityJournalID)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_operations\n\t\t (journal_id, operation_id, authority_journal_id, command_digest, mutation_digest, mutation_encoding_version, canonical_mutation)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", anchor, string(in.OperationID), authArg, in.CommandDigest, prepared.DerivedDigest(), prepared.EncodingVersion().String(), prepared.CanonicalBytes()); err != nil {
		return fmt.Errorf("insert journal_operations for %q: %w", in.OperationID, err)
	}
	return nil
}

func (scope *connScope) insertAuthorityAssignmentTransition(jid int64, assignment journal.AssignmentID, transitionID int) error {
	opAuthID := fmt.Sprintf("authority--assignment--%d", jid)
	if _, err := scope.conn.ExecContext(scope.ctx, insertJournalAuthoritySQL, jid, authKindAssignmentID, opAuthID); err != nil {
		return fmt.Errorf("insert journal_authorities (assignment): %w", err)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_authority_assignment_transitions (journal_id, assignment_id, transition_id) VALUES (?1, ?2, ?3)", jid, string(assignment), transitionID); err != nil {
		return fmt.Errorf("insert assignment transition (%s): %w", journal.AssignmentTransition(transitionID), err)
	}
	return nil
}

func (scope *connScope) insertResultSlot(anchor int64, slot journal.ResultSlotID, producedJID int64) error {
	// rule 9 own-operation integrity (§3.2, §10 rule 9): the produced row must
	// have been produced by this same operation. Always holds on the normal path
	// (producedJID is a row this operation just inserted), enforced anyway.
	if err := scope.requireResultSlotOwnOperation(anchor, producedJID); err != nil {
		return err
	}
	if _, err := scope.conn.ExecContext(scope.ctx, "INSERT INTO journal_operation_result_slots (journal_id, result_slot_id, produced_journal_id) VALUES (?1, ?2, ?3)", anchor, string(slot), producedJID); err != nil {
		return fmt.Errorf("insert result slot %q: %w", slot, err)
	}
	return nil
}

func (scope *connScope) requireResultSlotOwnOperation(anchor, producedJID int64) error {
	return requireResultSlotOwnOperation(scope.ctx, allocationSQLTx{conn: scope.conn}, anchor, producedJID)
}

func requireResultSlotOwnOperation(ctx context.Context, tx fusedtx.SQLTx, anchor, producedJID int64) error {
	var producer sql.NullInt64
	if err := tx.QueryRow(ctx, "SELECT produced_by_operation_journal_id FROM journal WHERE journal_id = ?1", producedJID).Scan(&producer); err != nil {
		return fmt.Errorf("rule-9 check: load produced row %d: %w", producedJID, err)
	}
	if !producer.Valid || producer.Int64 != anchor {
		return fmt.Errorf(
			"%w: result slot on operation anchor %d references produced row %d whose own producing "+
				"operation is %d — where: result-slot fold (§3.2, §10 rule 9); when: before commit; "+
				"impact: nothing committed; fix: a result slot may only map to a row its own operation produced",
			journal.ErrResultSlotIntegrity, anchor, producedJID, producer.Int64)
	}
	return nil
}

func (scope *connScope) insertAttribution(task journal.TaskID, actor journal.ActorID, jid int64) error {
	// Targets the real task_attributions during a live Apply and the shadow
	// attribution table during a from-empty replay derivation (§8.2, §15).
	if scope.projectionTarget == projectionTargetLive {
		return v1InsertAttribution(scope.ctx, allocationSQLTx{conn: scope.conn}, task, actor, jid)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, scope.projectionTarget.insertAttributionQuery(), task.String(), actor.String(), jid); err != nil {
		return fmt.Errorf("update %s attribution: %w", scope.projectionTarget.label(), err)
	}
	return nil
}

func (scope *connScope) advanceWatermark(task journal.TaskID, jid int64) error {
	// Targets the real tasks table during a live Apply and the shadow tasks table
	// during a from-empty replay derivation (§8.1, §15).
	if scope.projectionTarget == projectionTargetLive {
		return v1AdvanceWatermark(scope.ctx, allocationSQLTx{conn: scope.conn}, task, jid)
	}
	if _, err := scope.conn.ExecContext(scope.ctx, scope.projectionTarget.advanceWatermarkQuery(), jid, task.String()); err != nil {
		return fmt.Errorf("advance %s task watermark: %w", scope.projectionTarget.label(), err)
	}
	return nil
}

// recomputeTaskOwner materializes the owner-responsibility projection
// (§8.1): tasks.owner_id becomes the current active owner episode's occupant, or
// NULL when none is active. The watermark advances to jid. The SELECT reads the
// journal spine (the source of truth, untouched by replay); the UPDATE targets the
// projection table — real during Apply, shadow during replay (§15).
func (scope *connScope) recomputeTaskOwner(task journal.TaskID, jid int64) error {
	var owner sql.NullString
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT e.actor_id FROM journal_authority_assignment_episodes e\n\t\t JOIN journal_authority_assignment_transitions started\n\t\t   ON started.assignment_id = e.assignment_id AND started.transition_id = ?2\n\t\t WHERE e.task_id = ?1 AND e.slot_id = ?3\n\t\t   AND NOT EXISTS (SELECT ?5 FROM journal_authority_assignment_transitions ended\n\t\t                   WHERE ended.assignment_id = e.assignment_id AND ended.transition_id = ?4)\n\t\t ORDER BY started.journal_id DESC LIMIT ?6", task.String(), transitionStartedID, slotOwnerResponsibilityID, transitionEndedID, 1, 1).Scan(&owner); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("recompute task owner: %w", err)
	}
	var ownerArg any
	if owner.Valid {
		ownerArg = owner.String
	}
	if _, err := scope.conn.ExecContext(scope.ctx, scope.projectionTarget.updateOwnerQuery(), ownerArg, jid, task.String()); err != nil {
		return fmt.Errorf("update %s owner: %w", scope.projectionTarget.label(), err)
	}
	return nil
}

func (target projectionTarget) insertAttributionQuery() string {
	switch target {
	case projectionTargetLive:
		return "INSERT OR IGNORE INTO task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)"
	case projectionTargetShadow:
		return "INSERT OR IGNORE INTO shadow_task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) advanceWatermarkQuery() string {
	switch target {
	case projectionTargetLive:
		return "UPDATE tasks SET last_journal_id = ?1 WHERE id = ?2"
	case projectionTargetShadow:
		return "UPDATE shadow_tasks SET last_journal_id = ?1 WHERE id = ?2"
	default:
		panic("unknown projection target")
	}
}

func (target projectionTarget) updateOwnerQuery() string {
	switch target {
	case projectionTargetLive:
		return "UPDATE tasks SET owner_id = ?1, last_journal_id = ?2 WHERE id = ?3"
	case projectionTargetShadow:
		return "UPDATE shadow_tasks SET owner_id = ?1, last_journal_id = ?2 WHERE id = ?3"
	default:
		panic("unknown projection target")
	}
}

// ---------------------------------------------------------------------------
// Episode/authority state queries (§4.4, §14)
// ---------------------------------------------------------------------------

func (scope *connScope) episodeStarted(assignment journal.AssignmentID) (bool, error) {
	return scope.transitionExists(assignment, transitionStartedID)
}

func (scope *connScope) episodeEnded(assignment journal.AssignmentID) (ended bool, exists bool, err error) {
	exists, err = scope.episodeExists(assignment)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, false, nil
	}
	ended, err = scope.transitionExists(assignment, transitionEndedID)
	return ended, true, err
}

func (scope *connScope) episodeExists(assignment journal.AssignmentID) (bool, error) {
	var found int
	err := scope.conn.QueryRowContext(scope.ctx, "SELECT ?2 FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", string(assignment), 1).Scan(&found)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("episode exists %q: %w", assignment, err)
	}
	return err == nil, nil
}

func (scope *connScope) transitionExists(assignment journal.AssignmentID, transitionID int) (bool, error) {
	var found int
	err := scope.conn.QueryRowContext(scope.ctx, "SELECT ?3 FROM journal_authority_assignment_transitions WHERE assignment_id = ?1 AND transition_id = ?2", string(assignment), transitionID, 1).Scan(&found)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("transition exists %q/%d: %w", assignment, transitionID, err)
	}
	return err == nil, nil
}

func (scope *connScope) episodeTask(assignment journal.AssignmentID) (journal.TaskID, error) {
	var raw string
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT task_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", string(assignment)).Scan(&raw); err != nil {
		return journal.TaskID{}, fmt.Errorf("episode task %q: %w", assignment, err)
	}
	if raw == "" {
		return journal.TaskID{}, fmt.Errorf("episode %q has no task", assignment)
	}
	return journalParseTask(raw)
}

// episodeParent returns the ParentAssignmentID citation of an episode
// (§4.4, §14.5). hasParent is false when the episode cites no parent (NULL
// parent_assignment_id) or does not exist.
func (scope *connScope) episodeParent(assignment journal.AssignmentID) (parent journal.AssignmentID, hasParent bool, err error) {
	var raw sql.NullString
	if execErr := scope.conn.QueryRowContext(scope.ctx, "SELECT parent_assignment_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", string(assignment)).Scan(&raw); execErr != nil {
		if errors.Is(execErr, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("episode parent %q: %w", assignment, execErr)
	}
	if raw.Valid {
		parent = journal.AssignmentID(raw.String)
		hasParent = parent != ""
	}
	return parent, hasParent, nil
}

// transitionExistsBefore reports whether the episode has the given
// transition committed at a journal position strictly before beforeJID (§14.5
// position-aware liveness). It is the position-scoped variant of
// transitionExists, which considers transitions at any position.
func (scope *connScope) transitionExistsBefore(assignment journal.AssignmentID, transitionID int, beforeJID int64) (bool, error) {
	var found int
	err := scope.conn.QueryRowContext(scope.ctx, "SELECT ?4 FROM journal_authority_assignment_transitions\n\t\t WHERE assignment_id = ?1 AND transition_id = ?2 AND journal_id < ?3 LIMIT ?5", string(assignment), transitionID, beforeJID, 1, 1).Scan(&found)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("transition-before %q/%d < %d: %w", assignment, transitionID, beforeJID, err)
	}
	return err == nil, nil
}

// episodeActiveAt reports whether episode `assignment` is active at journal
// position beforeJID (§14.5 liveness): it has a started transition strictly before
// beforeJID and no ended transition strictly before beforeJID. "Active at effect
// time" — used both for the citation guard (at the start transition's own
// position) and for whole-chain liveness in the governance walk (at the consuming
// effect's position).
func (scope *connScope) episodeActiveAt(assignment journal.AssignmentID, beforeJID int64) (bool, error) {
	started, err := scope.transitionExistsBefore(assignment, transitionStartedID, beforeJID)
	if err != nil {
		return false, err
	}
	if !started {
		return false, nil
	}
	ended, err := scope.transitionExistsBefore(assignment, transitionEndedID, beforeJID)
	if err != nil {
		return false, err
	}
	return !ended, nil
}

func (scope *connScope) taskHasActiveOwnerEpisode(task journal.TaskID) (bool, error) {
	var found int
	err := scope.conn.QueryRowContext(scope.ctx, "SELECT ?5 FROM journal_authority_assignment_episodes e\n\t\t WHERE e.task_id = ?1 AND e.slot_id = ?2\n\t\t   AND EXISTS (SELECT ?6 FROM journal_authority_assignment_transitions s WHERE s.assignment_id = e.assignment_id AND s.transition_id = ?3)\n\t\t   AND NOT EXISTS (SELECT ?7 FROM journal_authority_assignment_transitions x WHERE x.assignment_id = e.assignment_id AND x.transition_id = ?4)\n\t\t LIMIT ?8", task.String(), slotOwnerResponsibilityID, transitionStartedID, transitionEndedID, 1, 1, 1, 1).Scan(&found)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("active owner episode %q: %w", task, err)
	}
	return err == nil, nil
}

// ---------------------------------------------------------------------------
// Genesis + authority scope validation (§4.6, §9.3, §10 rules 6-7, §14.1)
// ---------------------------------------------------------------------------

func (scope *connScope) operationCount() (int, error) {
	var n int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal_operations").Scan(&n); err != nil {
		return 0, fmt.Errorf("count operations: %w", err)
	}
	return n, nil
}

func (scope *connScope) validateGenesis(in journal.OperationInput) error {
	count, err := scope.operationCount()
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

func (scope *connScope) requireAuthorityExists(authJID journal.JournalID) error {
	var found int
	err := scope.conn.QueryRowContext(scope.ctx, "SELECT ?2 FROM journal_authorities WHERE journal_id = ?1", int64(authJID), 1).Scan(&found)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("require authority %d: %w", authJID, err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"%w: operation cites authority %d which is not a committed journal_authorities row — "+
				"where: authority resolution (§4.2); when: before commit; impact: nothing committed; "+
				"fix: cite an authority produced by an earlier committed operation",
			journal.ErrAuthorityScope, authJID)
	}
	return nil
}

// requireAuthorityGoverns authorizes a task-bearing effect against the
// operation's authority at the effect's own JournalID (§9.3, §14.1). A genesis
// operation never reaches here (its sole effect is a bootstrap, task-free).
func (scope *connScope) requireAuthorityGoverns(in journal.OperationInput, effectJID int64, task journal.TaskID) error {
	if in.AuthorityJournalID == nil {
		return fmt.Errorf(
			"%w: a task-bearing effect on %q requires a non-NULL authority (§4.6 restricts NULL "+
				"authority to a genesis operation's sole bootstrap effect)", journal.ErrGenesis, task)
	}
	governs, err := scope.authorityGovernsTaskAt(*in.AuthorityJournalID, task, effectJID)
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

// authorityGovernsTaskAt answers whether the authority at authJID governs
// targetTask for an effect committed at beforeJID (§9.3, §14.5): a bootstrap
// authority (the system root) governs every task; an assignment authority governs
// its own active episode's task PLUS every task whose episode reaches that episode
// via a chain of deliberate ParentAssignmentID citations, with the whole chain
// active at beforeJID. There is no edge-graph governance — a scheduling edge such
// as blocked_by carries no ownership semantics, so a task merely reachable through
// one is NOT governed (§14.1); only deliberate parent citations cross tasks. The
// authority must strictly precede the effect by JournalID (never by RecordedAt,
// §12).
func (scope *connScope) authorityGovernsTaskAt(authJID journal.JournalID, targetTask journal.TaskID, beforeJID int64) (bool, error) {
	return v1AuthorityGoverns(scope.ctx, allocationSQLTx{conn: scope.conn}, authJID, targetTask, beforeJID)
}

func (scope *connScope) countEpisodes() (int, error) {
	var n int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal_authority_assignment_episodes").Scan(&n); err != nil {
		return 0, fmt.Errorf("count episodes: %w", err)
	}
	return n, nil
}

// requireParentCitationValid validates an assignment-start's
// ParentAssignmentID citation (§14.5): the cited parent must exist and be active
// at this start transition's own journal position startJID, and the citation must
// not create a cycle. It returns nil for an empty (absent) citation.
func (scope *connScope) requireParentCitationValid(newEpisode, parent journal.AssignmentID, startJID int64) error {
	if parent == "" {
		return nil
	}
	exists, err := scope.episodeExists(parent)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf(
			"%w: assignment start cites parent episode %q which does not exist — where: assignment-start "+
				"fold (§14.5); when: before commit; impact: nothing committed; fix: cite an existing, active "+
				"episode, or omit the parent citation",
			journal.ErrParentCitation, parent)
	}
	active, err := scope.episodeActiveAt(parent, startJID)
	if err != nil {
		return err
	}
	if !active {
		return fmt.Errorf(
			"%w: assignment start cites parent episode %q which is not active at journal position %d "+
				"(never started, or already ended) — where: assignment-start fold (§14.5); when: before "+
				"commit; impact: nothing committed; fix: root this episode under a parent whose episode is "+
				"active at the moment of citation",
			journal.ErrParentCitation, parent, startJID)
	}
	// Cycle guard: the new episode must not already appear in the parent's
	// ancestry. With citation-at-start the new AssignmentID does not yet exist, so
	// this is structurally impossible for honest input; the bounded, visited-
	// tracked walk still fails closed on a forged/corrupt ancestry.
	return scope.requireNoParentCycle(newEpisode, parent)
}

// requireNoParentCycle walks the parent's existing ancestry and rejects a
// citation that would place newEpisode in its own ancestry (a cycle), or that
// traverses a pre-existing corrupt cycle (§14.5). Liveness is not consulted here —
// a cycle is a structural property of the stored chain.
func (scope *connScope) requireNoParentCycle(newEpisode, parent journal.AssignmentID) error {
	visited := map[journal.AssignmentID]struct{}{}
	cur := parent
	maxSteps, err := scope.countEpisodes()
	if err != nil {
		return err
	}
	for step := 0; ; step++ {
		if cur == newEpisode {
			return fmt.Errorf(
				"%w: citing parent %q would make episode %q an ancestor of itself (a cycle) — where: "+
					"assignment-start fold (§14.5); when: before commit; impact: nothing committed; fix: "+
					"parent chains are finite; do not cite a descendant as a parent",
				journal.ErrParentCitation, parent, newEpisode)
		}
		if _, seen := visited[cur]; seen || step > maxSteps {
			return fmt.Errorf(
				"%w: parent %q ancestry revisited %q — where: assignment-start cycle guard (§14.5); when: "+
					"before commit; impact: nothing committed; fix: the stored parent_assignment_id chain is "+
					"already corrupt (a cycle); repair the journal before citing into it",
				journal.ErrCorruptParentChain, parent, cur)
		}
		visited[cur] = struct{}{}
		next, hasParent, err := scope.episodeParent(cur)
		if err != nil {
			return err
		}
		if !hasParent {
			return nil
		}
		cur = next
	}
}

// validateClosesEndAssignments rejects an operation that closes a task
// (a provenance.task.closed effect) while leaving an active owner-responsibility
// episode on it (§8.1 / owner_responsibility regression c): the close and the
// episode end must not drift apart.
func (scope *connScope) validateClosesEndAssignments(anchor int64, effects []journal.Effect) error {
	return v1ValidateClosedEvents(scope.ctx, allocationSQLTx{conn: scope.conn}, effects)
}

// The v1 helpers below are neutral SQLTx journal primitives. Both ordinary
// Journal.Apply and the governed-allocation composition reducer delegate here,
// so authority semantics cannot fork between those production paths.
func journalFoldError(stage, why, fix string, err error) error {
	return fmt.Errorf("journal fold %s failed — why: %s; where: shared V1 journal reducer; when: folding the canonical effect before commit; impact: the operation returns no committed result and its transaction is rolled back; fix: %s: %w", stage, why, fix, err)
}

func foldV1TaskEvent(ctx context.Context, tx fusedtx.SQLTx, in journal.OperationInput, jid, recordedAt int64, effect journal.Effect) error {
	if err := journal.ValidateEventKind(effect.EventKind); err != nil {
		return err
	}
	if err := v1RequireAuthorityGoverns(ctx, tx, *in.AuthorityJournalID, effect.TaskID, jid); err != nil {
		return err
	}
	payload := effect.Payload
	if effect.Forced && journal.IsTransitionLifecycleKind(effect.EventKind) {
		payload = journal.EncodeForcedTransitionPayload()
	}
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_task_events (journal_id,task_id,event_kind,payload) VALUES (?1,?2,?3,?4)`, jid, effect.TaskID.String(), string(effect.EventKind), string(payload)); err != nil {
		return journalFoldError("task-event persistence", "the canonical task event row could not be inserted", "repair journal_task_events schema or effect references, then retry", err)
	}
	if err := persistV1TaskContexts(ctx, tx, jid, effect.Contexts); err != nil {
		return err
	}
	return materializeV1TaskEvent(ctx, tx, effect, recordedAt)
}

func foldV1Evidence(ctx context.Context, tx fusedtx.SQLTx, in journal.OperationInput, jid int64, effect journal.Effect) error {
	var taskArg any
	if effect.TaskID.Namespace != "" {
		if err := v1RequireAuthorityGoverns(ctx, tx, *in.AuthorityJournalID, effect.TaskID, jid); err != nil {
			return err
		}
		taskArg = effect.TaskID.String()
	}
	payload := effect.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_evidence (journal_id,evidence_kind,task_id,content_digest,payload) VALUES (?1,?2,?3,?4,?5)`, jid, string(effect.EvidenceKind), taskArg, effect.ContentDigest, string(payload)); err != nil {
		return journalFoldError("evidence persistence", "the canonical evidence row could not be inserted", "repair journal_evidence schema or effect references, then retry", err)
	}
	return persistV1FactContexts(ctx, tx, `INSERT INTO journal_evidence_contexts (evidence_journal_id,context_kind,context_identity) VALUES (?1,?2,?3)`, jid, effect.Contexts)
}

func foldV1EdgeAdd(ctx context.Context, tx fusedtx.SQLTx, in journal.OperationInput, jid, _ int64, effect journal.Effect) error {
	if err := v1RequireAuthorityGoverns(ctx, tx, *in.AuthorityJournalID, effect.TaskID, jid); err != nil {
		return err
	}
	if effect.EdgeRelKind == ptypes.EdgeBlockedBy {
		var cycle int
		err := tx.QueryRow(ctx, `WITH RECURSIVE reach(node) AS (
			SELECT ?1 UNION SELECT e.target_id FROM edges e JOIN reach r ON e.source_id=r.node WHERE e.kind_id=?3
		) SELECT ?4 FROM reach WHERE node=?2 LIMIT ?5`, effect.EdgeTargetID, effect.TaskID.String(), int(ptypes.EdgeBlockedBy), 1, 1).Scan(&cycle)
		if err == nil {
			return fmt.Errorf("%w: adding blocked-by edge %q -> %q would create a cycle", ptypes.ErrCycleDetected, effect.TaskID.String(), effect.EdgeTargetID)
		}
		if !fusedtx.IsNoRows(err) {
			return journalFoldError("edge cycle check", "the existing edge graph could not be read", "repair the edges projection, then retry", err)
		}
	}
	payload, err := journal.EncodeEdgeMutationPayload(effect.EdgeTargetID, effect.EdgeRelKind)
	if err != nil {
		return journalFoldError("edge payload encoding", "the typed edge operands could not be encoded", "supply a valid edge target and relationship kind", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_task_events (journal_id,task_id,event_kind,payload) VALUES (?1,?2,?3,?4)`, jid, effect.TaskID.String(), string(journal.EventKindEdgeAdded), string(payload)); err != nil {
		return journalFoldError("edge-event persistence", "the canonical edge event row could not be inserted", "repair journal_task_events schema or edge references, then retry", err)
	}
	return persistV1TaskContexts(ctx, tx, jid, effect.Contexts)
}

func foldV1ActivityCreate(ctx context.Context, tx fusedtx.SQLTx, in journal.OperationInput, jid, recordedAt int64, effect journal.Effect) error {
	if effect.ActivityID.Namespace == "" || effect.ActivityAgentID.Namespace == "" {
		return fmt.Errorf("operation %q ActivityCreate has an unnamespaced activity or agent identity — why: durable activity identities must include a namespace; where: shared V1 ActivityCreate validation; when: before persisting the activity; impact: the operation returns no committed result and its transaction is rolled back; fix: supply both ActivityID and ActivityAgentID as valid namespaced identities", in.OperationID)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO activities (id,agent_id,phase_id,stage_id,started_at,ended_at,notes) VALUES (?1,?2,?3,?4,?5,?6,?7)`, effect.ActivityID.String(), effect.ActivityAgentID.String(), int(effect.ActivityPhase), int(effect.ActivityStage), recordedAt, nil, effect.ActivityNotes); err != nil {
		if isUniqueViolation(err) {
			var existing int64
			lookupErr := tx.QueryRow(ctx, `SELECT journal_id FROM journal_activity_creations WHERE activity_id=?1`, effect.ActivityID.String()).Scan(&existing)
			if lookupErr != nil && !fusedtx.IsNoRows(lookupErr) {
				return journalFoldError("attribute ActivityID collision", "the existing ActivityID could not be attributed to its journal row", "repair journal_activity_creations, then retry", lookupErr)
			}
			return &journal.ActivityConflict{ActivityID: effect.ActivityID, ExistingJournalID: journal.JournalID(existing)}
		}
		return journalFoldError("activity persistence", "the activity row could not be inserted", "register the cited agent and repair activity references, then retry", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO journal_activity_creations (journal_id,activity_id) VALUES (?1,?2)`, jid, effect.ActivityID.String()); err != nil {
		return journalFoldError("activity journal binding", "the activity could not be bound to its producing journal row", "repair journal_activity_creations schema or references, then retry", err)
	}
	return nil
}

func persistV1TaskContexts(ctx context.Context, tx fusedtx.SQLTx, jid int64, contexts []journal.EventContext) error {
	canonical, err := journal.CanonicalEventContexts(contexts)
	if err != nil {
		return err
	}
	for _, value := range canonical {
		kind, identity, err := journal.EncodeStoredEventContext(value)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO journal_task_event_contexts (event_journal_id,context_kind,context_identity,attached_by_journal_id) VALUES (?1,?2,?3,?4)`, jid, string(kind), identity, jid); err != nil {
			return journalFoldError("task-context persistence", "a canonical task context could not be inserted", "repair journal_task_event_contexts schema or references, then retry", err)
		}
	}
	return nil
}

func persistV1FactContexts(ctx context.Context, tx fusedtx.SQLTx, statement string, jid int64, contexts []journal.EventContext) error {
	canonical, err := journal.CanonicalEventContexts(contexts)
	if err != nil {
		return err
	}
	for _, value := range canonical {
		kind, identity, err := journal.EncodeStoredEventContext(value)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, statement, jid, string(kind), identity); err != nil {
			return journalFoldError("fact-context persistence", "a canonical fact context could not be inserted", "repair the fact-context schema or references, then retry", err)
		}
	}
	return nil
}

func materializeV1TaskEvent(ctx context.Context, tx fusedtx.SQLTx, effect journal.Effect, recordedAt int64) error {
	closeReasonSet := effect.EventKind == journal.EventKindTaskClosed && effect.CloseReason != ""
	if effect.UpdateTitle == nil && effect.UpdateDescription == nil && effect.UpdatePriority == nil && effect.UpdatePhase == nil && effect.UpdateNotes == nil && !closeReasonSet {
		return nil
	}
	value := func(pointer *string) any {
		if pointer == nil {
			return nil
		}
		return *pointer
	}
	flag := func(set bool) int {
		if set {
			return 1
		}
		return 0
	}
	var priority, phase any
	if effect.UpdatePriority != nil {
		priority = int(*effect.UpdatePriority)
	}
	if effect.UpdatePhase != nil {
		phase = int(*effect.UpdatePhase)
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET updated_at=?1,
		title=CASE WHEN ?2 THEN ?3 ELSE title END, description=CASE WHEN ?4 THEN ?5 ELSE description END,
		priority_id=CASE WHEN ?6 THEN ?7 ELSE priority_id END, phase_id=CASE WHEN ?8 THEN ?9 ELSE phase_id END,
		notes=CASE WHEN ?10 THEN ?11 ELSE notes END, close_reason=CASE WHEN ?12 THEN ?13 ELSE close_reason END WHERE id=?14`,
		recordedAt, flag(effect.UpdateTitle != nil), value(effect.UpdateTitle), flag(effect.UpdateDescription != nil), value(effect.UpdateDescription), flag(effect.UpdatePriority != nil), priority, flag(effect.UpdatePhase != nil), phase, flag(effect.UpdateNotes != nil), value(effect.UpdateNotes), flag(closeReasonSet), effect.CloseReason, effect.TaskID.String()); err != nil {
		return journalFoldError("task-event materialization", "the canonical task event could not update the task projection", "repair the tasks projection schema or referenced task, then retry", err)
	}
	return nil
}

func v1ProjectEdgeAdd(ctx context.Context, tx fusedtx.SQLTx, source journal.TaskID, target string, kind ptypes.EdgeKind, recordedAt int64) error {
	if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO edges (source_id,target_id,kind_id,created_at) VALUES (?1,?2,?3,?4)`, source.String(), target, int(kind), recordedAt); err != nil {
		return fmt.Errorf("project journal edge-add %s->%s: %w", source, target, err)
	}
	return nil
}

func v1InsertAttribution(ctx context.Context, tx fusedtx.SQLTx, task journal.TaskID, actor journal.ActorID, jid int64) error {
	if _, err := tx.Exec(ctx, `INSERT OR IGNORE INTO task_attributions (task_id,actor_id,first_journal_id) VALUES (?1,?2,?3)`, task.String(), actor.String(), jid); err != nil {
		return fmt.Errorf("attribute journal task event: %w", err)
	}
	return nil
}

func v1AdvanceWatermark(ctx context.Context, tx fusedtx.SQLTx, task journal.TaskID, jid int64) error {
	if _, err := tx.Exec(ctx, `UPDATE tasks SET last_journal_id=?1 WHERE id=?2`, jid, task.String()); err != nil {
		return fmt.Errorf("advance journal task watermark: %w", err)
	}
	return nil
}

func v1ValidateClosedEvents(ctx context.Context, tx fusedtx.SQLTx, effects []journal.Effect) error {
	for _, effect := range effects {
		if effect.Sort != journal.EffectTaskEvent || effect.EventKind != journal.EventKindTaskClosed {
			continue
		}
		var active int
		err := tx.QueryRow(ctx, `SELECT 1 FROM journal_authority_assignment_episodes e
			WHERE e.task_id=?1 AND e.slot_id=?2
			AND EXISTS (SELECT 1 FROM journal_authority_assignment_transitions s WHERE s.assignment_id=e.assignment_id AND s.transition_id=?3)
			AND NOT EXISTS (SELECT 1 FROM journal_authority_assignment_transitions x WHERE x.assignment_id=e.assignment_id AND x.transition_id=?4) LIMIT ?5`, effect.TaskID.String(), slotOwnerResponsibilityID, transitionStartedID, transitionEndedID, 1).Scan(&active)
		if err == nil {
			return fmt.Errorf("%w: journal task %q was closed but retains an active owner-responsibility assignment — why: close and assignment-end must be atomic; where: shared V1 journal close gate; when: before commit; impact: the operation returns no committed result and is rolled back; fix: end the active owner episode in the same operation as the close", journal.ErrCloseWithoutEnding, effect.TaskID.String())
		}
		if !fusedtx.IsNoRows(err) {
			return journalFoldError("active-owner close check", "active owner assignments could not be read", "repair assignment episode and transition rows, then retry", err)
		}
	}
	return nil
}

func v1RequireAuthorityGoverns(ctx context.Context, tx fusedtx.SQLTx, authority journal.JournalID, task journal.TaskID, before int64) error {
	governs, err := v1AuthorityGoverns(ctx, tx, authority, task, before)
	if err != nil {
		return err
	}
	if !governs {
		return fmt.Errorf("%w: journal authority %d does not govern task %q at journal position %d — why: the cited authority is absent, inactive, too new, or outside the task ancestry; where: shared V1 journal authority gate; when: before persisting the consuming effect; impact: the operation returns no committed result and is rolled back; fix: cite an earlier active authority that governs this task", journal.ErrAuthorityScope, authority, task.String(), before)
	}
	return nil
}

func v1AuthorityGoverns(ctx context.Context, tx fusedtx.SQLTx, authority journal.JournalID, task journal.TaskID, before int64) (bool, error) {
	if int64(authority) >= before {
		return false, nil
	}
	var kind int
	err := tx.QueryRow(ctx, `SELECT authority_kind_id FROM journal_authorities WHERE journal_id=?1`, int64(authority)).Scan(&kind)
	if fusedtx.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve journal authority %d: %w", authority, err)
	}
	if kind == int(journal.AuthorityKindBootstrap) {
		return true, nil
	}
	if kind != int(journal.AuthorityKindAssignment) {
		return false, nil
	}
	var authorityAssignment string
	if err := tx.QueryRow(ctx, `SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id=?1`, int64(authority)).Scan(&authorityAssignment); err != nil {
		if fusedtx.IsNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("resolve journal assignment authority %d: %w", authority, err)
	}
	active, err := v1EpisodeActiveAt(ctx, tx, journal.AssignmentID(authorityAssignment), before)
	if err != nil || !active {
		return false, err
	}
	var authorityTask string
	if err := tx.QueryRow(ctx, `SELECT task_id FROM journal_authority_assignment_episodes WHERE assignment_id=?1`, authorityAssignment).Scan(&authorityTask); err != nil {
		return false, err
	}
	parsedAuthorityTask, err := journalParseTask(authorityTask)
	if err != nil {
		return false, fmt.Errorf("parse durable task identity %q for journal assignment authority %d — where: shared V1 authority lookup; when: authorizing an effect; impact: authorization fails closed and nothing is committed; fix: repair the malformed task_id in journal_authority_assignment_episodes: %w", authorityTask, authority, err)
	}
	if parsedAuthorityTask == task {
		return true, nil
	}
	rows, err := tx.Query(ctx, `SELECT assignment_id FROM journal_authority_assignment_episodes WHERE task_id=?1`, task.String())
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var candidate string
		if err := rows.Scan(&candidate); err != nil {
			return false, err
		}
		candidateActive, err := v1EpisodeActiveAt(ctx, tx, journal.AssignmentID(candidate), before)
		if err != nil {
			return false, err
		}
		if !candidateActive {
			continue
		}
		reaches, err := v1ParentChainReaches(ctx, tx, journal.AssignmentID(candidate), journal.AssignmentID(authorityAssignment), before)
		if err != nil {
			return false, err
		}
		if reaches {
			return true, nil
		}
	}
	return false, rows.Err()
}

func v1EpisodeActiveAt(ctx context.Context, tx fusedtx.SQLTx, assignment journal.AssignmentID, before int64) (bool, error) {
	var started int
	err := tx.QueryRow(ctx, `SELECT 1 FROM journal_authority_assignment_transitions WHERE assignment_id=?1 AND transition_id=?2 AND journal_id<?3 LIMIT ?4`, string(assignment), transitionStartedID, before, 1).Scan(&started)
	if fusedtx.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var ended int
	err = tx.QueryRow(ctx, `SELECT 1 FROM journal_authority_assignment_transitions WHERE assignment_id=?1 AND transition_id=?2 AND journal_id<?3 LIMIT ?4`, string(assignment), transitionEndedID, before, 1).Scan(&ended)
	if fusedtx.IsNoRows(err) {
		return true, nil
	}
	return false, err
}

func v1ParentChainReaches(ctx context.Context, tx fusedtx.SQLTx, start, target journal.AssignmentID, before int64) (bool, error) {
	var maxSteps int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM journal_authority_assignment_episodes`).Scan(&maxSteps); err != nil {
		return false, fmt.Errorf("count journal authority episodes: %w", err)
	}
	seen := map[journal.AssignmentID]struct{}{}
	current := start
	for step := 0; ; step++ {
		if current == target {
			return true, nil
		}
		if _, duplicate := seen[current]; duplicate || step > maxSteps {
			return false, fmt.Errorf("%w: journal authority parent chain from %q revisits %q before reaching %q — where: ordinary journal authority traversal; when: authorizing an effect; impact: authorization fails closed and nothing is committed; fix: repair the stored parent_assignment_id chain", journal.ErrCorruptParentChain, start, current, target)
		}
		seen[current] = struct{}{}
		var parent *string
		err := tx.QueryRow(ctx, `SELECT parent_assignment_id FROM journal_authority_assignment_episodes WHERE assignment_id=?1`, string(current)).Scan(&parent)
		if fusedtx.IsNoRows(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read parent of journal authority episode %q while walking from %q to %q — where: shared V1 authority parent-chain traversal; when: authorizing an effect; impact: authorization fails closed and nothing is committed; fix: repair the authority episode store or retry after the database read failure: %w", current, start, target, err)
		}
		if parent == nil {
			return false, nil
		}
		current = journal.AssignmentID(*parent)
		active, err := v1EpisodeActiveAt(ctx, tx, current, before)
		if err != nil || !active {
			return false, err
		}
	}
}

// ---------------------------------------------------------------------------
// Replay identity + committed-result reconstruction (§3.2, §9.4)
// ---------------------------------------------------------------------------

type storedOperation struct {
	anchor   int64
	identity storedOperationReplayIdentity
}

func (scope *connScope) lookupOperation(op journal.OperationID) (storedOperation, bool, error) {
	out := storedOperation{}
	var authority *journal.JournalID
	var commandDigest, mutationDigest, canonicalMutation []byte
	var encodingVersion string
	var authorityRaw sql.NullInt64
	var encodingRaw sql.NullString
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT journal_id, authority_journal_id, command_digest, mutation_digest,\n\t\t        mutation_encoding_version, canonical_mutation\n\t\t FROM journal_operations WHERE operation_id = ?1", string(op)).Scan(&out.anchor, &authorityRaw, &commandDigest, &mutationDigest, &encodingRaw, &canonicalMutation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storedOperation{}, false, nil
		}
		return storedOperation{}, false, fmt.Errorf("lookup operation %q: %w", op, err)
	}
	commandDigest = append([]byte(nil), commandDigest...)
	mutationDigest = append([]byte(nil), mutationDigest...)
	canonicalMutation = append([]byte(nil), canonicalMutation...)
	if authorityRaw.Valid {
		a := journal.JournalID(authorityRaw.Int64)
		authority = &a
	}
	if encodingRaw.Valid {
		encodingVersion = encodingRaw.String
	}
	// The committing actor lives on the anchor journal row.
	var actor journal.ActorID
	var actorRaw string
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT actor_id FROM journal WHERE journal_id = ?1", out.anchor).Scan(&actorRaw); err != nil {
		return storedOperation{}, false, fmt.Errorf("lookup operation actor %q: %w", op, err)
	}
	actor, err := journalParseActor(actorRaw)
	if err != nil {
		return storedOperation{}, false, err
	}
	out.identity = newStoredOperationReplayIdentity(op, actor, authority, commandDigest, mutationDigest, encodingVersion, canonicalMutation)
	return out, true, nil
}

// reconcileAllocatedTaskCreates resolves only explicitly allocated-create
// provisional UUIDs from the already committed result slots. Fixed task_create
// effects never enter this path. Namespace, slot, order, and every non-UUID
// operand remain part of canonical replay identity.
func (scope *connScope) reconcileAllocatedTaskCreates(in journal.OperationInput, existing storedOperation) (journal.OperationInput, error) {
	hasAllocation := false
	for _, effect := range in.Effects {
		if effect.Sort == journal.EffectTaskCreateAllocated {
			hasAllocation = true
			break
		}
	}
	if !hasAllocation {
		return in, nil
	}
	committedMutation, err := decodeStoredOperationMutation(existing.identity)
	if err != nil {
		return journal.OperationInput{}, fmt.Errorf("reconcile allocated create for operation %q: decode committed mutation: %w", in.OperationID, err)
	}
	committedEffects := committedMutation.NormalizedEffects()
	if len(committedEffects) != len(in.Effects) {
		return in, nil
	}
	result, err := scope.reconstructAndValidateCommitted(existing.anchor)
	if err != nil {
		return journal.OperationInput{}, err
	}
	slots := make(map[journal.ResultSlotID]journal.ResultSlotBinding, len(result.ResultSlots))
	for _, binding := range result.ResultSlots {
		slots[binding.Slot] = binding
	}
	var produced []journal.JournalID
	if err := scope.queryRows("SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id", []any{existing.anchor}, func(rows *sql.Rows) error {
		var producedJID int64
		if err := rows.Scan(&producedJID); err != nil {
			return err
		}
		produced = append(produced, journal.JournalID(producedJID))
		return nil
	}); err != nil {
		return journal.OperationInput{}, err
	}
	if len(produced) != len(committedEffects) {
		return journal.OperationInput{}, fmt.Errorf("%w: allocated create operation %q has %d produced rows for %d canonical effects — where: replay allocation reconciliation; impact: retry fails closed without writes; fix: restore the operation rows and canonical mutation from the same committed backup", journal.ErrResultSlotIntegrity, in.OperationID, len(produced), len(committedEffects))
	}
	reconciled := append([]journal.Effect(nil), in.Effects...)
	for i := range reconciled {
		proposed, committed := reconciled[i], committedEffects[i]
		if proposed.Sort != journal.EffectTaskCreateAllocated {
			continue
		}
		if committed.Sort != journal.EffectTaskCreateAllocated || proposed.ResultSlot == "" || proposed.ResultSlot != committed.ResultSlot || proposed.TaskID.Namespace != committed.TaskID.Namespace {
			continue
		}
		binding, ok := slots[proposed.ResultSlot]
		if !ok || binding.Kind != journal.JournalKindTaskEvent || binding.TaskID == nil || *binding.TaskID != committed.TaskID || binding.ProducedJournalID != produced[i] {
			return journal.OperationInput{}, fmt.Errorf("%w: allocated create operation %q slot %q does not resolve to its canonical task %q — where: replay allocation reconciliation; impact: retry fails closed without writes; fix: restore the operation result slot and canonical mutation from the same committed backup", journal.ErrResultSlotIntegrity, in.OperationID, proposed.ResultSlot, committed.TaskID)
		}
		reconciled[i].TaskID.UUID = binding.TaskID.UUID
	}
	in.Effects = reconciled
	return in, nil
}

// committedOutcomeForExisting resolves the §9.4 outcome for an OperationID
// that already has a committed row. An exact four-field identity match returns the
// original committed result short-circuited (no re-execution, nil error). Any
// mismatch returns the closed-sum CommittedConflict variant carrying the typed
// *OperationConflict payload, alongside an error that wraps BOTH the
// ErrOperationConflict sentinel and the *OperationConflict value with %w — so a
// caller recovers it with errors.Is(err, ErrOperationConflict) or
// errors.As(err, &*OperationConflict), and a caller switching on res.Kind sees
// CommittedConflict (§11, §9.6). Shared by the Apply short-circuit and the
// concurrent-insert race translation so both surface the identical typed shape.
func (scope *connScope) committedOutcomeForExisting(in journal.OperationInput, existing storedOperation, callerMutationDigest []byte) (journal.CommittedResult, error) {
	err := compareStoredOperationIdentity(existing.identity, in, func(candidate journal.OperationInput) (journal.OperationInput, error) {
		return scope.reconcileAllocatedTaskCreates(candidate, existing)
	})
	if err != nil {
		var conflict *journal.OperationConflict
		if errors.As(err, &conflict) {
			return journal.CommittedResult{Kind: journal.CommittedConflict, Conflict: conflict}, err
		}
		return journal.CommittedResult{}, err
	}
	if err := scope.verifyFactContextIntegrity(); err != nil {
		return journal.CommittedResult{}, err
	}
	res, err := scope.reconstructAndValidateCommitted(existing.anchor)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	res.ShortCircuited = true
	return res, nil
}

// resolveOperationIDInsertRace implements §9.6's second bullet: when the
// anchor insert violates journal_operations.OperationID UNIQUE because a
// concurrent writer committed the same new OperationID first, the reducer catches
// that violation and re-runs the §9.4 idempotent-replay comparison against the
// now-committed row, returning the typed idempotent result or the typed
// CommittedConflict — never the raw SQLite constraint error. BEGIN IMMEDIATE makes
// this unreachable for cooperating Apply callers because the §9.4 lookup runs
// after write ownership is acquired; it remains defense in depth for a writer
// that bypasses that protocol.
func (scope *connScope) resolveOperationIDInsertRace(in journal.OperationInput, callerMutationDigest []byte) (journal.CommittedResult, error) {
	existing, found, err := scope.lookupOperation(in.OperationID)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	if !found {
		// The UNIQUE violation proved a row exists, but this transaction's read
		// snapshot cannot see it (the winning writer committed on another
		// connection after this transaction's snapshot began). Surface an actionable
		// integrity error rather than inventing an operand location that could not
		// be observed from this transaction's snapshot.
		return journal.CommittedResult{}, fmt.Errorf("%w: OperationID %q lost a concurrent insert but the winning row is not visible — where: insert-race structural replay; when: after UNIQUE rejection; impact: no caller conflict axis can be classified and nothing additional is committed; fix: retry after opening a fresh transaction so the winning canonical row can be compared", journal.ErrProjectionDivergence, in.OperationID)
	}
	return scope.committedOutcomeForExisting(in, existing, callerMutationDigest)
}

// LookupCommitted returns the committed result for an OperationID (§9.4): the
// closed Absent variant with no side effects for a never-applied operation, or
// the Exact variant with the reconstructed EmittedEvents closure and slot map.
func (db *DB) LookupCommitted(op journal.OperationID) (journal.CommittedResult, error) {
	if err := journal.ValidateOperationID(op); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("LookupCommitted: %w", err)
	}
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return journal.CommittedResult{}, fmt.Errorf("LookupCommitted: lease pooled connection: %w", err)
	}
	defer scope.release()
	stored, found, err := scope.lookupOperation(op)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	if !found {
		return journal.CommittedResult{Kind: journal.CommittedAbsent}, nil
	}
	return scope.reconstructAndValidateCommitted(stored.anchor)
}

// AuthorityGovernsTaskAt is the pure authorization predicate (§9.3, §14.1),
// exposed so the ordering-vs-authority corpus histories can assert that an
// authority committed after an effect (greater JournalID) never authorizes it,
// regardless of RecordedAt (§12).
func (db *DB) AuthorityGovernsTaskAt(authJID journal.JournalID, task journal.TaskID, beforeJID journal.JournalID) (bool, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return false, fmt.Errorf("AuthorityGovernsTaskAt: lease pooled connection: %w", err)
	}
	defer scope.release()
	return scope.authorityGovernsTaskAt(authJID, task, int64(beforeJID))
}

// CountAuthoritiesOfKind returns how many journal_authorities rows carry the
// given authority_kind_id. It is an audit/read helper (e.g. asserting a genesis
// retry created no second bootstrap authority).
func (db *DB) CountAuthoritiesOfKind(kind int) (int, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("CountAuthoritiesOfKind: lease pooled connection: %w", err)
	}
	defer scope.release()
	var n int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal_authorities WHERE authority_kind_id = ?1", kind).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountAuthoritiesOfKind %d: %w", kind, err)
	}
	return n, nil
}

// CountSuccessorEpisodes returns how many episodes on a task cite a predecessor
// (i.e. were created by a transfer). It is an audit/read helper used to prove a
// losing CAS transfer wrote nothing.
func (db *DB) CountSuccessorEpisodes(task journal.TaskID) (int, error) {
	scope, err := db.bindScope(context.Background(), projectionTargetLive)
	if err != nil {
		return 0, fmt.Errorf("CountSuccessorEpisodes: lease pooled connection: %w", err)
	}
	defer scope.release()
	var n int
	if err := scope.conn.QueryRowContext(scope.ctx, "SELECT COUNT(*) FROM journal_authority_assignment_episodes WHERE task_id = ?1 AND predecessor_assignment_id IS NOT ?2", task.String(), nil).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountSuccessorEpisodes %q: %w", task, err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Small utilities
// ---------------------------------------------------------------------------

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *moderncsqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case 1555, 2067: // SQLITE_CONSTRAINT_PRIMARYKEY, SQLITE_CONSTRAINT_UNIQUE
			return true
		}
	}
	return errors.Is(err, errUniqueSentinel)
}

func isForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *moderncsqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == 787 // SQLITE_CONSTRAINT_FOREIGNKEY
}

// errUniqueSentinel keeps the predicate total for an explicitly wrapped test or
// domain error without depending on driver error text.
var errUniqueSentinel = errors.New("unique constraint")
