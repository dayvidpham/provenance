package sqlite

import (
	"context"
	"errors"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
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

func (scope *connScope) insertJournalRowLocked(kind journal.JournalKind, actor journal.ActorID, recordedAt int64, pboj *int64) (int64, error) {
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
	if err := sqlitex.Execute(scope.conn, "INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id) VALUES (?1, ?2, ?3, ?4)", &sqlitex.ExecOptions{Args: []any{int(kind), actorArg, recordedAt, pbojArg}}); err != nil {
		return 0, fmt.Errorf("insert journal row (kind %s): %w", kind, err)
	}
	return scope.conn.LastInsertRowID(), nil
}

func (db *DB) insertJournalRowLocked(kind journal.JournalKind, actor journal.ActorID, recordedAt int64, pboj *int64) (int64, error) {
	return borrowConnScope(db.conn, db.projectionTarget).insertJournalRowLocked(kind, actor, recordedAt, pboj)
}

func (scope *connScope) insertOperationRowLocked(anchor int64, in journal.OperationInput, prepared journal.CanonicalMutation) error {
	var authArg any
	if in.AuthorityJournalID != nil {
		authArg = int64(*in.AuthorityJournalID)
	}
	if err := sqlitex.Execute(scope.conn, "INSERT INTO journal_operations\n\t\t (journal_id, operation_id, authority_journal_id, command_digest, mutation_digest, mutation_encoding_version, canonical_mutation)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", &sqlitex.ExecOptions{Args: []any{anchor, string(in.OperationID), authArg, in.CommandDigest, prepared.DerivedDigest(), prepared.EncodingVersion().String(), prepared.CanonicalBytes()}}); err != nil {
		return fmt.Errorf("insert journal_operations for %q: %w", in.OperationID, err)
	}
	return nil
}

func (scope *connScope) insertAuthorityAssignmentTransitionLocked(jid int64, assignment journal.AssignmentID, transitionID int) error {
	opAuthID := fmt.Sprintf("authority--assignment--%d", jid)
	if err := sqlitex.Execute(scope.conn,
		insertJournalAuthoritySQL,
		&sqlitex.ExecOptions{Args: []any{jid, authKindAssignmentID, opAuthID}}); err != nil {
		return fmt.Errorf("insert journal_authorities (assignment): %w", err)
	}
	if err := sqlitex.Execute(scope.conn, "INSERT INTO journal_authority_assignment_transitions (journal_id, assignment_id, transition_id) VALUES (?1, ?2, ?3)", &sqlitex.ExecOptions{Args: []any{jid, string(assignment), transitionID}}); err != nil {
		return fmt.Errorf("insert assignment transition (%s): %w", journal.AssignmentTransition(transitionID), err)
	}
	return nil
}

func (db *DB) insertAuthorityAssignmentTransitionLocked(jid int64, assignment journal.AssignmentID, transitionID int) error {
	return borrowConnScope(db.conn, db.projectionTarget).insertAuthorityAssignmentTransitionLocked(jid, assignment, transitionID)
}

func (scope *connScope) insertResultSlotLocked(anchor int64, slot journal.ResultSlotID, producedJID int64) error {
	// rule 9 own-operation integrity (§3.2, §10 rule 9): the produced row must
	// have been produced by this same operation. Always holds on the normal path
	// (producedJID is a row this operation just inserted), enforced anyway.
	if err := scope.requireResultSlotOwnOperationLocked(anchor, producedJID); err != nil {
		return err
	}
	if err := sqlitex.Execute(scope.conn, "INSERT INTO journal_operation_result_slots (journal_id, result_slot_id, produced_journal_id) VALUES (?1, ?2, ?3)", &sqlitex.ExecOptions{Args: []any{anchor, string(slot), producedJID}}); err != nil {
		return fmt.Errorf("insert result slot %q: %w", slot, err)
	}
	return nil
}

func (scope *connScope) requireResultSlotOwnOperationLocked(anchor, producedJID int64) error {
	var producer int64
	var isNull = true
	if err := sqlitex.Execute(scope.conn, "SELECT produced_by_operation_journal_id FROM journal WHERE journal_id = ?1", &sqlitex.ExecOptions{Args: []any{producedJID}, ResultFunc: func(stmt *zs.Stmt) error {
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

func (db *DB) requireResultSlotOwnOperationLocked(anchor, producedJID int64) error {
	return borrowConnScope(db.conn, db.projectionTarget).requireResultSlotOwnOperationLocked(anchor, producedJID)
}

func (scope *connScope) insertAttributionLocked(task journal.TaskID, actor journal.ActorID, jid int64) error {
	// Targets the real task_attributions during a live Apply and the shadow
	// attribution table during a from-empty replay derivation (§8.2, §15).
	if err := sqlitex.Execute(scope.conn,
		scope.projectionTarget.insertAttributionQuery(),
		&sqlitex.ExecOptions{Args: []any{task.String(), actor.String(), jid}}); err != nil {
		return fmt.Errorf("update %s attribution: %w", scope.projectionTarget.label(), err)
	}
	return nil
}

func (db *DB) insertAttributionLocked(task journal.TaskID, actor journal.ActorID, jid int64) error {
	return borrowConnScope(db.conn, db.projectionTarget).insertAttributionLocked(task, actor, jid)
}

func (scope *connScope) advanceWatermarkLocked(task journal.TaskID, jid int64) error {
	// Targets the real tasks table during a live Apply and the shadow tasks table
	// during a from-empty replay derivation (§8.1, §15).
	if err := sqlitex.Execute(scope.conn,
		scope.projectionTarget.advanceWatermarkQuery(),
		&sqlitex.ExecOptions{Args: []any{jid, task.String()}}); err != nil {
		return fmt.Errorf("advance %s task watermark: %w", scope.projectionTarget.label(), err)
	}
	return nil
}

func (db *DB) advanceWatermarkLocked(task journal.TaskID, jid int64) error {
	return borrowConnScope(db.conn, db.projectionTarget).advanceWatermarkLocked(task, jid)
}

// recomputeTaskOwnerLocked materializes the owner-responsibility projection
// (§8.1): tasks.owner_id becomes the current active owner episode's occupant, or
// NULL when none is active. The watermark advances to jid. The SELECT reads the
// journal spine (the source of truth, untouched by replay); the UPDATE targets the
// projection table — real during Apply, shadow during replay (§15).
func (scope *connScope) recomputeTaskOwnerLocked(task journal.TaskID, jid int64) error {
	var owner any
	if err := sqlitex.Execute(scope.conn, "SELECT e.actor_id FROM journal_authority_assignment_episodes e\n\t\t JOIN journal_authority_assignment_transitions started\n\t\t   ON started.assignment_id = e.assignment_id AND started.transition_id = ?2\n\t\t WHERE e.task_id = ?1 AND e.slot_id = ?3\n\t\t   AND NOT EXISTS (SELECT ?5 FROM journal_authority_assignment_transitions ended\n\t\t                   WHERE ended.assignment_id = e.assignment_id AND ended.transition_id = ?4)\n\t\t ORDER BY started.journal_id DESC LIMIT ?6", &sqlitex.ExecOptions{
		Args:       []any{task.String(), transitionStartedID, slotOwnerResponsibilityID, transitionEndedID, 1, 1},
		ResultFunc: func(stmt *zs.Stmt) error { owner = stmt.ColumnText(0); return nil },
	}); err != nil {
		return fmt.Errorf("recompute task owner: %w", err)
	}
	if err := sqlitex.Execute(scope.conn,
		scope.projectionTarget.updateOwnerQuery(),
		&sqlitex.ExecOptions{Args: []any{owner, jid, task.String()}}); err != nil {
		return fmt.Errorf("update %s owner: %w", scope.projectionTarget.label(), err)
	}
	return nil
}

func (db *DB) recomputeTaskOwnerLocked(task journal.TaskID, jid int64) error {
	return borrowConnScope(db.conn, db.projectionTarget).recomputeTaskOwnerLocked(task, jid)
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

func (scope *connScope) episodeStartedLocked(assignment journal.AssignmentID) (bool, error) {
	return scope.transitionExistsLocked(assignment, transitionStartedID)
}

func (scope *connScope) episodeEndedLocked(assignment journal.AssignmentID) (ended bool, exists bool, err error) {
	exists, err = scope.episodeExistsLocked(assignment)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, false, nil
	}
	ended, err = scope.transitionExistsLocked(assignment, transitionEndedID)
	return ended, true, err
}

func (scope *connScope) episodeExistsLocked(assignment journal.AssignmentID) (bool, error) {
	found := false
	if err := sqlitex.Execute(scope.conn, "SELECT ?2 FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", &sqlitex.ExecOptions{Args: []any{string(assignment), 1}, ResultFunc: func(*zs.Stmt) error { found = true; return nil }}); err != nil {
		return false, fmt.Errorf("episode exists %q: %w", assignment, err)
	}
	return found, nil
}

func (scope *connScope) transitionExistsLocked(assignment journal.AssignmentID, transitionID int) (bool, error) {
	found := false
	if err := sqlitex.Execute(scope.conn, "SELECT ?3 FROM journal_authority_assignment_transitions WHERE assignment_id = ?1 AND transition_id = ?2", &sqlitex.ExecOptions{Args: []any{string(assignment), transitionID, 1}, ResultFunc: func(*zs.Stmt) error { found = true; return nil }}); err != nil {
		return false, fmt.Errorf("transition exists %q/%d: %w", assignment, transitionID, err)
	}
	return found, nil
}

func (db *DB) transitionExistsLocked(assignment journal.AssignmentID, transitionID int) (bool, error) {
	return borrowConnScope(db.conn, db.projectionTarget).transitionExistsLocked(assignment, transitionID)
}

func (scope *connScope) episodeTaskLocked(assignment journal.AssignmentID) (journal.TaskID, error) {
	var raw string
	if err := sqlitex.Execute(scope.conn, "SELECT task_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", &sqlitex.ExecOptions{Args: []any{string(assignment)}, ResultFunc: func(stmt *zs.Stmt) error { raw = stmt.ColumnText(0); return nil }}); err != nil {
		return journal.TaskID{}, fmt.Errorf("episode task %q: %w", assignment, err)
	}
	if raw == "" {
		return journal.TaskID{}, fmt.Errorf("episode %q has no task", assignment)
	}
	return journalParseTask(raw)
}

// episodeParentLocked returns the ParentAssignmentID citation of an episode
// (§4.4, §14.5). hasParent is false when the episode cites no parent (NULL
// parent_assignment_id) or does not exist.
func (scope *connScope) episodeParentLocked(assignment journal.AssignmentID) (parent journal.AssignmentID, hasParent bool, err error) {
	if execErr := sqlitex.Execute(scope.conn, "SELECT parent_assignment_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", &sqlitex.ExecOptions{Args: []any{string(assignment)}, ResultFunc: func(stmt *zs.Stmt) error {
		if stmt.ColumnType(0) != zs.TypeNull {
			parent = journal.AssignmentID(stmt.ColumnText(0))
			hasParent = parent != ""
		}
		return nil
	}}); execErr != nil {
		return "", false, fmt.Errorf("episode parent %q: %w", assignment, execErr)
	}
	return parent, hasParent, nil
}

// transitionExistsBeforeLocked reports whether the episode has the given
// transition committed at a journal position strictly before beforeJID (§14.5
// position-aware liveness). It is the position-scoped variant of
// transitionExistsLocked, which considers transitions at any position.
func (scope *connScope) transitionExistsBeforeLocked(assignment journal.AssignmentID, transitionID int, beforeJID int64) (bool, error) {
	found := false
	if err := sqlitex.Execute(scope.conn, "SELECT ?4 FROM journal_authority_assignment_transitions\n\t\t WHERE assignment_id = ?1 AND transition_id = ?2 AND journal_id < ?3 LIMIT ?5", &sqlitex.ExecOptions{Args: []any{string(assignment), transitionID, beforeJID, 1, 1}, ResultFunc: func(*zs.Stmt) error { found = true; return nil }}); err != nil {
		return false, fmt.Errorf("transition-before %q/%d < %d: %w", assignment, transitionID, beforeJID, err)
	}
	return found, nil
}

// episodeActiveAtLocked reports whether episode `assignment` is active at journal
// position beforeJID (§14.5 liveness): it has a started transition strictly before
// beforeJID and no ended transition strictly before beforeJID. "Active at effect
// time" — used both for the citation guard (at the start transition's own
// position) and for whole-chain liveness in the governance walk (at the consuming
// effect's position).
func (scope *connScope) episodeActiveAtLocked(assignment journal.AssignmentID, beforeJID int64) (bool, error) {
	started, err := scope.transitionExistsBeforeLocked(assignment, transitionStartedID, beforeJID)
	if err != nil {
		return false, err
	}
	if !started {
		return false, nil
	}
	ended, err := scope.transitionExistsBeforeLocked(assignment, transitionEndedID, beforeJID)
	if err != nil {
		return false, err
	}
	return !ended, nil
}

func (scope *connScope) taskHasActiveOwnerEpisodeLocked(task journal.TaskID) (bool, error) {
	found := false
	if err := sqlitex.Execute(scope.conn, "SELECT ?5 FROM journal_authority_assignment_episodes e\n\t\t WHERE e.task_id = ?1 AND e.slot_id = ?2\n\t\t   AND EXISTS (SELECT ?6 FROM journal_authority_assignment_transitions s WHERE s.assignment_id = e.assignment_id AND s.transition_id = ?3)\n\t\t   AND NOT EXISTS (SELECT ?7 FROM journal_authority_assignment_transitions x WHERE x.assignment_id = e.assignment_id AND x.transition_id = ?4)\n\t\t LIMIT ?8", &sqlitex.ExecOptions{
		Args:       []any{task.String(), slotOwnerResponsibilityID, transitionStartedID, transitionEndedID, 1, 1, 1, 1},
		ResultFunc: func(*zs.Stmt) error { found = true; return nil },
	}); err != nil {
		return false, fmt.Errorf("active owner episode %q: %w", task, err)
	}
	return found, nil
}

// ---------------------------------------------------------------------------
// Genesis + authority scope validation (§4.6, §9.3, §10 rules 6-7, §14.1)
// ---------------------------------------------------------------------------

func (scope *connScope) operationCountLocked() (int, error) {
	var n int
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM journal_operations", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("count operations: %w", err)
	}
	return n, nil
}

func (scope *connScope) validateGenesisLocked(in journal.OperationInput) error {
	count, err := scope.operationCountLocked()
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

func (scope *connScope) requireAuthorityExistsLocked(authJID journal.JournalID) error {
	found := false
	if err := sqlitex.Execute(scope.conn, "SELECT ?2 FROM journal_authorities WHERE journal_id = ?1", &sqlitex.ExecOptions{Args: []any{int64(authJID), 1}, ResultFunc: func(*zs.Stmt) error { found = true; return nil }}); err != nil {
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

func (db *DB) requireAuthorityExistsLocked(authJID journal.JournalID) error {
	return borrowConnScope(db.conn, db.projectionTarget).requireAuthorityExistsLocked(authJID)
}

// requireAuthorityGovernsLocked authorizes a task-bearing effect against the
// operation's authority at the effect's own JournalID (§9.3, §14.1). A genesis
// operation never reaches here (its sole effect is a bootstrap, task-free).
func (scope *connScope) requireAuthorityGovernsLocked(in journal.OperationInput, effectJID int64, task journal.TaskID) error {
	if in.AuthorityJournalID == nil {
		return fmt.Errorf(
			"%w: a task-bearing effect on %q requires a non-NULL authority (§4.6 restricts NULL "+
				"authority to a genesis operation's sole bootstrap effect)", journal.ErrGenesis, task)
	}
	governs, err := scope.authorityGovernsTaskAtLocked(*in.AuthorityJournalID, task, effectJID)
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

func (db *DB) requireAuthorityGovernsLocked(in journal.OperationInput, effectJID int64, task journal.TaskID) error {
	return borrowConnScope(db.conn, db.projectionTarget).requireAuthorityGovernsLocked(in, effectJID, task)
}

// authorityGovernsTaskAtLocked answers whether the authority at authJID governs
// targetTask for an effect committed at beforeJID (§9.3, §14.5): a bootstrap
// authority (the system root) governs every task; an assignment authority governs
// its own active episode's task PLUS every task whose episode reaches that episode
// via a chain of deliberate ParentAssignmentID citations, with the whole chain
// active at beforeJID. There is no edge-graph governance — a scheduling edge such
// as blocked_by carries no ownership semantics, so a task merely reachable through
// one is NOT governed (§14.1); only deliberate parent citations cross tasks. The
// authority must strictly precede the effect by JournalID (never by RecordedAt,
// §12).
func (scope *connScope) authorityGovernsTaskAtLocked(authJID journal.JournalID, targetTask journal.TaskID, beforeJID int64) (bool, error) {
	if int64(authJID) >= beforeJID {
		return false, nil // authority does not precede the effect (§9.3)
	}
	var kind = -1
	if err := sqlitex.Execute(scope.conn, "SELECT authority_kind_id FROM journal_authorities WHERE journal_id = ?1", &sqlitex.ExecOptions{Args: []any{int64(authJID)}, ResultFunc: func(stmt *zs.Stmt) error { kind = stmt.ColumnInt(0); return nil }}); err != nil {
		return false, fmt.Errorf("authority kind %d: %w", authJID, err)
	}
	switch kind {
	case authKindBootstrapID:
		return true, nil
	case authKindAssignmentID:
		return scope.assignmentAuthorityGovernsLocked(authJID, targetTask, beforeJID)
	default:
		return false, nil // unknown/absent authority governs nothing
	}
}

// assignmentAuthorityGovernsLocked implements the §14.5 governance predicate for
// an assignment authority at beforeJID: the authority's episode E governs
// targetTask when (a) E's own task is targetTask and E is active at beforeJID, or
// (b) some episode on targetTask reaches E by following ParentAssignmentID
// citations with EVERY episode on that chain — the child through each cited
// ancestor up to E — active at beforeJID. The walk is bounded and visited-tracked;
// a corrupted stored chain that cycles fails closed with ErrCorruptParentChain
// rather than looping.
func (scope *connScope) assignmentAuthorityGovernsLocked(authJID journal.JournalID, targetTask journal.TaskID, beforeJID int64) (bool, error) {
	// Resolve the assignment episode this authority (a transition row) belongs to.
	var authEpisode string
	if err := sqlitex.Execute(scope.conn, "SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id = ?1", &sqlitex.ExecOptions{Args: []any{int64(authJID)}, ResultFunc: func(stmt *zs.Stmt) error { authEpisode = stmt.ColumnText(0); return nil }}); err != nil {
		return false, fmt.Errorf("authority assignment %d: %w", authJID, err)
	}
	if authEpisode == "" {
		return false, nil
	}
	authAssignment := journal.AssignmentID(authEpisode)
	// The authority's own episode E must itself be active at the effect position:
	// an ended authority governs nothing, directly or by delegation.
	authActive, err := scope.episodeActiveAtLocked(authAssignment, beforeJID)
	if err != nil {
		return false, err
	}
	if !authActive {
		return false, nil
	}
	// (a) Direct: the authority's own episode task.
	authTask, err := scope.episodeTaskLocked(authAssignment)
	if err != nil {
		return false, err
	}
	if authTask.String() == targetTask.String() {
		return true, nil
	}
	// (b) Transitive: some active episode on targetTask reaches E by parent
	// citations, every episode on the chain active at beforeJID.
	starts, err := scope.episodesOnTaskLocked(targetTask)
	if err != nil {
		return false, err
	}
	for _, start := range starts {
		active, err := scope.episodeActiveAtLocked(start, beforeJID)
		if err != nil {
			return false, err
		}
		if !active {
			continue // an inactive child episode roots no live delegation chain
		}
		reached, err := scope.parentChainReachesLocked(start, authAssignment, beforeJID)
		if err != nil {
			return false, err // corrupted cyclic chain — fail closed (§14.5)
		}
		if reached {
			return true, nil
		}
	}
	return false, nil
}

// episodesOnTaskLocked returns every episode whose task is `task` (§14.5 walk
// entry points). A task typically has few episodes.
func (scope *connScope) episodesOnTaskLocked(task journal.TaskID) ([]journal.AssignmentID, error) {
	var out []journal.AssignmentID
	if err := sqlitex.Execute(scope.conn, "SELECT assignment_id FROM journal_authority_assignment_episodes WHERE task_id = ?1", &sqlitex.ExecOptions{Args: []any{task.String()}, ResultFunc: func(stmt *zs.Stmt) error {
		out = append(out, journal.AssignmentID(stmt.ColumnText(0)))
		return nil
	}}); err != nil {
		return nil, fmt.Errorf("episodes on task %q: %w", task, err)
	}
	return out, nil
}

// parentChainReachesLocked walks up the ParentAssignmentID chain from `start`,
// returning true when it reaches `target` with every intermediate cited ancestor
// active at beforeJID (the caller has already verified `start` and `target` are
// active). It fails closed with ErrCorruptParentChain when a cycle is detected
// (a revisited episode or a step past the bounded cap), so a corrupted stored
// chain halts authorization rather than looping (§14.5).
func (scope *connScope) parentChainReachesLocked(start, target journal.AssignmentID, beforeJID int64) (bool, error) {
	visited := map[journal.AssignmentID]struct{}{}
	cur := start
	// Defense-in-depth bound: the number of episodes is a hard ceiling on any
	// acyclic chain length; the visited set is the primary cycle guard.
	maxSteps, err := scope.countEpisodesLocked()
	if err != nil {
		return false, err
	}
	for step := 0; ; step++ {
		if cur == target {
			return true, nil
		}
		if _, seen := visited[cur]; seen || step > maxSteps {
			return false, fmt.Errorf(
				"%w: parent-citation walk from episode %q revisited %q before reaching %q — where: "+
					"§14.5 governance walk; when: during per-effect authorization; impact: authorization "+
					"fails closed and nothing is committed; fix: the stored parent_assignment_id chain is "+
					"corrupt (a cycle only reachable by bypassing the start-effect citation guard); repair "+
					"the journal", journal.ErrCorruptParentChain, start, cur, target)
		}
		visited[cur] = struct{}{}
		parent, hasParent, err := scope.episodeParentLocked(cur)
		if err != nil {
			return false, err
		}
		if !hasParent {
			return false, nil // reached a citation root that is not the target
		}
		// Each cited ancestor on the chain must be active at the effect position;
		// a chain broken by an ended middle episode delegates nothing past it.
		active, err := scope.episodeActiveAtLocked(parent, beforeJID)
		if err != nil {
			return false, err
		}
		if !active {
			return false, nil
		}
		cur = parent
	}
}

func (scope *connScope) countEpisodesLocked() (int, error) {
	var n int
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM journal_authority_assignment_episodes", &sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("count episodes: %w", err)
	}
	return n, nil
}

// requireParentCitationValidLocked validates an assignment-start's
// ParentAssignmentID citation (§14.5): the cited parent must exist and be active
// at this start transition's own journal position startJID, and the citation must
// not create a cycle. It returns nil for an empty (absent) citation.
func (scope *connScope) requireParentCitationValidLocked(newEpisode, parent journal.AssignmentID, startJID int64) error {
	if parent == "" {
		return nil
	}
	exists, err := scope.episodeExistsLocked(parent)
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
	active, err := scope.episodeActiveAtLocked(parent, startJID)
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
	return scope.requireNoParentCycleLocked(newEpisode, parent)
}

// requireNoParentCycleLocked walks the parent's existing ancestry and rejects a
// citation that would place newEpisode in its own ancestry (a cycle), or that
// traverses a pre-existing corrupt cycle (§14.5). Liveness is not consulted here —
// a cycle is a structural property of the stored chain.
func (scope *connScope) requireNoParentCycleLocked(newEpisode, parent journal.AssignmentID) error {
	visited := map[journal.AssignmentID]struct{}{}
	cur := parent
	maxSteps, err := scope.countEpisodesLocked()
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
		next, hasParent, err := scope.episodeParentLocked(cur)
		if err != nil {
			return err
		}
		if !hasParent {
			return nil
		}
		cur = next
	}
}

// validateClosesEndAssignmentsLocked rejects an operation that closes a task
// (a provenance.task.closed effect) while leaving an active owner-responsibility
// episode on it (§8.1 / owner_responsibility regression c): the close and the
// episode end must not drift apart.
func (scope *connScope) validateClosesEndAssignmentsLocked(anchor int64, effects []journal.Effect) error {
	for _, eff := range effects {
		if eff.Sort != journal.EffectTaskEvent || eff.EventKind != "provenance.task.closed" {
			continue
		}
		active, err := scope.taskHasActiveOwnerEpisodeLocked(eff.TaskID)
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
	identity storedOperationReplayIdentity
}

func (scope *connScope) lookupOperationLocked(op journal.OperationID) (storedOperation, bool, error) {
	out := storedOperation{}
	var authority *journal.JournalID
	var commandDigest, mutationDigest, canonicalMutation []byte
	var encodingVersion string
	found := false
	if err := sqlitex.Execute(scope.conn, "SELECT journal_id, authority_journal_id, command_digest, mutation_digest,\n\t\t        mutation_encoding_version, canonical_mutation\n\t\t FROM journal_operations WHERE operation_id = ?1", &sqlitex.ExecOptions{Args: []any{string(op)}, ResultFunc: func(stmt *zs.Stmt) error {
		found = true
		out.anchor = stmt.ColumnInt64(0)
		if stmt.ColumnType(1) != zs.TypeNull {
			a := journal.JournalID(stmt.ColumnInt64(1))
			authority = &a
		}
		commandDigest = readBlob(stmt, 2)
		mutationDigest = readBlob(stmt, 3)
		if stmt.ColumnType(4) != zs.TypeNull {
			encodingVersion = stmt.ColumnText(4)
		}
		if stmt.ColumnType(5) != zs.TypeNull {
			canonicalMutation = readBlob(stmt, 5)
		}
		return nil
	}}); err != nil {
		return storedOperation{}, false, fmt.Errorf("lookup operation %q: %w", op, err)
	}
	if !found {
		return storedOperation{}, false, nil
	}
	// The committing actor lives on the anchor journal row.
	var actor journal.ActorID
	if err := sqlitex.Execute(scope.conn, "SELECT actor_id FROM journal WHERE journal_id = ?1", &sqlitex.ExecOptions{Args: []any{out.anchor}, ResultFunc: func(stmt *zs.Stmt) error {
		parsed, err := journalParseActor(stmt.ColumnText(0))
		if err != nil {
			return err
		}
		actor = parsed
		return nil
	}}); err != nil {
		return storedOperation{}, false, fmt.Errorf("lookup operation actor %q: %w", op, err)
	}
	out.identity = newStoredOperationReplayIdentity(op, actor, authority, commandDigest, mutationDigest, encodingVersion, canonicalMutation)
	return out, true, nil
}

// reconcileAllocatedTaskCreatesLocked resolves only explicitly allocated-create
// provisional UUIDs from the already committed result slots. Fixed task_create
// effects never enter this path. Namespace, slot, order, and every non-UUID
// operand remain part of canonical replay identity.
func (scope *connScope) reconcileAllocatedTaskCreatesLocked(in journal.OperationInput, existing storedOperation) (journal.OperationInput, error) {
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
	result, err := scope.reconstructAndValidateCommittedLocked(existing.anchor)
	if err != nil {
		return journal.OperationInput{}, err
	}
	slots := make(map[journal.ResultSlotID]journal.ResultSlotBinding, len(result.ResultSlots))
	for _, binding := range result.ResultSlots {
		slots[binding.Slot] = binding
	}
	var produced []journal.JournalID
	if err := sqlitex.Execute(scope.conn, "SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id", &sqlitex.ExecOptions{Args: []any{existing.anchor}, ResultFunc: func(stmt *zs.Stmt) error {
		produced = append(produced, journal.JournalID(stmt.ColumnInt64(0)))
		return nil
	}}); err != nil {
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
func (scope *connScope) committedOutcomeForExistingLocked(in journal.OperationInput, existing storedOperation, callerMutationDigest []byte) (journal.CommittedResult, error) {
	err := compareStoredOperationIdentity(existing.identity, in, func(candidate journal.OperationInput) (journal.OperationInput, error) {
		return scope.reconcileAllocatedTaskCreatesLocked(candidate, existing)
	})
	if err != nil {
		var conflict *journal.OperationConflict
		if errors.As(err, &conflict) {
			return journal.CommittedResult{Kind: journal.CommittedConflict, Conflict: conflict}, err
		}
		return journal.CommittedResult{}, err
	}
	res, err := scope.reconstructAndValidateCommittedLocked(existing.anchor)
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
// CommittedConflict — never the raw SQLite constraint error. BEGIN IMMEDIATE makes
// this unreachable for cooperating Apply callers because the §9.4 lookup runs
// after write ownership is acquired; it remains defense in depth for a writer
// that bypasses that protocol.
func (scope *connScope) resolveOperationIDInsertRaceLocked(in journal.OperationInput, callerMutationDigest []byte) (journal.CommittedResult, error) {
	existing, found, err := scope.lookupOperationLocked(in.OperationID)
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
	return scope.committedOutcomeForExistingLocked(in, existing, callerMutationDigest)
}

func (db *DB) resolveOperationIDInsertRaceLocked(in journal.OperationInput, callerMutationDigest []byte) (journal.CommittedResult, error) {
	return borrowConnScope(db.conn, db.projectionTarget).resolveOperationIDInsertRaceLocked(in, callerMutationDigest)
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
	stored, found, err := scope.lookupOperationLocked(op)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	if !found {
		return journal.CommittedResult{Kind: journal.CommittedAbsent}, nil
	}
	return scope.reconstructAndValidateCommittedLocked(stored.anchor)
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
	return scope.authorityGovernsTaskAtLocked(authJID, task, int64(beforeJID))
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
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM journal_authorities WHERE authority_kind_id = ?1", &sqlitex.ExecOptions{Args: []any{kind}, ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
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
	if err := sqlitex.Execute(scope.conn, "SELECT COUNT(*) FROM journal_authority_assignment_episodes WHERE task_id = ?1 AND predecessor_assignment_id IS NOT ?2", &sqlitex.ExecOptions{Args: []any{task.String(), nil}, ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
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
