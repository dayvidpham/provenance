package journal

import "fmt"

// ConflictAxis is the closed five-value discriminator on OperationConflict (§11).
// All five values are nonzero. Index is -1 for scalar axes (Actor/Authority/Command)
// and ≥ 0 for the first differing collection element (Condition or Effect).
type ConflictAxis uint8

const (
	ConflictActor     ConflictAxis = iota + 1 // nonzero: committing actor differs
	ConflictAuthority                          // authority JournalID differs
	ConflictCommand                            // command digest differs
	ConflictCondition                          // condition list length or element differs
	ConflictEffect                             // effect list length or element differs
)

var conflictAxisNames = [...]string{0: "<invalid>", 1: "Actor", 2: "Authority", 3: "Command", 4: "Condition", 5: "Effect"}

func (a ConflictAxis) String() string {
	if int(a) < len(conflictAxisNames) && conflictAxisNames[a] != "" {
		return conflictAxisNames[a]
	}
	return fmt.Sprintf("ConflictAxis(%d)", a)
}

// ConflictAxes returns the closed set of five axes in declaration order.
func ConflictAxes() []ConflictAxis {
	return []ConflictAxis{ConflictActor, ConflictAuthority, ConflictCommand, ConflictCondition, ConflictEffect}
}

// OperationConflict is the typed conflict returned when an OperationID is reused
// with a differing identity (§11), or when a concurrent racer loses the OperationID
// insert with a non-matching identity (§9.6). It is the payload of CommittedConflict.
//
// Index is -1 for scalar axes (Actor, Authority, Command).
// Index ≥ 0 identifies the collection element (Condition or Effect) that first differed,
// or -1 when the collection lengths differ.
type OperationConflict struct {
	OperationID OperationID
	Axis        ConflictAxis
	// Index is -1 for scalar axes or collection-length mismatch; ≥ 0 for element diff.
	Index int
}

func (c *OperationConflict) Error() string {
	if c.Index < 0 {
		return fmt.Sprintf(
			"%v: OperationID %q differs on %s axis — "+
				"where: Apply exact replay comparison; when: before writes; "+
				"impact: nothing was committed; "+
				"fix: retry with the identical actor, authority, command digest, and "+
				"canonical mutation, or use a new OperationID for a different operation",
			ErrOperationConflict, c.OperationID, c.Axis)
	}
	return fmt.Sprintf(
		"%v: OperationID %q differs on %s[%d] — "+
			"where: Apply exact replay comparison; when: before writes; "+
			"impact: nothing was committed; "+
			"fix: retry with the identical actor, authority, command digest, and "+
			"canonical mutation, or use a new OperationID for a different operation",
		ErrOperationConflict, c.OperationID, c.Axis, c.Index)
}

func (c *OperationConflict) Is(target error) bool { return target == ErrOperationConflict }
func (c *OperationConflict) Unwrap() error        { return ErrOperationConflict }

// ConditionFailure is the typed failure returned when a pre-condition on the
// Apply write path is not satisfied (§9.5). Index identifies which condition
// failed; AssertedJournalID and ActualJournalID let the caller diagnose drift.
type ConditionFailure struct {
	Index             int
	Kind              ConditionKind
	Reason            ConditionFailureReason
	AssertedJournalID JournalID
	ActualJournalID   JournalID
}

func (e *ConditionFailure) Error() string {
	return fmt.Sprintf(
		"%v: condition[%d] kind=%s asserted journal row %d, observed %d (reason=%s) — "+
			"where: Apply transaction-local condition evaluation; when: after exact replay lookup "+
			"and before writes; impact: the operation was not committed; "+
			"fix: refresh the selected fact and retry with its current JournalID",
		ErrConditionFailed, e.Index, e.Kind, e.AssertedJournalID, e.ActualJournalID, e.Reason)
}

func (e *ConditionFailure) Is(target error) bool { return target == ErrConditionFailed }
func (e *ConditionFailure) Unwrap() error        { return ErrConditionFailed }

// ActivityConflict is the typed conflict returned when an ActivityCreate fold
// finds the ActivityID already committed. Callers distinguish this from a
// general rollback so they can retry with the original OperationID or a new ActivityID.
type ActivityConflict struct {
	ActivityID        ActivityID
	ExistingJournalID JournalID
}

func (e *ActivityConflict) Error() string {
	return fmt.Sprintf(
		"%v: ActivityID %q already belongs to journal row %d — "+
			"where: ActivityCreate fold; when: before operation commit; "+
			"impact: the operation was rolled back; "+
			"fix: retry the original OperationID or choose a new ActivityID",
		ErrActivityConflict, e.ActivityID.String(), e.ExistingJournalID)
}

func (e *ActivityConflict) Is(target error) bool { return target == ErrActivityConflict }
func (e *ActivityConflict) Unwrap() error        { return ErrActivityConflict }
