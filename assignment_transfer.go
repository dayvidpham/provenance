package provenance

import (
	"fmt"
	"time"
)

// AssignmentTransferRequest identifies one assignment episode to consume and
// its exact successor. The governing authority and operation identity remain
// bound to the Session and Apply options, respectively.
type AssignmentTransferRequest struct {
	TaskID               TaskID
	SlotID               AssignmentSlotID
	PreviousAssignmentID AssignmentID
	NextAssignmentID     AssignmentID
	NextOccupant         ActorID
}

// AssignmentTransferResult is the semantic result of TransferAssignment. It
// intentionally omits journal/storage identities; Replayed reports whether an
// exact pinned retry returned the already-committed transfer.
type AssignmentTransferResult struct {
	TaskID               TaskID
	SlotID               AssignmentSlotID
	PreviousAssignmentID AssignmentID
	NextAssignmentID     AssignmentID
	NextOccupant         ActorID
	Replayed             bool
}

// TransferAssignment atomically consumes this Session's exact active
// predecessor assignment authority, ends that predecessor, and starts one exact
// successor on the same task and slot. Generic Session.Atomic and Journal.Apply
// retain their ordinary per-effect authorization behavior.
func (s *Session) TransferAssignment(request AssignmentTransferRequest, opts ...ApplyOption) (AssignmentTransferResult, error) {
	if err := s.checkGate("TransferAssignment"); err != nil {
		return AssignmentTransferResult{}, err
	}
	if err := s.requireInitialized("TransferAssignment"); err != nil {
		return AssignmentTransferResult{}, err
	}

	cfg := s.resolveAssignmentTransfer(request, opts)
	committed, err := s.tr.db.ApplyAssignmentTransfer(s.assignmentTransferOperationInput(request, cfg))
	if err != nil {
		return AssignmentTransferResult{}, fmt.Errorf("provenance.Session.TransferAssignment: %w", err)
	}
	return AssignmentTransferResult{
		TaskID:               request.TaskID,
		SlotID:               request.SlotID,
		PreviousAssignmentID: request.PreviousAssignmentID,
		NextAssignmentID:     request.NextAssignmentID,
		NextOccupant:         request.NextOccupant,
		Replayed:             committed.ShortCircuited,
	}, nil
}

// resolveAssignmentTransfer is shared by the direct and DBOS entry points so
// both bind the same semantic request to the operation identity.
func (s *Session) resolveAssignmentTransfer(request AssignmentTransferRequest, opts []ApplyOption) applyConfig {
	return s.resolve(opts,
		"assignment-transfer",
		request.TaskID.String(),
		string(request.SlotID),
		string(request.PreviousAssignmentID),
		string(request.NextAssignmentID),
		request.NextOccupant.String(),
	)
}

func assignmentTransferEffects(request AssignmentTransferRequest) []Effect {
	return []Effect{
		{
			Sort:         EffectAssignmentEnd,
			AssignmentID: request.PreviousAssignmentID,
			TaskID:       request.TaskID,
			SlotID:       request.SlotID,
		},
		{
			Sort:         EffectAssignmentStart,
			AssignmentID: request.NextAssignmentID,
			TaskID:       request.TaskID,
			SlotID:       request.SlotID,
			Occupant:     request.NextOccupant,
			Predecessor:  request.PreviousAssignmentID,
		},
	}
}

func (s *Session) assignmentTransferOperationInput(request AssignmentTransferRequest, cfg applyConfig) OperationInput {
	authority := s.authority
	return OperationInput{
		OperationID:        cfg.opID,
		ActorID:            s.actor,
		AuthorityJournalID: &authority,
		CommandDigest:      cfg.commandDigest,
		MutationDigest:     cfg.mutationDigest,
		RecordedAt:         time.Now().UTC().UnixNano(),
		Effects:            assignmentTransferEffects(request),
	}
}
