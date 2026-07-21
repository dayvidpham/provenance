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
	if err := executeStatement(db.conn,
		sqlStatement142,
		&sqlitex.ExecOptions{Args: []any{int(kind), actorArg, recordedAt, pbojArg}}); err != nil {
		return 0, fmt.Errorf("insert journal row (kind %s): %w", kind, err)
	}
	return db.conn.LastInsertRowID(), nil
}

func (db *DB) insertOperationRowLocked(anchor int64, in journal.OperationInput, prepared journal.CanonicalMutation) error {
	var authArg any
	if in.AuthorityJournalID != nil {
		authArg = int64(*in.AuthorityJournalID)
	}
	if err := executeStatement(db.conn,
		sqlStatement156,
		&sqlitex.ExecOptions{Args: []any{anchor, string(in.OperationID), authArg, in.CommandDigest, prepared.DerivedDigest(), prepared.EncodingVersion().String(), prepared.CanonicalBytes()}}); err != nil {
		return fmt.Errorf("insert journal_operations for %q: %w", in.OperationID, err)
	}
	return nil
}

func (db *DB) insertAuthorityAssignmentTransitionLocked(jid int64, assignment journal.AssignmentID, transitionID int) error {
	opAuthID := fmt.Sprintf("authority--assignment--%d", jid)
	if err := executeStatement(db.conn,
		sqlStatement131,
		&sqlitex.ExecOptions{Args: []any{jid, authKindAssignmentID, opAuthID}}); err != nil {
		return fmt.Errorf("insert journal_authorities (assignment): %w", err)
	}
	if err := executeStatement(db.conn,
		sqlStatement144,
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
	if err := executeStatement(db.conn,
		sqlStatement157,
		&sqlitex.ExecOptions{Args: []any{anchor, string(slot), producedJID}}); err != nil {
		return fmt.Errorf("insert result slot %q: %w", slot, err)
	}
	return nil
}

func (db *DB) requireResultSlotOwnOperationLocked(anchor, producedJID int64) error {
	var producer int64
	var isNull = true
	if err := executeStatement(db.conn,
		sqlStatement158,
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
	// Targets the real task_attributions during a live Apply and the shadow
	// attribution table during a from-empty replay derivation (§8.2, §15).
	if err := executeStatement(db.conn,
		db.projectionTarget.insertAttributionStatement(),
		&sqlitex.ExecOptions{Args: []any{task.String(), actor.String(), jid}}); err != nil {
		return fmt.Errorf("update %s attribution: %w", db.projectionTarget.label(), err)
	}
	return nil
}

func (db *DB) advanceWatermarkLocked(task journal.TaskID, jid int64) error {
	// Targets the real tasks table during a live Apply and the shadow tasks table
	// during a from-empty replay derivation (§8.1, §15).
	if err := executeStatement(db.conn,
		db.projectionTarget.advanceWatermarkStatement(),
		&sqlitex.ExecOptions{Args: []any{jid, task.String()}}); err != nil {
		return fmt.Errorf("advance %s task watermark: %w", db.projectionTarget.label(), err)
	}
	return nil
}

// recomputeTaskOwnerLocked materializes the owner-responsibility projection
// (§8.1): tasks.owner_id becomes the current active owner episode's occupant, or
// NULL when none is active. The watermark advances to jid. The SELECT reads the
// journal spine (the source of truth, untouched by replay); the UPDATE targets the
// projection table — real during Apply, shadow during replay (§15).
func (db *DB) recomputeTaskOwnerLocked(task journal.TaskID, jid int64) error {
	var owner any
	if err := executeStatement(db.conn,
		sqlStatement159,
		&sqlitex.ExecOptions{
			Args:       []any{task.String(), transitionStartedID, slotOwnerResponsibilityID, transitionEndedID},
			ResultFunc: func(stmt *zs.Stmt) error { owner = stmt.ColumnText(0); return nil },
		}); err != nil {
		return fmt.Errorf("recompute task owner: %w", err)
	}
	if err := executeStatement(db.conn,
		db.projectionTarget.updateOwnerStatement(),
		&sqlitex.ExecOptions{Args: []any{owner, jid, task.String()}}); err != nil {
		return fmt.Errorf("update %s owner: %w", db.projectionTarget.label(), err)
	}
	return nil
}

func (target projectionTarget) insertAttributionStatement() sqlStatement {
	if target == projectionTargetShadow {
		return sqlStatement160
	}
	return sqlStatement161
}

func (target projectionTarget) advanceWatermarkStatement() sqlStatement {
	if target == projectionTargetShadow {
		return sqlStatement162
	}
	return sqlStatement055
}

func (target projectionTarget) updateOwnerStatement() sqlStatement {
	if target == projectionTargetShadow {
		return sqlStatement163
	}
	return sqlStatement164
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
	if err := executeStatement(db.conn,
		sqlStatement165,
		&sqlitex.ExecOptions{Args: []any{string(assignment)}, ResultFunc: func(*zs.Stmt) error { found = true; return nil }}); err != nil {
		return false, fmt.Errorf("episode exists %q: %w", assignment, err)
	}
	return found, nil
}

func (db *DB) transitionExistsLocked(assignment journal.AssignmentID, transitionID int) (bool, error) {
	found := false
	if err := executeStatement(db.conn,
		sqlStatement166,
		&sqlitex.ExecOptions{Args: []any{string(assignment), transitionID}, ResultFunc: func(*zs.Stmt) error { found = true; return nil }}); err != nil {
		return false, fmt.Errorf("transition exists %q/%d: %w", assignment, transitionID, err)
	}
	return found, nil
}

func (db *DB) episodeTaskLocked(assignment journal.AssignmentID) (journal.TaskID, error) {
	var raw string
	if err := executeStatement(db.conn,
		sqlStatement167,
		&sqlitex.ExecOptions{Args: []any{string(assignment)}, ResultFunc: func(stmt *zs.Stmt) error { raw = stmt.ColumnText(0); return nil }}); err != nil {
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
func (db *DB) episodeParentLocked(assignment journal.AssignmentID) (parent journal.AssignmentID, hasParent bool, err error) {
	if execErr := executeStatement(db.conn,
		sqlStatement168,
		&sqlitex.ExecOptions{Args: []any{string(assignment)}, ResultFunc: func(stmt *zs.Stmt) error {
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
func (db *DB) transitionExistsBeforeLocked(assignment journal.AssignmentID, transitionID int, beforeJID int64) (bool, error) {
	found := false
	if err := executeStatement(db.conn,
		sqlStatement169,
		&sqlitex.ExecOptions{Args: []any{string(assignment), transitionID, beforeJID}, ResultFunc: func(*zs.Stmt) error { found = true; return nil }}); err != nil {
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
func (db *DB) episodeActiveAtLocked(assignment journal.AssignmentID, beforeJID int64) (bool, error) {
	started, err := db.transitionExistsBeforeLocked(assignment, transitionStartedID, beforeJID)
	if err != nil {
		return false, err
	}
	if !started {
		return false, nil
	}
	ended, err := db.transitionExistsBeforeLocked(assignment, transitionEndedID, beforeJID)
	if err != nil {
		return false, err
	}
	return !ended, nil
}

func (db *DB) taskHasActiveOwnerEpisodeLocked(task journal.TaskID) (bool, error) {
	found := false
	if err := executeStatement(db.conn,
		sqlStatement170,
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
	if err := executeStatement(db.conn, sqlStatement171,
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
	if err := executeStatement(db.conn,
		sqlStatement172,
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
// targetTask for an effect committed at beforeJID (§9.3, §14.5): a bootstrap
// authority (the system root) governs every task; an assignment authority governs
// its own active episode's task PLUS every task whose episode reaches that episode
// via a chain of deliberate ParentAssignmentID citations, with the whole chain
// active at beforeJID. There is no edge-graph governance — a scheduling edge such
// as blocked_by carries no ownership semantics, so a task merely reachable through
// one is NOT governed (§14.1); only deliberate parent citations cross tasks. The
// authority must strictly precede the effect by JournalID (never by RecordedAt,
// §12).
func (db *DB) authorityGovernsTaskAtLocked(authJID journal.JournalID, targetTask journal.TaskID, beforeJID int64) (bool, error) {
	if int64(authJID) >= beforeJID {
		return false, nil // authority does not precede the effect (§9.3)
	}
	var kind = -1
	if err := executeStatement(db.conn,
		sqlStatement173,
		&sqlitex.ExecOptions{Args: []any{int64(authJID)}, ResultFunc: func(stmt *zs.Stmt) error { kind = stmt.ColumnInt(0); return nil }}); err != nil {
		return false, fmt.Errorf("authority kind %d: %w", authJID, err)
	}
	switch kind {
	case authKindBootstrapID:
		return true, nil
	case authKindAssignmentID:
		return db.assignmentAuthorityGovernsLocked(authJID, targetTask, beforeJID)
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
func (db *DB) assignmentAuthorityGovernsLocked(authJID journal.JournalID, targetTask journal.TaskID, beforeJID int64) (bool, error) {
	// Resolve the assignment episode this authority (a transition row) belongs to.
	var authEpisode string
	if err := executeStatement(db.conn,
		sqlStatement174,
		&sqlitex.ExecOptions{Args: []any{int64(authJID)}, ResultFunc: func(stmt *zs.Stmt) error { authEpisode = stmt.ColumnText(0); return nil }}); err != nil {
		return false, fmt.Errorf("authority assignment %d: %w", authJID, err)
	}
	if authEpisode == "" {
		return false, nil
	}
	authAssignment := journal.AssignmentID(authEpisode)
	// The authority's own episode E must itself be active at the effect position:
	// an ended authority governs nothing, directly or by delegation.
	authActive, err := db.episodeActiveAtLocked(authAssignment, beforeJID)
	if err != nil {
		return false, err
	}
	if !authActive {
		return false, nil
	}
	// (a) Direct: the authority's own episode task.
	authTask, err := db.episodeTaskLocked(authAssignment)
	if err != nil {
		return false, err
	}
	if authTask.String() == targetTask.String() {
		return true, nil
	}
	// (b) Transitive: some active episode on targetTask reaches E by parent
	// citations, every episode on the chain active at beforeJID.
	starts, err := db.episodesOnTaskLocked(targetTask)
	if err != nil {
		return false, err
	}
	for _, start := range starts {
		active, err := db.episodeActiveAtLocked(start, beforeJID)
		if err != nil {
			return false, err
		}
		if !active {
			continue // an inactive child episode roots no live delegation chain
		}
		reached, err := db.parentChainReachesLocked(start, authAssignment, beforeJID)
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
func (db *DB) episodesOnTaskLocked(task journal.TaskID) ([]journal.AssignmentID, error) {
	var out []journal.AssignmentID
	if err := executeStatement(db.conn,
		sqlStatement175,
		&sqlitex.ExecOptions{Args: []any{task.String()}, ResultFunc: func(stmt *zs.Stmt) error {
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
func (db *DB) parentChainReachesLocked(start, target journal.AssignmentID, beforeJID int64) (bool, error) {
	visited := map[journal.AssignmentID]struct{}{}
	cur := start
	// Defense-in-depth bound: the number of episodes is a hard ceiling on any
	// acyclic chain length; the visited set is the primary cycle guard.
	maxSteps, err := db.countEpisodesLocked()
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
		parent, hasParent, err := db.episodeParentLocked(cur)
		if err != nil {
			return false, err
		}
		if !hasParent {
			return false, nil // reached a citation root that is not the target
		}
		// Each cited ancestor on the chain must be active at the effect position;
		// a chain broken by an ended middle episode delegates nothing past it.
		active, err := db.episodeActiveAtLocked(parent, beforeJID)
		if err != nil {
			return false, err
		}
		if !active {
			return false, nil
		}
		cur = parent
	}
}

func (db *DB) countEpisodesLocked() (int, error) {
	var n int
	if err := executeStatement(db.conn, sqlStatement176,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *zs.Stmt) error { n = stmt.ColumnInt(0); return nil }}); err != nil {
		return 0, fmt.Errorf("count episodes: %w", err)
	}
	return n, nil
}

// requireParentCitationValidLocked validates an assignment-start's
// ParentAssignmentID citation (§14.5): the cited parent must exist and be active
// at this start transition's own journal position startJID, and the citation must
// not create a cycle. It returns nil for an empty (absent) citation.
func (db *DB) requireParentCitationValidLocked(newEpisode, parent journal.AssignmentID, startJID int64) error {
	if parent == "" {
		return nil
	}
	exists, err := db.episodeExistsLocked(parent)
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
	active, err := db.episodeActiveAtLocked(parent, startJID)
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
	return db.requireNoParentCycleLocked(newEpisode, parent)
}

// requireNoParentCycleLocked walks the parent's existing ancestry and rejects a
// citation that would place newEpisode in its own ancestry (a cycle), or that
// traverses a pre-existing corrupt cycle (§14.5). Liveness is not consulted here —
// a cycle is a structural property of the stored chain.
func (db *DB) requireNoParentCycleLocked(newEpisode, parent journal.AssignmentID) error {
	visited := map[journal.AssignmentID]struct{}{}
	cur := parent
	maxSteps, err := db.countEpisodesLocked()
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
		next, hasParent, err := db.episodeParentLocked(cur)
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
	anchor            int64
	identity          journal.StoredOperationIdentity
	encodingVersion   string
	canonicalMutation []byte
}

func (db *DB) lookupOperationLocked(op journal.OperationID) (storedOperation, bool, error) {
	var out storedOperation
	found := false
	if err := executeStatement(db.conn,
		sqlStatement177,
		&sqlitex.ExecOptions{Args: []any{string(op)}, ResultFunc: func(stmt *zs.Stmt) error {
			found = true
			out.anchor = stmt.ColumnInt64(0)
			if stmt.ColumnType(1) != zs.TypeNull {
				a := journal.JournalID(stmt.ColumnInt64(1))
				out.identity.AuthorityJournalID = &a
			}
			out.identity.CommandDigest = readBlob(stmt, 2)
			out.identity.MutationDigest = readBlob(stmt, 3)
			if stmt.ColumnType(4) != zs.TypeNull {
				out.encodingVersion = stmt.ColumnText(4)
			}
			if stmt.ColumnType(5) != zs.TypeNull {
				out.canonicalMutation = readBlob(stmt, 5)
			}
			return nil
		}}); err != nil {
		return storedOperation{}, false, fmt.Errorf("lookup operation %q: %w", op, err)
	}
	if !found {
		return storedOperation{}, false, nil
	}
	// The committing actor lives on the anchor journal row.
	if err := executeStatement(db.conn,
		sqlStatement178,
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

// reconcileAllocatedTaskCreatesLocked resolves only explicitly allocated-create
// provisional UUIDs from the already committed result slots. Fixed task_create
// effects never enter this path. Namespace, slot, order, and every non-UUID
// operand remain part of canonical replay identity.
func (db *DB) reconcileAllocatedTaskCreatesLocked(in journal.OperationInput, existing storedOperation) (journal.OperationInput, error) {
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
	committedMutation, err := journal.DecodeCanonicalMutation(existing.canonicalMutation)
	if err != nil {
		return journal.OperationInput{}, fmt.Errorf("reconcile allocated create for operation %q: decode committed mutation: %w", in.OperationID, err)
	}
	committedEffects := committedMutation.NormalizedEffects()
	if len(committedEffects) != len(in.Effects) {
		return in, nil
	}
	result, err := db.reconstructCommittedLocked(existing.anchor)
	if err != nil {
		return journal.OperationInput{}, err
	}
	slots := make(map[journal.ResultSlotID]journal.ResultSlotBinding, len(result.ResultSlots))
	for _, binding := range result.ResultSlots {
		slots[binding.Slot] = binding
	}
	var produced []journal.JournalID
	if err := executeStatement(db.conn, sqlStatement179, &sqlitex.ExecOptions{Args: []any{existing.anchor}, ResultFunc: func(stmt *zs.Stmt) error {
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
func (db *DB) committedOutcomeForExistingLocked(in journal.OperationInput, existing storedOperation, callerMutationDigest []byte) (journal.CommittedResult, error) {
	var prepared journal.CanonicalMutation
	if existing.encodingVersion != "" {
		var err error
		in, err = db.reconcileAllocatedTaskCreatesLocked(in, existing)
		if err != nil {
			return journal.CommittedResult{}, err
		}
		prepared, err = journal.PrepareMutationV1(in.Effects)
		if err != nil {
			return journal.CommittedResult{}, err
		}
		in.MutationDigest = prepared.DerivedDigest()
	}
	proposedMutationDigest := in.MutationDigest
	if existing.encodingVersion == "" {
		proposedMutationDigest = callerMutationDigest
		if len(proposedMutationDigest) == 0 {
			proposedMutationDigest = in.MutationDigest
		}
	}
	if field, ok := identityMismatch(existing.identity, journal.StoredOperationIdentity{
		ActorID:            in.ActorID,
		AuthorityJournalID: in.AuthorityJournalID,
		CommandDigest:      in.CommandDigest,
		MutationDigest:     proposedMutationDigest,
	}); !ok {
		conflict := &journal.OperationConflict{OperationID: in.OperationID, Field: field}
		return journal.CommittedResult{Kind: journal.CommittedConflict, Conflict: conflict},
			fmt.Errorf("%w: %w", journal.ErrOperationConflict, conflict)
	}
	if existing.encodingVersion != "" {
		if existing.encodingVersion != prepared.EncodingVersion().String() || !bytes.Equal(existing.canonicalMutation, prepared.CanonicalBytes()) {
			conflict := &journal.OperationConflict{OperationID: in.OperationID, Field: "canonical effects"}
			return journal.CommittedResult{Kind: journal.CommittedConflict, Conflict: conflict},
				fmt.Errorf("%w: %w", journal.ErrOperationConflict, conflict)
		}
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
func (db *DB) resolveOperationIDInsertRaceLocked(in journal.OperationInput, callerMutationDigest []byte) (journal.CommittedResult, error) {
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
	return db.committedOutcomeForExistingLocked(in, existing, callerMutationDigest)
}

func (db *DB) reconstructCommittedLocked(anchor int64) (journal.CommittedResult, error) {
	res := journal.CommittedResult{Kind: journal.CommittedExact, AnchorJournalID: journal.JournalID(anchor)}
	// EmittedEvents: the flat task_event closure in JournalID order (§2.1, §3.2).
	if err := executeStatement(db.conn,
		sqlStatement180,
		&sqlitex.ExecOptions{Args: []any{anchor, int(journal.JournalKindTaskEvent)}, ResultFunc: func(stmt *zs.Stmt) error {
			res.EmittedEvents = append(res.EmittedEvents, journal.JournalID(stmt.ColumnInt64(0)))
			return nil
		}}); err != nil {
		return journal.CommittedResult{}, fmt.Errorf("reconstruct emitted events: %w", err)
	}
	// Slot-keyed result map (§3.2), bucketed by JournalKind.
	if err := executeStatement(db.conn,
		sqlStatement181,
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
	if err := executeStatement(db.conn,
		sqlStatement182,
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
	if err := executeStatement(db.conn,
		sqlStatement183,
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
