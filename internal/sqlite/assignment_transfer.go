package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dayvidpham/provenance/internal/journal"
)

type foldOptions struct {
	faultHook          func(effectIndex int) error
	assignmentTransfer *assignmentTransferLease
}

// assignmentTransferLease is the transaction-local capability carried only by
// ApplyAssignmentTransfer. It can authorize no effect except the exact successor
// start after the canonical predecessor end has folded in the same operation.
type assignmentTransferLease struct {
	task                 journal.TaskID
	slot                 journal.AssignmentSlotID
	previous             journal.AssignmentID
	next                 journal.AssignmentID
	nextOccupant         journal.ActorID
	authority            journal.JournalID
	authenticated        bool
	predecessorEndFolded bool
	successorStartFolded bool
}

func assignmentTransferValidationError(field, reason, fix string) error {
	return &journal.CanonicalMutationError{
		Field:  "assignment-transfer." + field,
		Reason: reason,
		Fix:    fix,
	}
}

// newAssignmentTransferLease validates the exact closed transfer shape after
// the ordinary canonical codec has normalized it. No liveness read occurs here;
// replay/conflict admission must happen first inside BEGIN IMMEDIATE.
func newAssignmentTransferLease(in journal.OperationInput) (*assignmentTransferLease, error) {
	if len(in.Conditions) != 0 {
		return nil, assignmentTransferValidationError("conditions", "assignment transfer does not accept preconditions", "remove all conditions; predecessor liveness is authenticated transactionally by TransferAssignment")
	}
	if len(in.Effects) != 2 {
		return nil, assignmentTransferValidationError("effects", fmt.Sprintf("expected exactly two ordered effects, got %d", len(in.Effects)), "submit exactly End(previous) followed by Start(next with Predecessor=previous)")
	}
	end, start := in.Effects[0], in.Effects[1]
	if end.Sort != journal.EffectAssignmentEnd || start.Sort != journal.EffectAssignmentStart {
		return nil, assignmentTransferValidationError("effects", fmt.Sprintf("expected assignment_end then assignment_start, got %s then %s", end.Sort, start.Sort), "preserve the exact End(previous), Start(next) order")
	}
	if end.ResultSlot != "" || start.ResultSlot != "" || end.RecordedAtOverride != nil || start.RecordedAtOverride != nil {
		return nil, assignmentTransferValidationError("effects", "result slots and per-effect timestamps are not part of the semantic transfer shape", "leave result slots and RecordedAtOverride empty; TransferAssignment returns semantic IDs only")
	}
	if end.TaskID.Namespace == "" || start.TaskID.Namespace == "" || end.TaskID != start.TaskID {
		return nil, assignmentTransferValidationError("task", "both effects must name the same nonempty TaskID", "supply the predecessor episode's exact task once in AssignmentTransferRequest.TaskID")
	}
	if end.SlotID != journal.SlotOwnerResponsibility || start.SlotID != journal.SlotOwnerResponsibility || end.SlotID != start.SlotID {
		return nil, assignmentTransferValidationError("slot", fmt.Sprintf("slot must be the currently registered %q slot on both effects (got %q and %q)", journal.SlotOwnerResponsibility, end.SlotID, start.SlotID), "use SlotOwnerResponsibility; register and implement any future slot before transferring it")
	}
	if end.AssignmentID == "" || start.AssignmentID == "" {
		return nil, assignmentTransferValidationError("assignment", "previous and next assignment IDs must both be nonempty", "supply stable nonempty PreviousAssignmentID and NextAssignmentID values")
	}
	if end.AssignmentID == start.AssignmentID {
		return nil, assignmentTransferValidationError("assignment", fmt.Sprintf("previous and next assignment IDs are both %q", end.AssignmentID), "supply a distinct NextAssignmentID for the successor episode")
	}
	if start.Predecessor != end.AssignmentID {
		return nil, assignmentTransferValidationError("predecessor", fmt.Sprintf("successor predecessor %q does not equal ended assignment %q", start.Predecessor, end.AssignmentID), "set the successor Predecessor to PreviousAssignmentID")
	}
	if start.Parent != "" {
		return nil, assignmentTransferValidationError("parent", "assignment transfer cannot change parent-citation lineage", "leave Parent empty; transfer only succeeds the same task and slot episode")
	}
	if start.Occupant == (journal.ActorID{}) {
		return nil, assignmentTransferValidationError("next-occupant", "the successor occupant is empty", "supply a nonempty registered ActorID in NextOccupant")
	}
	return &assignmentTransferLease{
		task:         end.TaskID,
		slot:         end.SlotID,
		previous:     end.AssignmentID,
		next:         start.AssignmentID,
		nextOccupant: start.Occupant,
	}, nil
}

// ApplyAssignmentTransfer is the distinct SQLite write path used only by
// Session.TransferAssignment. It shares canonical preparation, replay identity,
// savepoint folding, projections, and result reconstruction with Apply.
func (db *DB) ApplyAssignmentTransfer(in journal.OperationInput) (journal.CommittedResult, error) {
	normalized, prepared, callerMutationDigest, err := prepareApplyInput(in)
	if err != nil {
		return journal.CommittedResult{}, err
	}
	lease, err := newAssignmentTransferLease(normalized)
	if err != nil {
		return journal.CommittedResult{}, fmt.Errorf("ApplyAssignmentTransfer: validate exact semantic shape before write ownership: %w", err)
	}
	return db.applyPreparedOperation(context.Background(), normalized, prepared, callerMutationDigest, foldOptions{assignmentTransfer: lease})
}

