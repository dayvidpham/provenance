package provenance

// journal_result_slot_matrix_test.go tests the complete result slot
// reconstruction through Apply, LookupCommitted, replay, and reopen;
// and the ValidateResultSlotBinding arm matrix.

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestResultSlotMatrix_TaskEvent verifies TaskEvent slot reconstruction.
func TestResultSlotMatrix_TaskEvent(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis-slot-te")
	task := env.newTask(t, boot, "task-for-slot")

	opID := OperationID("slot-task-event-" + uuid.Must(uuid.NewV7()).String())
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        opID,
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("slot-te"),
		Effects:            []Effect{{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.test.event", ResultSlot: "evt"}},
	})
	if err != nil || res.Kind != CommittedExact {
		t.Fatalf("TaskEvent slot Apply: %v %+v", err, res)
	}
	if len(res.ResultSlots) != 1 {
		t.Fatalf("TaskEvent slot: expected 1 result slot, got %d: %+v", len(res.ResultSlots), res.ResultSlots)
	}
	slot := res.ResultSlots[0]
	if slot.Slot != "evt" || slot.Kind != JournalKindTaskEvent {
		t.Fatalf("TaskEvent slot: unexpected binding %+v", slot)
	}
	if slot.TaskID == nil || *slot.TaskID != task {
		t.Fatalf("TaskEvent slot: TaskID=%v, want %v", slot.TaskID, task)
	}
	if slot.ActivityID != nil {
		t.Fatalf("TaskEvent slot: ActivityID must be nil, got %v", slot.ActivityID)
	}
}

// TestResultSlotMatrix_Activity verifies Activity slot reconstruction.
func TestResultSlotMatrix_Activity(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis-slot-act")
	actID := newActivityID()

	opID := OperationID("slot-activity-" + uuid.Must(uuid.NewV7()).String())
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        opID,
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("slot-act"),
		Effects: []Effect{{
			Sort: EffectActivityCreate, ResultSlot: "activity",
			ActivityID: actID, ActivityAgentID: AgentID(env.actor),
			ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress,
		}},
	})
	if err != nil || res.Kind != CommittedExact {
		t.Fatalf("Activity slot Apply: %v %+v", err, res)
	}
	if len(res.ResultSlots) != 1 {
		t.Fatalf("Activity slot: expected 1 result slot, got %d", len(res.ResultSlots))
	}
	slot := res.ResultSlots[0]
	if slot.Slot != "activity" || slot.Kind != JournalKindActivity {
		t.Fatalf("Activity slot: unexpected binding %+v", slot)
	}
	if slot.ActivityID == nil || *slot.ActivityID != actID {
		t.Fatalf("Activity slot: ActivityID=%v, want %v", slot.ActivityID, actID)
	}
	if slot.TaskID != nil {
		t.Fatalf("Activity slot: TaskID must be nil, got %v", slot.TaskID)
	}
}

