package provenance_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	provenance "github.com/dayvidpham/provenance"
)

// Every top-level test in this file is parallel under the isolation proof
// documented above openGovernedTracker in governed_allocation_integration_test.go:
// each test owns a private t.TempDir database, or is a pure function test with no
// external state at all.

func TestGovernedAllocationSupplementOperationIDStableKnownValue(t *testing.T) {
	t.Parallel()

	const external provenance.OperationID = "known-external-operation"
	const want provenance.OperationID = "provenance.governed-supplement.v1.3c505d51d26e8eee56122aa6db3440031e8bb872ea647e6b2e9f7510101f35f3"

	first := provenance.GovernedAllocationSupplementOperationID(external)
	second := provenance.GovernedAllocationSupplementOperationID(external)
	if first != want || second != want {
		t.Fatalf("supplemental producer identity=(%q, %q), want stable known value %q", first, second, want)
	}
}

func TestComposedGovernedAllocationRejectsEmptyBeforeDBOS(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	participantCalls := 0
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "composed-empty-contract", func(context.Context, provenance.GovernedAllocationTransaction, provenance.GovernedAllocationRequest, provenance.OperationClosure) error {
		participantCalls++
		return nil
	})
	actor := registerGovernedActor(t, fused.Tracker(), "composed-empty-contract")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-empty-contract-root")
	request := composedGovernedRequest("composed-empty-contract", actor, root, 1)
	request.SupplementalEffects = nil
	before := snapshotGovernedTables(t, db)

	_, err := fused.RunAllocateComposed(ctx, "composed-empty-contract-workflow", root.AssignmentRow.JournalID, request)
	if err == nil {
		t.Fatal("empty composed request was accepted")
	}
	mustGovernedError(t, err, provenance.GovernedAllocationValidation)
	assertNoGovernedWrites(t, before, db)
	if participantCalls != 0 {
		t.Fatalf("empty request participant calls=%d, want 0", participantCalls)
	}
	if got := countFusedGovernedRows(t, db, `SELECT COUNT(*) FROM operation_outputs WHERE workflow_uuid=?1`, "composed-empty-contract-workflow"); got != 0 {
		t.Fatalf("empty request DBOS outputs=%d, want 0", got)
	}
}