// authenticateAssignmentTransfer runs only after OperationID admission reports
// Absent. BEGIN IMMEDIATE already owns the write order, so this current-state
// check and the following two-effect fold are one serialized decision.
func (scope *connScope) authenticateAssignmentTransfer(in journal.OperationInput, lease *assignmentTransferLease) error {
	if in.AuthorityJournalID == nil {
		return fmt.Errorf("%w: assignment transfer operation %q has no Session authority — where: ApplyAssignmentTransfer authentication; when: after replay admission and before the operation anchor; impact: zero writes; fix: bind Session.As to the started transition JournalID of PreviousAssignmentID %q", journal.ErrAuthorityScope, in.OperationID, lease.previous)
	}
	matches, err := scope.assignmentTransferAuthorityMatches(*in.AuthorityJournalID, lease)
	if err != nil {
		return err
	}
	if !matches {
		return fmt.Errorf("%w: Session authority %d is not the started transition of predecessor assignment %q on task %q slot %q — where: ApplyAssignmentTransfer authentication; when: after replay admission and before the operation anchor; impact: zero writes; fix: resolve and bind the exact active predecessor start authority before retrying", journal.ErrAuthorityScope, *in.AuthorityJournalID, lease.previous, lease.task, lease.slot)
	}
	ended, exists, err := scope.episodeEnded(lease.previous)
	if err != nil {
		return fmt.Errorf("ApplyAssignmentTransfer: inspect predecessor %q liveness after replay admission: %w", lease.previous, err)
	}
	if !exists {
		return fmt.Errorf("%w: predecessor assignment %q disappeared after its authority was resolved — where: ApplyAssignmentTransfer authentication; when: inside BEGIN IMMEDIATE before folding; impact: zero writes; fix: repair the assignment episode/transition rows from a consistent backup", journal.ErrAuthorityScope, lease.previous)
	}
	if ended {
		return fmt.Errorf("%w: predecessor assignment %q is already ended — where: ApplyAssignmentTransfer liveness authentication; when: after replay admission inside BEGIN IMMEDIATE; impact: this losing transfer wrote nothing; fix: resolve the current active assignment and submit a new transfer operation", journal.ErrStaleEpisode, lease.previous)
	}
	lease.authority = *in.AuthorityJournalID
	lease.authenticated = true
	return nil
}

func (scope *connScope) assignmentTransferAuthorityMatches(authority journal.JournalID, lease *assignmentTransferLease) (bool, error) {
	var (
		kind, transition, slot int
		assignment, task       string
	)
	err := scope.conn.QueryRowContext(scope.ctx, `SELECT a.authority_kind_id,t.transition_id,t.assignment_id,e.task_id,e.slot_id
		FROM journal_authorities a
		JOIN journal_authority_assignment_transitions t ON t.journal_id=a.journal_id
		JOIN journal_authority_assignment_episodes e ON e.assignment_id=t.assignment_id
		WHERE a.journal_id=?1`, int64(authority)).Scan(&kind, &transition, &assignment, &task, &slot)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("ApplyAssignmentTransfer: resolve Session authority %d to its assignment transition: %w", authority, err)
	}
	wantSlot, err := slotDBID(lease.slot)
	if err != nil {
		return false, err
	}
	return kind == authKindAssignmentID &&
		transition == transitionStartedID &&
		journal.AssignmentID(assignment) == lease.previous &&
		task == lease.task.String() &&
		slot == wantSlot, nil
}

func (lease *assignmentTransferLease) permitsSuccessorStart(in journal.OperationInput, effect journal.Effect, index int) bool {
	return lease != nil && lease.authenticated && lease.predecessorEndFolded && !lease.successorStartFolded &&
		index == 1 && in.AuthorityJournalID != nil && *in.AuthorityJournalID == lease.authority &&
		effect.Sort == journal.EffectAssignmentStart && effect.TaskID == lease.task &&
		effect.SlotID == lease.slot && effect.AssignmentID == lease.next &&
		effect.Predecessor == lease.previous && effect.Occupant == lease.nextOccupant && effect.Parent == ""
}

func (lease *assignmentTransferLease) recordFoldedEffect(effect journal.Effect, index int) error {
	switch index {
	case 0:
		if effect.Sort != journal.EffectAssignmentEnd || effect.TaskID != lease.task || effect.SlotID != lease.slot || effect.AssignmentID != lease.previous {
			return assignmentTransferValidationError("lease", "the first folded effect did not match the authenticated predecessor end", "use the exact canonical transfer path without altering effects during the fold")
		}
		lease.predecessorEndFolded = true
	case 1:
		if !lease.permitsSuccessorStart(journal.OperationInput{AuthorityJournalID: &lease.authority}, effect, index) {
			return assignmentTransferValidationError("lease", "the second folded effect did not match the leased successor start", "use the exact canonical transfer path without altering effects during the fold")
		}
		lease.successorStartFolded = true
	default:
		return assignmentTransferValidationError("lease", fmt.Sprintf("unexpected folded effect index %d", index), "transfer exactly two effects")
	}
	return nil
}

func (lease *assignmentTransferLease) complete() bool {
	return lease != nil && lease.authenticated && lease.predecessorEndFolded && lease.successorStartFolded
}
