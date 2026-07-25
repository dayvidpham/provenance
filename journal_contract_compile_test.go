package provenance_test

import (
	"errors"
	"fmt"
	"testing"

	provenance "github.com/dayvidpham/provenance"
	"github.com/google/uuid"
)

// TestExternalAtomicJournalContractCompiles proves all public DTOs and typed errors
// introduced by this vertical are usable from an external package without any
// internal helpers. This is requirement 9 from the acceptance criteria.
func TestExternalAtomicJournalContractCompiles(t *testing.T) {
	task := provenance.TaskID{Namespace: "fixture", UUID: uuid.MustParse("018f0000-0000-7000-8000-000000000001")}
	actor := provenance.ActorID{Namespace: "fixture", UUID: uuid.MustParse("018f0000-0000-7000-8000-000000000002")}

	// Prove condition type and Canonicalize compile and run.
	condition := provenance.Condition{Kind: provenance.ConditionCurrentFact, Selector: provenance.FactSelector{Kind: provenance.FactEvidence, Filter: provenance.FactFilter{TaskScope: provenance.FactTaskScope{Kind: provenance.FactTaskUnscoped}}, EvidenceKind: "fixture.evidence"}}
	prepared, err := provenance.Canonicalize(provenance.OperationInput{Conditions: []provenance.Condition{condition}, Effects: []provenance.Effect{{Sort: provenance.EffectBootstrapAuthority, ResultSlot: "root", BootstrapLabel: "root"}}})
	if err != nil || len(prepared.NormalizedConditions()) != 1 {
		t.Fatalf("prepare complete mutation: %v", err)
	}

	// Prove all result-slot arm types compile (Activity arm reserved for later vertical).
	bindings := []provenance.ResultSlotBinding{
		{Slot: "task", ProducedJournalID: 1, Kind: provenance.JournalKindTaskEvent, TaskID: &task},
		{Slot: "authority", ProducedJournalID: 2, Kind: provenance.JournalKindAuthority},
		{Slot: "decision", ProducedJournalID: 3, Kind: provenance.JournalKindDecision},
		{Slot: "evidence", ProducedJournalID: 4, Kind: provenance.JournalKindEvidence},
	}
	for _, binding := range bindings {
		if err := provenance.ValidateResultSlotBinding(binding); err != nil {
			t.Fatalf("binding %+v: %v", binding, err)
		}
	}

	// Prove all three top-level typed errors support errors.Is and errors.As.
	activity := provenance.ActivityID{Namespace: "fixture", UUID: uuid.MustParse("018f0000-0000-7000-8000-000000000003")}
	conditionFailure := &provenance.ConditionFailure{Index: 0, Kind: provenance.ConditionCurrentFact, Reason: provenance.ConditionCurrentMismatch, ActualJournalID: 4}
	activityConflict := &provenance.ActivityConflict{ActivityID: activity, ExistingJournalID: 5}
	// OperationConflict: five broad axes, Index -1 for scalar (no per-field SemanticOperand).
	operationConflict := &provenance.OperationConflict{OperationID: "operation", Axis: provenance.ConflictEffect, Index: 0}
	for _, control := range []struct {
		err, sentinel error
		target        any
	}{
		{conditionFailure, provenance.ErrConditionFailed, new(*provenance.ConditionFailure)},
		{activityConflict, provenance.ErrActivityConflict, new(*provenance.ActivityConflict)},
		{operationConflict, provenance.ErrOperationConflict, new(*provenance.OperationConflict)},
	} {
		wrapped := fmt.Errorf("external boundary: %w", control.err)
		if !errors.Is(wrapped, control.sentinel) || !errors.As(wrapped, control.target) {
			t.Fatalf("typed error contract lost: %v", wrapped)
		}
	}

	// Prove fact query types compile and validate from an external package.
	query := provenance.DecisionQuery{
		Filter: provenance.FactFilter{
			TaskScope:         provenance.FactTaskScope{Kind: provenance.FactTaskExact, TaskID: task},
			EffectiveActorIDs: []provenance.ActorID{actor},
		},
		Kinds: []provenance.DecisionKind{"fixture.decision"},
		Page:  provenance.FactPageRequest{Limit: provenance.MaxFactPageSize},
	}
	if err := query.Validate(); err != nil {
		t.Fatalf("external query contract: %v", err)
	}
	tracker, err := provenance.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer tracker.Close()
	var _ provenance.Journal = tracker.Journal()
	if _, err := tracker.Journal().Facts().QueryDecisions(query); err != nil {
		t.Fatalf("Tracker.Journal().Facts().QueryDecisions: %v", err)
	}
	if _, err := tracker.Journal().Facts().QueryEvidence(provenance.EvidenceQuery{
		Kinds: []provenance.EvidenceKind{"fixture.evidence"},
		Page:  provenance.FactPageRequest{Limit: 1},
	}); err != nil {
		t.Fatalf("Tracker.Journal().Facts().QueryEvidence: %v", err)
	}

	// Prove ConflictAxes (five broad axes, no SemanticOperand taxonomy) is accessible.
	axes := provenance.ConflictAxes()
	if len(axes) != 5 {
		t.Fatalf("ConflictAxes = %v, want 5 nonzero axes (Actor/Authority/Command/Condition/Effect)", axes)
	}

	// All five axes must be nonzero.
	for _, ax := range axes {
		if ax == 0 {
			t.Fatalf("ConflictAxis zero found in ConflictAxes(); all axes must be nonzero")
		}
	}

	// ActivityCreate DTO: prove external construction of the complete contract
	// matching StartActivityWithID(id, agentID, phase, stage, notes).
	agentID := provenance.AgentID{Namespace: "fixture", UUID: uuid.MustParse("018f0000-0000-7000-8000-000000000005")}
	activityCreate := provenance.Effect{
		Sort:            provenance.EffectActivityCreate,
		ResultSlot:      "activity-slot",
		ActivityID:      activity,
		ActivityAgentID: agentID,
		ActivityPhase:   provenance.Phase(-1), // out-of-range Phase; IsValid() returns false
		ActivityStage:   provenance.Stage(-1), // out-of-range Stage; IsValid() returns false
		ActivityNotes:   "born",
	}
	_ = activityCreate // DTO compiles; normalization of zero Phase/Stage is tested separately
	activityBinding := provenance.ResultSlotBinding{
		Slot:              "activity",
		ProducedJournalID: 10,
		Kind:              provenance.JournalKindActivity,
		ActivityID:        &activity,
	}
	if err := provenance.ValidateResultSlotBinding(activityBinding); err != nil {
		t.Fatalf("activity slot binding rejected: %v", err)
	}

	// ConditionKind: nonzero constants and String() are reachable externally.
	if provenance.ConditionExactFact.String() == "" || provenance.ConditionCurrentFact.String() == "" {
		t.Fatal("ConditionKind.String() returned empty for named constants")
	}
	var zeroKind provenance.ConditionKind // zero is the invalid sentinel
	if zeroKind == provenance.ConditionExactFact || zeroKind == provenance.ConditionCurrentFact {
		t.Fatal("zero ConditionKind must not equal any named nonzero constant")
	}
}