func TestComposedGovernedAllocationReplayReceiptAndCopies(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	participantCalls := 0
	fused, db := openFusedAllocatorWithParticipantAndDatabase(t, "composed-replay-contract", func(_ context.Context, _ provenance.GovernedAllocationTransaction, request provenance.GovernedAllocationRequest, closure provenance.OperationClosure) error {
		participantCalls++
		request.Children[0].Title = "participant mutation"
		children := closure.Children()
		children[0].AssignmentID = "participant mutation"
		return nil
	})
	actor := registerGovernedActor(t, fused.Tracker(), "composed-replay-contract")
	if err := fused.Launch(); err != nil {
		t.Fatalf("launch fused allocator: %v", err)
	}
	root := initializeFusedRoot(t, fused, actor, "composed-replay-contract-root")
	request := composedGovernedRequest("composed-replay-contract", actor, root, 1)
	original := composedGovernedRequest("composed-replay-contract", actor, root, 1)

	first, err := fused.RunAllocateComposed(ctx, "composed-replay-contract-workflow", root.AssignmentRow.JournalID, request)
	if err != nil {
		t.Fatalf("first composed allocation: %v", err)
	}
	if participantCalls != 1 {
		t.Fatalf("first participant calls=%d, want 1", participantCalls)
	}

	slots := first.SupplementalResultSlots()
	if len(slots) != 4 {
		t.Fatalf("result slots=%d, want 4", len(slots))
	}
	wantSlots := []struct {
		slot     string
		kind     provenance.JournalKind
		task     bool
		activity bool
	}{
		{"activity", provenance.JournalKindActivity, false, true},
		{"edge", provenance.JournalKindTaskEvent, true, false},
		{"evidence", provenance.JournalKindEvidence, false, false},
		{"slice-event", provenance.JournalKindTaskEvent, true, false},
	}
	for i, want := range wantSlots {
		got := slots[i]
		if string(got.Slot) != want.slot || got.Kind != want.kind || got.ProducedJournalID <= 0 || (got.TaskID != nil) != want.task || (got.ActivityID != nil) != want.activity {
			t.Fatalf("slot[%d]=%+v, want slot=%q kind=%v task=%v activity=%v and positive JournalID", i, got, want.slot, want.kind, want.task, want.activity)
		}
	}
	assertComposedSupplementalSlotsPersisted(t, db, original, slots)
	if *slots[0].ActivityID != original.SupplementalEffects[3].ActivityID || *slots[1].TaskID != root.TaskID || *slots[3].TaskID != original.Allocation.Children[0].TaskID {
		t.Fatalf("result slot identities do not match request: %+v", slots)
	}
	wantEvents := []provenance.JournalID{slots[1].ProducedJournalID, slots[3].ProducedJournalID}
	if got := first.SupplementalEmittedEvents(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("emitted events=%v, want exact task-event IDs %v", got, wantEvents)
	}

	request.Allocation.Children[0].Title = "caller mutation"
	request.SupplementalEffects[0].Payload[0] = '!'
	slots[0].Slot = "caller mutation"
	slots[0].ActivityID.Namespace = "caller mutation"
	events := first.SupplementalEmittedEvents()
	events[0] = 0
	if got := first.SupplementalResultSlots(); string(got[0].Slot) != "activity" || got[0].ActivityID.Namespace == "caller mutation" {
		t.Fatalf("returned slot mutation escaped defensive copy: %+v", got[0])
	}
	if got := first.SupplementalEmittedEvents(); got[0] == 0 {
		t.Fatal("returned event mutation escaped defensive copy")
	}
	var persistedTitle, persistedPayload string
	if err := db.QueryRow(`SELECT title FROM tasks WHERE id=?1`, original.Allocation.Children[0].TaskID.String()).Scan(&persistedTitle); err != nil {
		t.Fatalf("read persisted child title: %v", err)
	}
	if err := db.QueryRow(`SELECT payload FROM journal_evidence WHERE task_id=?1`, original.Allocation.Children[0].TaskID.String()).Scan(&persistedPayload); err != nil {
		t.Fatalf("read persisted evidence payload: %v", err)
	}
	if persistedTitle != original.Allocation.Children[0].Title || persistedPayload != string(original.SupplementalEffects[0].Payload) {
		t.Fatalf("caller/participant mutation changed persisted request: title=%q payload=%q", persistedTitle, persistedPayload)
	}

	replayed, err := fused.RunAllocateComposed(ctx, "composed-replay-contract-distinct-workflow", root.AssignmentRow.JournalID, original)
	if err != nil {
		t.Fatalf("distinct-workflow exact replay: %v", err)
	}
	if participantCalls != 1 {
		t.Fatalf("distinct-workflow replay participant calls=%d, want 1", participantCalls)
	}
	assertSameClosure(t, first.Closure(), replayed.Closure())
	if !reflect.DeepEqual(first.SupplementalResultSlots(), replayed.SupplementalResultSlots()) || !reflect.DeepEqual(first.SupplementalEmittedEvents(), replayed.SupplementalEmittedEvents()) {
		t.Fatalf("distinct-workflow replay changed receipt: first=%+v replay=%+v", first, replayed)
	}

	changedOrder := composedGovernedRequest("composed-replay-contract", actor, root, 1)
	changedOrder.SupplementalEffects[0], changedOrder.SupplementalEffects[1] = changedOrder.SupplementalEffects[1], changedOrder.SupplementalEffects[0]
	before := snapshotGovernedTables(t, db)
	outputsBefore := composedOutputCounts(t, db, "composed-replay-contract-workflow")
	_, err = fused.RunAllocateComposed(ctx, "composed-replay-contract-workflow", root.AssignmentRow.JournalID, changedOrder)
	if err == nil {
		t.Fatal("same-workflow changed supplemental order was accepted")
	}
	mustGovernedError(t, err, provenance.GovernedAllocationConflict)
	assertNoGovernedWrites(t, before, db)
	if participantCalls != 1 {
		t.Fatalf("changed-order conflict participant calls=%d, want 1", participantCalls)
	}
	if outputsAfter := composedOutputCounts(t, db, "composed-replay-contract-workflow"); outputsAfter != outputsBefore {
		t.Fatalf("changed-order conflict changed operation_outputs=%+v, want unchanged %+v (no success or error checkpoint)", outputsAfter, outputsBefore)
	}
}

