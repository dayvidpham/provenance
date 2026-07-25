package provenance

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
)

func TestDBOSWireRoundTripPreservesCompleteConditions(t *testing.T) {
	task := testTaskID(t)
	actor := testActorID(t)
	ctx, err := TaskContext(task)
	if err != nil {
		t.Fatal(err)
	}
	auth := journal.JournalID(17)
	in := journal.OperationInput{
		OperationID:        "conditions-round-trip",
		ActorID:            actor,
		AuthorityJournalID: &auth,
		CommandDigest:      []byte("command"),
		Conditions: []journal.Condition{{
			Kind: journal.ConditionExactFact,
			Selector: journal.FactSelector{
				Kind: journal.FactDecision,
				Filter: journal.FactFilter{
					TaskScope:         journal.FactTaskScope{Kind: journal.FactTaskExact, TaskID: task},
					RequiredContexts:  []journal.EventContext{ctx},
					EffectiveActorIDs: []journal.ActorID{actor},
					OperationIDs:      []journal.OperationID{"condition-source"},
				},
				DecisionKind: "fixture.decision",
			},
			AssertedJournalID: 17,
		}},
	}
	contract := newDBOSContractSnapshot()
	encoded, normalized, err := encodeApplyInput(contract, in)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeApplyInput(contract, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Conditions, normalized.Conditions) {
		t.Fatalf("decoded conditions=%#v want canonical=%#v", decoded.Conditions, normalized.Conditions)
	}
	if len(decoded.Conditions) != 1 || decoded.Conditions[0].Selector.Filter.TaskScope.Kind != journal.FactTaskExact {
		t.Fatalf("decoded condition lost selector shape: %#v", decoded.Conditions)
	}
}

func TestDBOSConditionFailureIsCheckpointedTypedAndPermanent(t *testing.T) {
	stack := newInternalDBOSStack(t, "dbos-condition-failure")
	callbacks := 0
	stack.adapter.testHooks.onWorkflowEntry = func() { callbacks++ }
	op := stack.operation("dbos-condition-failure")
	op.Conditions = []journal.Condition{{
		Kind: journal.ConditionExactFact,
		Selector: journal.FactSelector{
			Kind:         journal.FactDecision,
			Filter:       journal.FactFilter{TaskScope: journal.FactTaskScope{Kind: journal.FactTaskUnscoped}},
			DecisionKind: "fixture.missing",
		},
		AssertedJournalID: 1,
	}}
	_, err := stack.adapter.Apply(context.Background(), op)
	var first *journal.ConditionFailure
	if !errors.As(err, &first) || !errors.Is(err, journal.ErrConditionFailed) {
		t.Fatalf("first delivery error=%v, want typed ConditionFailure", err)
	}
	if looked, lookupErr := stack.tracker.Journal().LookupCommitted(op.OperationID); lookupErr != nil || looked.Kind != journal.CommittedAbsent {
		t.Fatalf("condition failure committed operation: result=%#v err=%v", looked, lookupErr)
	}
	attempts := callbacks
	_, err = stack.adapter.Apply(context.Background(), op)
	var recovered *journal.ConditionFailure
	if !errors.As(err, &recovered) || !errors.Is(err, journal.ErrConditionFailed) {
		t.Fatalf("recovered delivery error=%v, want typed ConditionFailure", err)
	}
	if !reflect.DeepEqual(first, recovered) || callbacks != attempts {
		t.Fatalf("condition failure drift/re-execution: first=%#v recovered=%#v callbacks=%d/%d", first, recovered, attempts, callbacks)
	}
}

func TestDBOSActivityConflictIsCheckpointedTypedAndActivityResultTransports(t *testing.T) {
	stack := newInternalDBOSStack(t, "dbos-activity-parity")
	callbacks := 0
	stack.adapter.testHooks.onWorkflowEntry = func() { callbacks++ }
	activity := newActivityID()
	auth := stack.authority
	seed := journal.OperationInput{
		OperationID:        "dbos-activity-seed",
		ActorID:            stack.actor,
		AuthorityJournalID: &auth,
		CommandDigest:      []byte("seed"),
		Effects: []journal.Effect{{
			Sort:            journal.EffectActivityCreate,
			ResultSlot:      "activity",
			ActivityID:      activity,
			ActivityAgentID: journal.AgentID(stack.actor),
			ActivityPhase:   PhaseWorkerSlices,
			ActivityStage:   StageInProgress,
		}},
	}
	if _, err := stack.tracker.Journal().Apply(seed); err != nil {
		t.Fatal(err)
	}
	op := seed
	op.OperationID = "dbos-activity-conflict"
	op.CommandDigest = []byte("conflict")
	_, err := stack.adapter.Apply(context.Background(), op)
	var first *journal.ActivityConflict
	if !errors.As(err, &first) || !errors.Is(err, journal.ErrActivityConflict) {
		t.Fatalf("activity conflict first delivery error=%v, want typed ActivityConflict", err)
	}
	if first.ActivityID != activity || first.ExistingJournalID <= 0 {
		t.Fatalf("activity conflict metadata=%#v", first)
	}
	attempts := callbacks
	_, err = stack.adapter.Apply(context.Background(), op)
	var recovered *journal.ActivityConflict
	if !errors.As(err, &recovered) || !errors.Is(err, journal.ErrActivityConflict) || !reflect.DeepEqual(first, recovered) {
		t.Fatalf("activity conflict recovered error=%v, want typed parity with %#v", err, first)
	}
	if callbacks != attempts {
		t.Fatalf("terminal activity conflict re-entered callback: callbacks=%d/%d", attempts, callbacks)
	}

	successOp := seed
	successOp.OperationID = "dbos-activity-success"
	successOp.CommandDigest = []byte("success")
	successOp.Effects[0].ActivityID = newActivityID()
	result, err := stack.adapter.Apply(context.Background(), successOp)
	if err != nil || len(result.ResultSlots) != 1 || result.ResultSlots[0].ActivityID == nil || *result.ResultSlots[0].ActivityID != successOp.Effects[0].ActivityID {
		t.Fatalf("activity result=%#v err=%v, want one ActivityID result slot", result, err)
	}
}
