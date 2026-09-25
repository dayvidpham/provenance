package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ActorOwnershipQueryAPI is an optional read capability implemented by SQLite
// and borrowed SQLite journals. Journal, ContextJournal, and Tracker do not
// include this method; callers type-assert the value returned by Journal.
type ActorOwnershipQueryAPI interface {
	QueryActorOwnership(context.Context, ActorOwnershipQuery) (ActorOwnershipSnapshot, error)
}

// ActorOwnershipQuery names one actor and the caller-defined event and evidence
// kinds to return. Empty kind lists are valid; empty entries are not.
type ActorOwnershipQuery struct {
	Actor         ActorID
	MaterialKinds []EventKind
	EvidenceKinds []EvidenceKind
}

// Validate checks the query without accessing a store or mutating caller-owned
// slices.
func (q ActorOwnershipQuery) Validate() error {
	if err := validateActorID(q.Actor); err != nil {
		return actorOwnershipInputError("actor ID is malformed: "+err.Error(), "use a canonical actor ID with a namespace and non-zero UUID")
	}
	for _, kind := range q.MaterialKinds {
		if err := ValidateEventKind(kind); err != nil {
			return actorOwnershipInputError("material kind is malformed: "+err.Error(), "use canonical non-empty namespaced material kinds")
		}
	}
	for _, kind := range q.EvidenceKinds {
		if err := ValidateEventKind(EventKind(kind)); err != nil {
			return actorOwnershipInputError("evidence kind is malformed: "+err.Error(), "use canonical non-empty namespaced evidence kinds")
		}
	}
	return nil
}

func actorOwnershipInputError(problem, fix string) error {
	return fmt.Errorf("%w: QueryActorOwnership input is invalid: %s. Why: the actor or kind list violates the read contract; where: QueryActorOwnership input; when: before leasing a store connection; impact: no read started; fix: %s", ErrInvalidQuery, problem, fix)
}

// OwnedTaskRow is one task owned by the queried actor in the read snapshot.
// The transfer predecessor is returned but never followed.
type OwnedTaskRow struct {
	TaskID                  TaskID
	Phase                   Phase
	AssignmentID            AssignmentID
	StartedJournalID        JournalID
	ProducingOperationID    OperationID
	PredecessorAssignmentID *AssignmentID
	Materials               []OwnedMaterialRow
	Evidence                []OwnedEvidenceRow
}

// OwnedMaterialRow is a selected task event produced by the owning operation.
type OwnedMaterialRow struct {
	JournalID JournalID
	EventKind EventKind
	Payload   json.RawMessage
}

// OwnedEvidenceRow is selected evidence produced by the owning operation.
type OwnedEvidenceRow struct {
	JournalID    JournalID
	EvidenceKind EvidenceKind
	Payload      []byte
}

// ActorOwnershipSnapshot is the complete result of one read transaction.
type ActorOwnershipSnapshot struct {
	Through    JournalID
	ActorKnown bool
	Tasks      []OwnedTaskRow
	Work       ActorOwnershipWork
}

// ActorOwnershipWork counts rows copied into a completed result and the bytes
// of every variable-width value copied across all three result stages.
type ActorOwnershipWork struct {
	TaskRows, MaterialRows, EvidenceRows int
	ResultBytes                          int64
}

// ActorOwnershipStage identifies the result stage whose first over-limit row
// stopped QueryActorOwnership.
type ActorOwnershipStage string

const (
	ActorOwnershipStageOwnedTasks ActorOwnershipStage = "owned-tasks"
	ActorOwnershipStageMaterials  ActorOwnershipStage = "materials"
	ActorOwnershipStageEvidence   ActorOwnershipStage = "evidence"
)

// MaxActorOwnershipResultBytes is the only bound on QueryActorOwnership output.
// Row counts are not bounded.
const MaxActorOwnershipResultBytes = 8 << 20

// ErrActorOwnershipLimit marks an integrity fault caused by result data that a
// supported writer cannot produce; it is not a capacity or pagination signal.
var ErrActorOwnershipLimit = errors.New("provenance: actor ownership read exceeded its result byte bound")

// ActorOwnershipLimitError reports the first row whose running byte total is
// above MaxActorOwnershipResultBytes. Work counts only rows copied before it.
type ActorOwnershipLimitError struct {
	Actor         ActorID
	Stage         ActorOwnershipStage
	LimitBytes    int64
	ObservedBytes int64
	Work          ActorOwnershipWork
}

func (e *ActorOwnershipLimitError) Error() string {
	return fmt.Sprintf("provenance: the ownership read for actor %s stopped at stage %s after %d bytes, above its bound of %d bytes. Why: a correct result is a few hundred bytes per owned task, so the store holds a value that no supported writer produces. Where: QueryActorOwnership, stage %s. When: inside the read transaction, before the row was added to the result. Impact: no result was returned; nothing was written. Fix: run Journal.VerifyIntegrity and Journal.ReplayProjections on this store to find the damaged rows; both only read; then restore the store from a known-good copy.", e.Actor, e.Stage, e.ObservedBytes, e.LimitBytes, e.Stage)
}

func (e *ActorOwnershipLimitError) Unwrap() error { return ErrActorOwnershipLimit }

// OwnerProjectionMismatchError reports disagreement between tasks.owner_id and
// the newest active owner-responsibility episode selected by the writer.
type OwnerProjectionMismatchError struct {
	Task           TaskID
	Owner          ActorID
	ActiveOccupant *ActorID
}

func (e *OwnerProjectionMismatchError) Error() string {
	occupant := "nobody"
	if e.ActiveOccupant != nil {
		occupant = e.ActiveOccupant.String()
	}
	return fmt.Sprintf("provenance: tasks.owner_id for task %s names actor %s, but the newest active owner-slot episode is held by %s. Why: the owner column is written in the same transaction as the episode rows, so they must agree. Where: QueryActorOwnership, stage owned-tasks. When: inside the read transaction. Impact: no result was returned; nothing was written. Fix: run Journal.ReplayProjections and Journal.VerifyIntegrity to confirm the diverging field (both only read); then restore the store from a known-good copy.", e.Task, e.Owner, occupant)
}

// Is makes the typed mismatch discoverable with errors.Is against the general
// projection-divergence sentinel.
func (e *OwnerProjectionMismatchError) Is(target error) bool {
	return target == ErrProjectionDivergence
}
