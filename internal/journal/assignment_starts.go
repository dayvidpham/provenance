package journal

import (
	"fmt"
	"time"
)

// MaxAssignmentStartSlotFilters is the number of registered assignment slots.
const MaxAssignmentStartSlotFilters = 1

// AssignmentStartPageRequest bounds consumed authority candidates, not matching
// rows. Limit must be 1..MaxFactPageSize. A fresh request leaves SnapshotPinned
// false and SnapshotMaxJournalID zero. AfterJournalID may retain a completed
// high-water mark. A pinned zero snapshot stays empty, even after new writes.
type AssignmentStartPageRequest struct {
	Limit                int
	SnapshotMaxJournalID JournalID
	SnapshotPinned       bool
	AfterJournalID       JournalID
}

// AssignmentStartQuery uses OR within a dimension and AND across dimensions.
// Empty filters match all starts. Filters are validated before SQL and applied
// only after candidate integrity checks. Replay filters on continuation: the
// stateless cursor does not bind or authenticate them.
type AssignmentStartQuery struct {
	Page          AssignmentStartPageRequest
	TaskIDs       []TaskID
	AssignmentIDs []AssignmentID
	ActorIDs      []ActorID
	OperationIDs  []OperationID
	SlotIDs       []AssignmentSlotID
}

// AssignmentStartRow identifies the exact start authority, not a material event,
// governing ancestor, or operation anchor. SlotID is not a consumer's role.
type AssignmentStartRow struct {
	AuthorityJournalID          JournalID
	RecordedAt                  time.Time
	AssignmentID                AssignmentID
	TaskID                      TaskID
	SlotID                      AssignmentSlotID
	Occupant                    ActorID
	ParentAssignmentID          *AssignmentID
	PredecessorAssignmentID     *AssignmentID
	ProducingOperationID        OperationID
	ProducingOperationJournalID JournalID
}

// AssignmentStartPage always returns a pinned snapshot. Empty Rows with Next
// nonnil is progress, not exhaustion: valid bootstrap/end and filtered-out
// candidates consume capacity. Next excludes the validated lookahead candidate.
type AssignmentStartPage struct {
	Rows                 []AssignmentStartRow
	SnapshotMaxJournalID JournalID
	SnapshotPinned       bool
	Next                 *AssignmentStartCursor
}

// AssignmentStartCursor resumes after the last consumed candidate.
type AssignmentStartCursor struct {
	SnapshotMaxJournalID JournalID
	SnapshotPinned       bool
	AfterJournalID       JournalID
}

// AssignmentStartQueryAPI is an optional read capability on SQLite journals,
// including borrowed journals. It does not extend the required Journal contract.
// It validates at most Limit+1 top-level candidates, plus one bounded prior-start
// diagnostic per end. This is not a bound on all SQLite rows visited. Episode-only
// orphans have no JournalID anchor and require Journal.VerifyIntegrity, not this
// page API. Exhaustion alone is not a whole-store integrity certificate.
type AssignmentStartQueryAPI interface {
	QueryAssignmentStarts(AssignmentStartQuery) (AssignmentStartPage, error)
}

func assignmentStartInputError(problem, fix string) error {
	return fmt.Errorf("%w: %s — where: QueryAssignmentStarts input; when: before page SQL; impact: no page returned; fix: %s", ErrInvalidQuery, problem, fix)
}

func (p AssignmentStartPageRequest) Validate() error {
	if p.Limit < 1 || p.Limit > MaxFactPageSize {
		return assignmentStartInputError("limit outside allowed range", fmt.Sprintf("use Limit 1..%d", MaxFactPageSize))
	}
	if p.SnapshotMaxJournalID < 0 || p.AfterJournalID < 0 || (!p.SnapshotPinned && p.SnapshotMaxJournalID != 0) || (p.SnapshotPinned && p.AfterJournalID > p.SnapshotMaxJournalID) {
		return assignmentStartInputError("inconsistent snapshot/cursor", "use nonnegative journal IDs; leave a fresh snapshot zero and unpinned, or pin the returned boundary with cursor no higher than it")
	}
	return nil
}

// Validate checks request shape without accessing the store. Boundary existence
// and a fresh cursor's relation to MAX are checked in the page read transaction.
func (q AssignmentStartQuery) Validate() error {
	if err := q.Page.Validate(); err != nil {
		return err
	}
	if len(q.TaskIDs) > MaxFactFilterValues || len(q.AssignmentIDs) > MaxFactFilterValues || len(q.ActorIDs) > MaxFactFilterValues || len(q.OperationIDs) > MaxFactFilterValues || len(q.SlotIDs) > MaxAssignmentStartSlotFilters {
		return assignmentStartInputError("filter collection exceeds its bound", fmt.Sprintf("use at most %d task/assignment/actor/operation values and %d slot values", MaxFactFilterValues, MaxAssignmentStartSlotFilters))
	}
	for _, id := range q.TaskIDs {
		if err := validateTaskID(id); err != nil {
			return assignmentStartInputError("malformed task ID: "+err.Error(), "supply canonical TaskIDs")
		}
	}
	for _, id := range q.ActorIDs {
		if err := validateActorID(id); err != nil {
			return assignmentStartInputError("malformed actor ID: "+err.Error(), "supply canonical ActorIDs")
		}
	}
	for _, id := range q.AssignmentIDs {
		if err := ValidateOperationID(OperationID(id)); err != nil {
			return assignmentStartInputError("malformed assignment ID: "+err.Error(), "supply nonempty assignment IDs without control characters")
		}
	}
	for _, id := range q.OperationIDs {
		if err := ValidateOperationID(id); err != nil {
			return assignmentStartInputError("malformed operation ID: "+err.Error(), "supply nonempty operation IDs without control characters")
		}
	}
	for _, id := range q.SlotIDs {
		if id != SlotOwnerResponsibility {
			return assignmentStartInputError("unknown assignment slot", "use SlotOwnerResponsibility")
		}
	}
	return nil
}