func composedOutputCounts(t *testing.T, db *sql.DB, workflowID string) (counts struct{ total, success, failure int }) {
	t.Helper()
	err := db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE error IS NULL), COUNT(*) FILTER (WHERE error IS NOT NULL) FROM operation_outputs WHERE workflow_uuid=?1`, workflowID).Scan(&counts.total, &counts.success, &counts.failure)
	if err != nil {
		t.Fatalf("count operation_outputs for workflow %q: %v", workflowID, err)
	}
	return counts
}

func assertComposedSupplementalSlotsPersisted(t *testing.T, db *sql.DB, request provenance.GovernedAllocationComposedRequest, slots []provenance.ResultSlotBinding) {
	t.Helper()
	// Query the actual rows using the public correlation identity. A match proves
	// the helper is the producer identity used by the composed execution path.
	operationID := provenance.GovernedAllocationSupplementOperationID(request.Allocation.OperationID)
	for _, slot := range slots {
		var kind string
		var taskID, evidenceTaskID, activityID sql.NullString
		err := db.QueryRow(`
			SELECT journal_kinds.name, journal_task_events.task_id,
			       journal_evidence.task_id, journal_activity_creations.activity_id
			FROM journal_operation_result_slots
			JOIN journal_operations ON journal_operations.journal_id=journal_operation_result_slots.journal_id
			JOIN journal ON journal.journal_id=journal_operation_result_slots.produced_journal_id
			JOIN journal_kinds ON journal_kinds.id=journal.kind_id
			LEFT JOIN journal_task_events ON journal_task_events.journal_id=journal_operation_result_slots.produced_journal_id
			LEFT JOIN journal_evidence ON journal_evidence.journal_id=journal_operation_result_slots.produced_journal_id
			LEFT JOIN journal_activity_creations ON journal_activity_creations.journal_id=journal_operation_result_slots.produced_journal_id
			WHERE journal_operations.operation_id=?1 AND journal_operation_result_slots.result_slot_id=?2
			  AND journal_operation_result_slots.produced_journal_id=?3`, operationID, slot.Slot, slot.ProducedJournalID).Scan(&kind, &taskID, &evidenceTaskID, &activityID)
		if err != nil {
			t.Fatalf("bind supplemental slot %q journal %d to persisted row: %v", slot.Slot, slot.ProducedJournalID, err)
		}
		if kind != slot.Kind.String() {
			t.Fatalf("slot %q persisted kind=%q, want receipt kind=%q", slot.Slot, kind, slot.Kind)
		}
		switch string(slot.Slot) {
		case "activity":
			if slot.ActivityID == nil || !activityID.Valid || activityID.String != slot.ActivityID.String() {
				t.Fatalf("activity slot persisted identity=%q, want receipt identity=%v", activityID.String, slot.ActivityID)
			}
		case "edge", "slice-event":
			if slot.TaskID == nil || !taskID.Valid || taskID.String != slot.TaskID.String() {
				t.Fatalf("task-event slot %q persisted task=%q, want receipt task=%v", slot.Slot, taskID.String, slot.TaskID)
			}
		case "evidence":
			wantTask := request.Allocation.Children[0].TaskID.String()
			if !evidenceTaskID.Valid || evidenceTaskID.String != wantTask {
				t.Fatalf("evidence slot persisted task=%q, want request child %q", evidenceTaskID.String, wantTask)
			}
		default:
			t.Fatalf("unexpected supplemental slot %q", slot.Slot)
		}
	}
}