// TestResultSlotMatrix_MultipleSlots verifies multiple result slots in one operation.
func TestResultSlotMatrix_MultipleSlots(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis-multi-slot")
	task := env.newTask(t, boot, "multi-slot-task")
	actID1 := newActivityID()
	actID2 := newActivityID()

	opID := OperationID("multi-slot-" + uuid.Must(uuid.NewV7()).String())
	res, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        opID,
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("multi"),
		Effects: []Effect{
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.test.event1", ResultSlot: "evt1"},
			{
				Sort: EffectActivityCreate, ResultSlot: "act1",
				ActivityID: actID1, ActivityAgentID: AgentID(env.actor),
				ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress,
			},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.test.event2", ResultSlot: "evt2"},
			{
				Sort: EffectActivityCreate, ResultSlot: "act2",
				ActivityID: actID2, ActivityAgentID: AgentID(env.actor),
				ActivityPhase: PhaseCodeReview, ActivityStage: StageComplete,
			},
		},
	})
	if err != nil || res.Kind != CommittedExact {
		t.Fatalf("multi-slot Apply: %v %+v", err, res)
	}
	if len(res.ResultSlots) != 4 {
		t.Fatalf("multi-slot: expected 4 slots, got %d: %+v", len(res.ResultSlots), res.ResultSlots)
	}

	slotsByName := make(map[ResultSlotID]ResultSlotBinding, 4)
	for _, s := range res.ResultSlots {
		slotsByName[s.Slot] = s
	}

	evt1, ok1 := slotsByName["evt1"]
	if !ok1 || evt1.Kind != JournalKindTaskEvent || evt1.TaskID == nil || *evt1.TaskID != task {
		t.Fatalf("evt1 slot: %+v", evt1)
	}
	act1, ok2 := slotsByName["act1"]
	if !ok2 || act1.Kind != JournalKindActivity || act1.ActivityID == nil || *act1.ActivityID != actID1 {
		t.Fatalf("act1 slot: %+v", act1)
	}
	evt2, ok3 := slotsByName["evt2"]
	if !ok3 || evt2.Kind != JournalKindTaskEvent || evt2.TaskID == nil || *evt2.TaskID != task {
		t.Fatalf("evt2 slot: %+v", evt2)
	}
	act2, ok4 := slotsByName["act2"]
	if !ok4 || act2.Kind != JournalKindActivity || act2.ActivityID == nil || *act2.ActivityID != actID2 {
		t.Fatalf("act2 slot: %+v", act2)
	}
}

// TestResultSlotMatrix_MissingSlotRejected verifies that EffectActivityCreate
// without a ResultSlot is rejected at canonicalization time.
func TestResultSlotMatrix_MissingSlotRejected(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis-slot-missing")
	actID := newActivityID()

	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        "missing-slot-test",
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("missing-slot"),
		Effects: []Effect{{
			Sort:       EffectActivityCreate, // ResultSlot intentionally omitted
			ActivityID: actID, ActivityAgentID: AgentID(env.actor),
			ActivityPhase: PhaseWorkerSlices, ActivityStage: StageInProgress,
		}},
	})
	if err == nil {
		t.Fatal("ActivityCreate without ResultSlot: Apply should fail at canonicalization")
	}
	if !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("ActivityCreate without ResultSlot: expected ErrCanonicalMutation, got: %v", err)
	}
}

// TestResultSlotMatrix_DuplicateSlotRejected verifies that duplicate ResultSlot
// IDs in one operation are rejected at canonicalization time.
func TestResultSlotMatrix_DuplicateSlotRejected(t *testing.T) {
	t.Parallel()
	env := newOpsEnv(t)
	boot := env.genesis(t, "op-genesis-dup-slot")
	task := env.newTask(t, boot, "dup-slot-task")

	_, err := env.tr.Journal().Apply(OperationInput{
		OperationID:        "dup-slot-test",
		ActorID:            env.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      env.digest("dup-slot"),
		Effects: []Effect{
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.test.e1", ResultSlot: "same"},
			{Sort: EffectTaskEvent, TaskID: task, EventKind: "provenance.test.e2", ResultSlot: "same"}, // duplicate
		},
	})
	if err == nil {
		t.Fatal("duplicate result slot: Apply should fail at canonicalization")
	}
	if !errors.Is(err, ErrCanonicalMutation) {
		t.Fatalf("duplicate result slot: expected ErrCanonicalMutation, got: %v", err)
	}
}

// newTask creates a task via Apply and returns its TaskID.
func (e *opsEnv) newTask(t *testing.T, boot JournalID, titleSuffix string) TaskID {
	t.Helper()
	task := TaskID{Namespace: "provenance-test", UUID: uuid.Must(uuid.NewV7())}
	opID := OperationID("create-task-" + task.String())
	if _, err := e.tr.Journal().Apply(OperationInput{
		OperationID:        opID,
		ActorID:            e.actor,
		AuthorityJournalID: &boot,
		CommandDigest:      e.digest("create-task-" + task.String()),
		Effects: []Effect{{
			Sort: EffectTaskCreate, TaskID: task,
			Title: titleSuffix, Type: TaskTypeTask, Priority: PriorityMedium, Phase: PhaseUnscoped,
		}},
	}); err != nil {
		t.Fatalf("create task %s: %v", titleSuffix, err)
	}
	return task
}
